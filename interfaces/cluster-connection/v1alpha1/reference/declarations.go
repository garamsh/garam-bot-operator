package reference

import (
	"cmp"
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/garamsh/garam-bot/api/declaration/v1alpha1/manifest"
)

const (
	// maxWait is DECL-3's bound on wait, and its default.
	maxWait = 50
	// maxDeclarationID is openapi.yaml's bound on a change's id.
	maxDeclarationID = 128
	opApply          = "apply"
	opDelete         = "delete"
)

// declaredKinds are DECL-7's kinds, interface 1's at v1alpha1.
var declaredKinds = []string{"Bot", "Workspace", "WorkspaceClass", "BotPool"}

// declaration is one declaration placed on a record. A tombstoned one stays, so its identifier is
// never placed again and a report naming it is still the record's.
type declaration struct {
	id         string
	cluster    GRN
	generation int64
	object     json.RawMessage
	// lastSeq is the position of the declaration's latest change in its record's log.
	lastSeq    int64
	tombstoned bool
}

// change is one entry of a record's log.
type change struct {
	seq    int64
	change Change
}

// Change is Change of openapi.yaml.
type Change struct {
	Op         string          `json:"op"`
	ID         string          `json:"id"`
	Generation int64           `json:"generation"`
	Object     json.RawMessage `json:"object,omitempty"`
}

// DeclarationPage is DeclarationPage of openapi.yaml.
type DeclarationPage struct {
	Cursor  string   `json:"cursor"`
	Full    bool     `json:"full,omitempty"`
	More    bool     `json:"more,omitempty"`
	Changes []Change `json:"changes"`
}

// declaredObject is DeclaredObject of openapi.yaml: what DECL-7 lets a declaration carry, and no more.
type declaredObject struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Namespace   string            `json:"namespace"`
		Name        string            `json:"name"`
		Labels      map[string]string `json:"labels,omitempty"`
		Annotations map[string]string `json:"annotations,omitempty"`
	} `json:"metadata"`
	Spec map[string]any `json:"spec"`
}

func (s *Server) watchDeclarations(w http.ResponseWriter, r *http.Request) error {
	if err := s.authenticateFirst(r); err != nil {
		return err
	}
	query := r.URL.Query()
	wait := maxWait
	if raw := query.Get("wait"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 0 || parsed > maxWait {
			return refuse(http.StatusBadRequest, codeInvalidRequest, "wait is not an integer from 0 to 50")
		}
		wait = parsed
	}
	if query.Has("after") && query.Get("after") == "" {
		return refuse(http.StatusBadRequest, codeInvalidRequest, "after is empty")
	}
	held := time.NewTimer(time.Duration(wait) * time.Second)
	defer held.Stop()
	expired := false
	for {
		s.mu.Lock()
		c, err := s.authenticate(r)
		if err != nil {
			s.mu.Unlock()
			return err
		}
		page, err := s.page(c, query.Get("after"))
		if err != nil {
			s.mu.Unlock()
			return err
		}
		wake := c.wake
		s.mu.Unlock()
		if page.Full || len(page.Changes) > 0 || expired {
			writeJSON(w, http.StatusOK, page)
			return nil
		}
		select {
		case <-wake:
		case <-held.C:
			expired = true
		case <-r.Context().Done():
			return nil
		}
	}
}

// cursor is a parsed position in a record's log. A full-set cursor also names the position its full
// set began at, which its last page answers as the cursor to go on from.
type cursor struct {
	full     bool
	base     int64
	lastSent int64
}

func (c cursor) String() string {
	if c.full {
		return fmt.Sprintf("f.%d.%d", c.base, c.lastSent)
	}
	return fmt.Sprintf("c.%d", c.lastSent)
}

// parseCursor reads an after value of record c. An absent one starts a full set.
func parseCursor(c *cluster, after string) (cursor, error) {
	if after == "" {
		return cursor{full: true, base: c.head, lastSent: -1}, nil
	}
	malformed := refuse(http.StatusBadRequest, codeInvalidRequest, "after is not a cursor of this record")
	parts := strings.Split(after, ".")
	numbers := make([]int64, 0, 2)
	for _, p := range parts[1:] {
		n, err := strconv.ParseInt(p, 10, 64)
		if err != nil || n < -1 || n > c.head {
			return cursor{}, malformed
		}
		numbers = append(numbers, n)
	}
	var parsed cursor
	switch {
	case parts[0] == "c" && len(numbers) == 1:
		parsed = cursor{base: numbers[0], lastSent: numbers[0]}
	case parts[0] == "f" && len(numbers) == 2:
		parsed = cursor{full: true, base: numbers[0], lastSent: numbers[1]}
	default:
		return cursor{}, malformed
	}
	if parsed.base <= c.purgedThrough {
		return cursor{}, refuse(http.StatusGone, codeCursorExpired, "the cursor is at or before the purged position")
	}
	return parsed, nil
}

// page answers the changes of record c after the cursor given (DECL-1, DECL-2, DECL-4, DECL-5, DECL-6).
// The caller holds s.mu.
func (s *Server) page(c *cluster, after string) (DeclarationPage, error) {
	at, err := parseCursor(c, after)
	if err != nil {
		return DeclarationPage{}, err
	}
	if at.full {
		return s.fullPage(c, at), nil
	}
	pending := make([]Change, 0)
	next := after
	for _, entry := range c.log {
		if entry.seq <= at.lastSent {
			continue
		}
		if len(pending) == s.cfg.PageSize {
			return DeclarationPage{Cursor: next, More: true, Changes: pending}, nil
		}
		pending = append(pending, entry.change)
		next = cursor{base: entry.seq, lastSent: entry.seq}.String()
	}
	return DeclarationPage{Cursor: next, Changes: pending}, nil
}

// fullPage answers the next page of a full set: the record's live declarations not yet sent, in the
// order they were last written, each as apply.
func (s *Server) fullPage(c *cluster, at cursor) DeclarationPage {
	var unsent []*declaration
	for _, d := range s.declarations {
		if d.cluster == c.grn && !d.tombstoned && d.lastSeq > at.lastSent {
			unsent = append(unsent, d)
		}
	}
	slices.SortFunc(unsent, func(a, b *declaration) int { return cmp.Compare(a.lastSeq, b.lastSeq) })
	page := DeclarationPage{Full: true, Changes: make([]Change, 0, min(len(unsent), s.cfg.PageSize))}
	for _, d := range unsent {
		if len(page.Changes) == s.cfg.PageSize {
			page.More = true
			page.Cursor = cursor{full: true, base: at.base, lastSent: at.lastSent}.String()
			return page
		}
		page.Changes = append(page.Changes, Change{Op: opApply, ID: d.id, Generation: d.generation, Object: d.object})
		at.lastSent = d.lastSeq
	}
	page.Cursor = cursor{base: at.base, lastSent: at.base}.String()
	return page
}

// write appends a change to record c's log and wakes its held polls. The caller holds s.mu.
func (c *cluster) write(d *declaration, op string) {
	c.head++
	d.lastSeq = c.head
	entry := change{seq: c.head, change: Change{Op: op, ID: d.id, Generation: d.generation}}
	if op == opApply {
		entry.change.Object = d.object
	}
	c.log = append(c.log, entry)
	c.notify()
}

// Declared is the fixture's answer to declare and to tombstone.
type Declared struct {
	ID         string `json:"id"`
	Generation int64  `json:"generation"`
}

// declare places an object on a record, as a new declaration or the next generation of the one id
// names. It refuses an object breaking DECL-7, DECL-8 or DECL-9, so none is stored or sent.
func (s *Server) declare(grn GRN, id string, object json.RawMessage) (Declared, error) {
	if len(id) > maxDeclarationID {
		return Declared{}, refuse(http.StatusBadRequest, codeInvalidRequest, "id is over %d characters", maxDeclarationID)
	}
	normalized, err := parseDeclaredObject(object)
	if err != nil {
		return Declared{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clusters[grn]
	if !ok {
		return Declared{}, errNoCluster
	}
	d, exists := s.declarations[id]
	switch {
	case !exists:
		if id == "" {
			drawn, err := s.identifier()
			if err != nil {
				return Declared{}, err
			}
			id = "decl-" + drawn
		}
		d = &declaration{id: id, cluster: grn}
		s.declarations[id] = d
	case d.cluster != grn:
		return Declared{}, refuse(http.StatusConflict, codeInvalidRequest, "declaration %s is placed on another record", id)
	case d.tombstoned:
		return Declared{}, refuse(http.StatusConflict, codeInvalidRequest, "declaration %s is tombstoned", id)
	}
	d.generation++
	d.object = normalized
	c.write(d, opApply)
	return Declared{ID: d.id, Generation: d.generation}, nil
}

// tombstone deletes a declaration by a change carrying its identifier and a generation past its last.
func (s *Server) tombstone(id string) (Declared, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, ok := s.declarations[id]
	if !ok || d.tombstoned {
		return Declared{}, refuse(http.StatusNotFound, codeNotFound, "no live declaration %s", id)
	}
	c, ok := s.clusters[d.cluster]
	if !ok {
		return Declared{}, errNoCluster
	}
	d.tombstoned = true
	d.generation++
	d.object = nil
	c.write(d, opDelete)
	return Declared{ID: d.id, Generation: d.generation}, nil
}

// Expired is the fixture's answer to expire-cursors.
type Expired struct {
	Cluster GRN `json:"cluster"`
}

// expireCursors purges every tombstone of a record and every cursor answered so far (DECL-6). The head
// moves one past the purged position, so the full set read next answers a cursor that is live.
func (s *Server) expireCursors(grn GRN) (Expired, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clusters[grn]
	if !ok {
		return Expired{}, errNoCluster
	}
	c.purgedThrough = c.head
	c.head++
	c.log = nil
	c.notify()
	return Expired{Cluster: grn}, nil
}

// parseDeclaredObject proves object is one admissible object of a declared kind by interface 1's own
// parser, as garam and the connector prove it: a declared kind (DECL-7), secrets named only by
// reference (DECL-8), and no image, since no field of the four kinds names one (DECL-9). It answers
// the object with only DECL-7's members, as it was written.
func parseDeclaredObject(object json.RawMessage) (json.RawMessage, error) {
	parsed, err := manifest.Parse(object)
	if err != nil {
		return nil, refuse(http.StatusBadRequest, codeInvalidRequest, "DECL-7 to DECL-9: %v", err)
	}
	if parsed.GetNamespace() == "" {
		return nil, refuse(http.StatusBadRequest, codeInvalidRequest, "DECL-7: metadata.namespace is missing")
	}
	var written declaredObject
	if err := json.Unmarshal(object, &written); err != nil {
		return nil, refuse(http.StatusBadRequest, codeInvalidRequest, "DECL-7: the object is not a DeclaredObject")
	}
	normalized, err := json.Marshal(written)
	if err != nil {
		return nil, fmt.Errorf("encode a declared object: %w", err)
	}
	return normalized, nil
}
