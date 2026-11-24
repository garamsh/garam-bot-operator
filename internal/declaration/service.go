package declaration

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
)

// resync is how often every held declaration is applied again with no change to wake it, so drift in
// a garam-owned object is reverted at least that often (ADR 0089 decision 2).
const resync = 10 * time.Minute

// Config is what a Service is built from.
type Config struct {
	// BaseURL is the cluster-connection base URL, without a trailing slash (T-3).
	BaseURL string
	// HTTP carries the record's token on every request (AUTH-1): the identity's Connection.Client.
	HTTP *http.Client
	// Record is the GRN of the cluster record the connector is, which its objects' marker names.
	Record string
	// Release is this release's identity, <release-namespace>/<release-name>, which a claim names.
	Release string
	// Namespaces are the watched namespaces.
	Namespaces []string
	// ReportOnly applies nothing and deletes nothing (DECL-15).
	ReportOnly bool
	// Version is the connector's version, which every report names (REP-1).
	Version string
	Objects Objects
	Log     *slog.Logger
	// Now, Sleep and After default to the clock, a timer and time.After.
	Now   func() time.Time
	Sleep func(ctx context.Context, d time.Duration) error
	After func(d time.Duration) <-chan time.Time
}

// Service applies garam's declarations to the watched namespaces, and reports what it found.
type Service interface {
	// Run polls the declarations, applies them and reports, until ctx ends.
	Run(ctx context.Context) error
	// Status answers what the last pass found, and false before the first full set was applied.
	Status() (Status, bool)
}

type service struct {
	cfg     Config
	watched map[string]bool
	// wake is signalled when the declarations held change, and passDone when a pass has finished.
	wake     chan struct{}
	passDone chan struct{}

	mu sync.Mutex
	// held are the declarations held, by identifier, and tombstones the ones deleted and not yet
	// carried out. full is set once the first full set was read.
	held       map[string]declared
	tombstones map[string]bool
	full       bool
	status     *Status
}

// NewService returns a Service under cfg.
func NewService(cfg Config) Service {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleep
	}
	if cfg.After == nil {
		cfg.After = time.After
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	watched := make(map[string]bool, len(cfg.Namespaces))
	for _, namespace := range cfg.Namespaces {
		watched[namespace] = true
	}
	return &service{cfg: cfg, watched: watched, wake: make(chan struct{}, 1), passDone: make(chan struct{}, 1),
		held:       map[string]declared{},
		tombstones: map[string]bool{}}
}

func (s *service) Run(ctx context.Context) error {
	group, ctx := errgroup.WithContext(ctx)
	group.Go(func() error { return s.feed(ctx) })
	group.Go(func() error { return s.passes(ctx) })
	group.Go(func() error { return s.reports(ctx) })
	return group.Wait()
}

func (s *service) Status() (Status, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.status == nil {
		return Status{}, false
	}
	return *s.status, true
}

// replace holds a full set in place of everything held. A full set names every live declaration, so
// a tombstone not carried out is dropped with the rest: what it named is reported as an orphan.
func (s *service) replace(full map[string]declared) {
	s.mu.Lock()
	s.held, s.tombstones, s.full = full, map[string]bool{}, true
	s.mu.Unlock()
	s.signal()
}

// take holds the changes after the cursor, in the order they were written (DECL-2).
func (s *service) take(changes []change) {
	if len(changes) == 0 {
		return
	}
	s.mu.Lock()
	for _, c := range changes {
		if held, ok := s.held[c.ID]; ok && held.generation > c.Generation {
			// DECL-10: a lower generation than the one held is never applied.
			continue
		}
		switch c.Op {
		case opApply:
			s.held[c.ID] = declare(s.cfg.Record, c.ID, c.Generation, c.Object)
			delete(s.tombstones, c.ID)
		case opDelete:
			delete(s.held, c.ID)
			s.tombstones[c.ID] = true
		}
	}
	s.mu.Unlock()
	s.signal()
}

func (s *service) signal() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}

// passed tells the reports a pass has finished.
func (s *service) passed() {
	select {
	case s.passDone <- struct{}{}:
	default:
	}
}

// passes runs a pass on every change of the declarations held and of the cluster, and every 10
// minutes, once the first full set was read. A pass that failed is run again after a back-off.
func (s *service) passes(ctx context.Context) error {
	wait := firstBackoff
	var retry <-chan time.Time
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.wake:
		case <-s.cfg.Objects.Changed():
		case <-s.cfg.After(resync):
		case <-retry:
		}
		retry = nil
		if err := s.pass(ctx); err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			s.cfg.Log.Warn("a declarations pass did not finish; running it again", "error", err, "wait", wait)
			retry = s.cfg.After(wait)
			wait = min(2*wait, lastBackoff)
			continue
		}
		wait = firstBackoff
	}
}

// pass reads the cluster, decides, and carries the writes out: input, stages, output.
func (s *service) pass(ctx context.Context) error {
	s.mu.Lock()
	if !s.full {
		s.mu.Unlock()
		return nil
	}
	held, tombstones := maps.Clone(s.held), maps.Clone(s.tombstones)
	s.mu.Unlock()

	observed := s.cfg.Now()
	seen, err := s.read(ctx)
	if err != nil {
		return err
	}
	decided := decide(s.cfg.Record, s.cfg.ReportOnly, s.watched, held, tombstones, seen)
	var failed []error
	for _, w := range decided.writes {
		if err := s.carry(ctx, w, decided.results); err != nil {
			failed = append(failed, err)
		}
	}
	s.mu.Lock()
	for _, id := range decided.buried {
		delete(s.tombstones, id)
	}
	results := make([]Result, 0, len(decided.results))
	for _, id := range sortedKeys(decided.results) {
		results = append(results, decided.results[id])
	}
	s.status = &Status{ObservedAt: observed, ReportOnly: s.cfg.ReportOnly, Namespaces: decided.claimed,
		ClaimedElsewhere: decided.elsewhere, Declarations: results, Orphans: decided.orphans,
		Objects: decided.objects}
	s.mu.Unlock()
	s.passed()
	return errors.Join(failed...)
}

// read is the cluster as a pass decides from: each watched namespace's claim, and every object of a
// declared kind in the namespaces claimed for this release (CLAIM-2).
func (s *service) read(ctx context.Context) (view, error) {
	seen := view{claimed: map[string]bool{}}
	for _, namespace := range slices.Sorted(maps.Keys(s.watched)) {
		holder, found, err := s.cfg.Objects.Claim(ctx, namespace)
		if err != nil {
			return view{}, err
		}
		if !found || holder != s.cfg.Release {
			seen.elsewhere = append(seen.elsewhere, namespace)
			continue
		}
		seen.claimed[namespace] = true
		objects, err := s.cfg.Objects.List(ctx, namespace)
		if err != nil {
			return view{}, err
		}
		seen.objects = append(seen.objects, objects...)
	}
	return seen, nil
}

// carry carries out one write, and records a create or apply that succeeded as applied. A write the API
// server refused for the object it carries makes the declaration invalid. One that lost a race with
// another writer is decided again by the next pass, which that writer's change wakes. Any other failure
// records nothing and is the pass's error, so the pass runs again.
func (s *service) carry(ctx context.Context, w write, results map[string]Result) error {
	var err error
	switch w.op {
	case create:
		err = s.cfg.Objects.Create(ctx, w.declaration.object.DeepCopy())
	case apply:
		err = s.cfg.Objects.Apply(ctx, w.declaration.object.DeepCopy(), w.held)
	case remove:
		err = s.cfg.Objects.Delete(ctx, w.held)
	}
	switch {
	case err == nil && w.op != remove:
		results[w.declaration.id] = Result{ID: w.declaration.id, Generation: w.declaration.generation, Outcome: Applied}
		return nil
	case err == nil, errors.Is(err, ErrNameTaken), errors.Is(err, ErrChanged):
		return nil
	case w.op != remove && (apierrors.IsInvalid(err) || apierrors.IsBadRequest(err)):
		results[w.declaration.id] = Result{ID: w.declaration.id, Generation: w.declaration.generation,
			Outcome: Invalid, Message: bounded("the API server refused the object: " + err.Error())}
		return nil
	}
	return err
}

// sleep waits d, or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
