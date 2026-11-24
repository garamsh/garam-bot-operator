package clusteridentity_test

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/garam-agent-operator/interfaces/cluster-connection/v1alpha1/reference"
	"github.com/garamsh/garam-agent-operator/internal/clusteridentity"
)

const (
	regPath  = "/cluster-connection/v1alpha1/registrations"
	keysPath = "/cluster-connection/v1alpha1/clusters/self/keys"
	declPath = "/cluster-connection/v1alpha1/clusters/self/declarations"
	// tokenPath is the reference server's exchange.
	tokenPath = "/oauth/token"
)

// maxWaits bounds how often one test's Service may wait. Every Sleep returns at once, so a retry loop
// that never settles would otherwise spin until the test binary's timeout.
const maxWaits = 50

var errWaitedTooOften = errors.New("the service waited more than 50 times: a retry loop did not settle")

// testKeys are three RSA 3072 keys, generated once: each takes most of a second.
var testKeys = sync.OnceValue(func() []*rsa.PrivateKey {
	out := make([]*rsa.PrivateKey, 3)
	for i := range out {
		k, err := rsa.GenerateKey(rand.Reader, 3072)
		if err != nil {
			panic(err)
		}
		out[i] = k
	}
	return out
})

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// memoryStore is a KeyStore in memory that keeps every State saved, in order.
type memoryStore struct {
	mu    sync.Mutex
	saved []clusteridentity.State
	// fail, where set, is asked before each Save, and its error is the Save's, which keeps nothing.
	fail func(clusteridentity.State) error
}

func (m *memoryStore) Load(context.Context) (clusteridentity.State, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if len(m.saved) == 0 {
		return clusteridentity.State{}, clusteridentity.ErrNoState
	}
	return m.saved[len(m.saved)-1], nil
}

func (m *memoryStore) Save(_ context.Context, state clusteridentity.State) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.fail != nil {
		if err := m.fail(state); err != nil {
			return err
		}
	}
	m.saved = append(m.saved, state)
	return nil
}

func (m *memoryStore) last() clusteridentity.State {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saved[len(m.saved)-1]
}

// seen is one request the reference server was sent: its path, header and body.
type seen struct {
	path   string
	header http.Header
	body   []byte
}

// harness is the reference server behind an interceptor, a token file, and what a Service is built from.
type harness struct {
	t       *testing.T
	clock   *clock
	base    string
	http    *http.Client
	fixture http.Handler
	store   *memoryStore
	dir     string
	log     bytes.Buffer
	logMu   sync.Mutex

	mu sync.Mutex
	// intercept, where it answers true, has answered the request in the server's place.
	intercept func(w http.ResponseWriter, r *http.Request, body []byte, next http.Handler) bool
	requests  []seen
	keys      []*rsa.PrivateKey
	sleeps    []time.Duration
	// onSleep, where set, runs at each Sleep, and its error ends the wait with it.
	onSleep func(n int, d time.Duration) error
	// random, requestTimeout and rotationPeriod, where set, are the Service's Random, RequestTimeout and
	// RotationPeriod.
	random         io.Reader
	requestTimeout time.Duration
	rotationPeriod time.Duration
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	h := &harness{t: t, clock: &clock{now: time.Date(2026, 11, 25, 9, 0, 0, 0, time.UTC)}, store: &memoryStore{},
		dir: t.TempDir(), keys: append([]*rsa.PrivateKey{}, testKeys()...)}
	var server http.Handler
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		h.mu.Lock()
		h.requests = append(h.requests, seen{path: r.URL.Path, header: r.Header.Clone(), body: body})
		intercept := h.intercept
		h.mu.Unlock()
		if intercept != nil && intercept(w, r, body, server) {
			return
		}
		server.ServeHTTP(w, r)
	}))
	ts.StartTLS()
	t.Cleanup(ts.Close)
	ref := reference.New(reference.Config{BaseURL: ts.URL, PageSize: 100, Now: h.clock.Now, Random: rand.Reader})
	server = ref.Handler()
	h.base, h.http, h.fixture = ts.URL, ts.Client(), ref.Fixture()
	return h
}

func (h *harness) Write(p []byte) (int, error) {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	return h.log.Write(p)
}

func (h *harness) logged() string {
	h.logMu.Lock()
	defer h.logMu.Unlock()
	return h.log.String()
}

// service is a Service over the harness. Its Sleep records each wait, advances the clock by it, and
// returns at once.
func (h *harness) service() clusteridentity.Service {
	return clusteridentity.NewService(clusteridentity.Config{
		BaseURL:        h.base,
		HTTP:           h.http,
		TokenFile:      h.tokenFile(),
		Version:        "test",
		Store:          h.store,
		Log:            slog.New(slog.NewJSONHandler(h, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Now:            h.clock.Now,
		Random:         h.random,
		RequestTimeout: h.requestTimeout,
		RotationPeriod: h.rotationPeriod,
		Sleep: func(ctx context.Context, d time.Duration) error {
			h.mu.Lock()
			h.sleeps = append(h.sleeps, d)
			n, onSleep := len(h.sleeps), h.onSleep
			h.mu.Unlock()
			h.clock.advance(d)
			if n > maxWaits {
				return errWaitedTooOften
			}
			if onSleep != nil {
				if err := onSleep(n, d); err != nil {
					return err
				}
			}
			return ctx.Err()
		},
		GenerateKey: func() (*rsa.PrivateKey, error) {
			h.mu.Lock()
			defer h.mu.Unlock()
			k := h.keys[0]
			h.keys = h.keys[1:]
			return k, nil
		},
	})
}

func (h *harness) tokenFile() string { return filepath.Join(h.dir, "registration-token") }

func (h *harness) writeToken(token string) {
	require.NoError(h.t, os.WriteFile(h.tokenFile(), []byte(token+"\n"), 0o600))
}

// fix runs one fixture action against the reference server.
func (h *harness) fix(action string, q reference.FixtureRequest) map[string]any {
	h.t.Helper()
	encoded, err := json.Marshal(q)
	require.NoError(h.t, err)
	rec := httptest.NewRecorder()
	h.fixture.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/"+action, bytes.NewReader(encoded)))
	require.Equal(h.t, http.StatusOK, rec.Code, rec.Body.String())
	var out map[string]any
	require.NoError(h.t, json.Unmarshal(rec.Body.Bytes(), &out))
	return out
}

// mint mints a registration token for a new record and writes it to the token file.
func (h *harness) mint() (string, string) {
	got := h.fix("mint-token", reference.FixtureRequest{})
	token, grn := got["registrationToken"].(string), got["cluster"].(string)
	h.writeToken(token)
	return token, grn
}

// establish mints a token and establishes a Connection with it.
func (h *harness) establish() clusteridentity.Connection {
	h.t.Helper()
	h.mint()
	connection, err := h.service().Establish(context.Background())
	require.NoError(h.t, err)
	return connection
}

// sent are the requests the server was sent to path.
func (h *harness) sent(path string) []seen {
	h.mu.Lock()
	defer h.mu.Unlock()
	var out []seen
	for _, s := range h.requests {
		if s.path == path {
			out = append(out, s)
		}
	}
	return out
}

func (h *harness) setIntercept(f func(w http.ResponseWriter, r *http.Request, body []byte, next http.Handler) bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.intercept = f
}

// poll sends a declarations request over client, which needs a live token and nothing else.
func poll(t *testing.T, client *http.Client, base string) int {
	t.Helper()
	resp, err := client.Get(base + declPath + "?wait=0")
	require.NoError(t, err)
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode
}

// drop closes the connection without answering: an answer that never arrives.
func drop(t *testing.T, w http.ResponseWriter) {
	conn, _, err := w.(http.Hijacker).Hijack()
	require.NoError(t, err)
	_ = conn.Close()
}

// answerThenDrop has the server handle the request, so it takes effect, and drops its answer.
func answerThenDrop(t *testing.T, w http.ResponseWriter, r *http.Request, next http.Handler) {
	next.ServeHTTP(httptest.NewRecorder(), r)
	drop(t, w)
}

func kid(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	sum, err := (&jose.JSONWebKey{Key: &key.PublicKey}).Thumbprint(crypto.SHA256)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(sum)
}

// headerKID reads the kid of a compact JWS's header, without verifying it.
func headerKID(t *testing.T, compact string) string {
	t.Helper()
	encoded, _, ok := strings.Cut(compact, ".")
	require.True(t, ok)
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	require.NoError(t, err)
	var header struct {
		KID string `json:"kid"`
	}
	require.NoError(t, json.Unmarshal(raw, &header))
	return header.KID
}

// exchangeKIDs are the kids of every assertion the server was sent at the exchange, in order.
func (h *harness) exchangeKIDs() []string {
	exchanges := h.sent(tokenPath)
	out := make([]string, 0, len(exchanges))
	for _, s := range exchanges {
		values, err := urlValues(s.body)
		require.NoError(h.t, err)
		out = append(out, headerKID(h.t, values.Get("client_assertion")))
	}
	return out
}

func urlValues(body []byte) (url.Values, error) { return url.ParseQuery(string(body)) }
