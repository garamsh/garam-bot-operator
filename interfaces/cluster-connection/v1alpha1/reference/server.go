package reference

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// org is the organization every cluster record of this server belongs to.
	org = "reference"
	// protocolPrefix begins every protocol path (T-3).
	protocolPrefix = "/cluster-connection/v1alpha1"
	// tokenPath is where this server answers the exchange it names as tokenUrl.
	tokenPath = "/oauth/token"
	// keysPath is ROT-1's route, whose absolute URL is the audience of its JWSs.
	keysPath = protocolPrefix + "/clusters/self/keys"
	// accessTokenLifetime is the lifetime of an exchanged token.
	accessTokenLifetime = time.Hour
	// secretBytes is the random length of a registration token and of an access token: 256 bits.
	secretBytes = 32
)

// Config is what a Server is built from.
type Config struct {
	// BaseURL is the absolute https URL clients reach this server at, without a trailing slash.
	BaseURL string
	// PageSize bounds the changes one declarations answer carries before it says more (DECL-5).
	PageSize int
	// RegistrationLimit bounds registration requests per source address per minute (REG-8); 0 bounds none.
	RegistrationLimit int
	// Now is the clock.
	Now func() time.Time
	// Random is the cryptographically secure source every token and identifier is drawn from.
	Random io.Reader
	Log    *slog.Logger
}

// GRN names a cluster record, grn:<org>/iam/cluster/<id>.
type GRN string

// Server holds every cluster record in memory, behind one lock.
type Server struct {
	cfg Config

	mu                 sync.Mutex
	clusters           map[GRN]*cluster
	registrationTokens map[digest]registrationToken
	accessTokens       map[digest]accessToken
	declarations       map[string]*declaration
	// seenJTI holds each verified jti, by audience, until its exp.
	seenJTI  map[string]time.Time
	throttle map[string]window
}

// digest is the SHA-256 of a secret; a secret itself is never kept.
type digest [sha256.Size]byte

func digestOf(secret string) digest { return sha256.Sum256([]byte(secret)) }

// cluster is one cluster record.
type cluster struct {
	grn  GRN
	keys map[keyID]*recordKey
	// held is every key the record has held, live or not, so none of them is made live again (ROT-4).
	held map[keyID]bool
	// log holds every change written after purgedThrough, in write order.
	log           []change
	head          int64
	purgedThrough int64
	report        *Report
	lastContactAt time.Time
	// wake is closed and replaced on every write a held poll may answer.
	wake chan struct{}
}

// recordKey is a key bound to a record.
type recordKey struct {
	public publicKey
	// retiresAt is when the key stops being live, or zero for never.
	retiresAt time.Time
	// rotation is the answer that made the key live, or nil for a key registration bound.
	rotation *KeyRotation
}

type registrationToken struct {
	cluster GRN
	expires time.Time
}

type accessToken struct {
	cluster GRN
	key     keyID
	expires time.Time
}

// New builds a Server holding no cluster record.
func New(cfg Config) *Server {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	return &Server{
		cfg:                cfg,
		clusters:           map[GRN]*cluster{},
		registrationTokens: map[digest]registrationToken{},
		accessTokens:       map[digest]accessToken{},
		declarations:       map[string]*declaration{},
		seenJTI:            map[string]time.Time{},
		throttle:           map[string]window{},
	}
}

// Handler serves the four routes of openapi.yaml and the exchange at tokenUrl.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.Handle("POST "+protocolPrefix+"/registrations", serve(s.cfg.Log, s.register))
	mux.Handle("POST "+keysPath, serve(s.cfg.Log, s.rotateKey))
	mux.Handle("GET "+protocolPrefix+"/clusters/self/declarations", serve(s.cfg.Log, s.watchDeclarations))
	mux.Handle("POST "+protocolPrefix+"/clusters/self/reports", serve(s.cfg.Log, s.report))
	mux.HandleFunc("POST "+tokenPath, s.exchange)
	mux.Handle("/", serve(s.cfg.Log, func(http.ResponseWriter, *http.Request) error {
		return refuse(http.StatusNotFound, codeNotFound, "no such route")
	}))
	return mux
}

func (s *Server) tokenURL() string { return s.cfg.BaseURL + tokenPath }

// secret draws a fresh random value, base64url without padding.
func (s *Server) secret() (string, error) {
	b := make([]byte, secretBytes)
	if _, err := io.ReadFull(s.cfg.Random, b); err != nil {
		return "", fmt.Errorf("draw a random value: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// identifier draws a fresh identifier of 64 random bits, in hex.
func (s *Server) identifier() (string, error) {
	b := make([]byte, 8)
	if _, err := io.ReadFull(s.cfg.Random, b); err != nil {
		return "", fmt.Errorf("draw an identifier: %w", err)
	}
	return hex.EncodeToString(b), nil
}

// newCluster creates a record with no key. The caller holds s.mu.
func (s *Server) newCluster() (*cluster, error) {
	id, err := s.identifier()
	if err != nil {
		return nil, err
	}
	grn := GRN(fmt.Sprintf("grn:%s/iam/cluster/%s", org, id))
	c := &cluster{
		grn: grn, keys: map[keyID]*recordKey{}, held: map[keyID]bool{}, purgedThrough: -1, wake: make(chan struct{}),
	}
	s.clusters[grn] = c
	return c, nil
}

// liveKeys are the record's keys still live at now. Retired keys are dropped as they are found.
func (c *cluster) liveKeys(now time.Time) map[keyID]*recordKey {
	for id, k := range c.keys {
		if !k.retiresAt.IsZero() && !now.Before(k.retiresAt) {
			delete(c.keys, id)
		}
	}
	return c.keys
}

// notify wakes every poll held on the record.
func (c *cluster) notify() {
	close(c.wake)
	c.wake = make(chan struct{})
}

// authenticate answers the record a request's bearer token was exchanged for, while the token, the
// record and the key it was exchanged under are all live (AUTH-2), and records the contact (REP-5).
// The caller holds s.mu.
func (s *Server) authenticate(r *http.Request) (*cluster, error) {
	unauthorized := refuse(http.StatusUnauthorized, codeUnauthorized, "no live token")
	bearer, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || bearer == "" {
		return nil, unauthorized
	}
	now := s.cfg.Now()
	token, ok := s.accessTokens[digestOf(bearer)]
	if !ok || !now.Before(token.expires) {
		return nil, unauthorized
	}
	c, ok := s.clusters[token.cluster]
	if !ok {
		return nil, unauthorized
	}
	if _, ok := c.liveKeys(now)[token.key]; !ok {
		return nil, unauthorized
	}
	c.lastContactAt = now
	return c, nil
}

// authenticateFirst refuses a request with no live token before anything else of it is read, so every
// such request is answered 401 whatever else it carries (AUTH-2).
func (s *Server) authenticateFirst(r *http.Request) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.authenticate(r)
	return err
}

// replayed records a verified jti for audience, answering whether it was seen before. The caller holds s.mu.
func (s *Server) replayed(audience string, claims signedClaims) bool {
	now := s.cfg.Now()
	for k, exp := range s.seenJTI {
		if now.After(exp) {
			delete(s.seenJTI, k)
		}
	}
	key := audience + " " + claims.ID
	if _, ok := s.seenJTI[key]; ok {
		return true
	}
	s.seenJTI[key] = claims.Expiry.Time()
	return false
}
