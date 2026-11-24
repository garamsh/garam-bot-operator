package reference_test

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/garam-agent-operator/interfaces/cluster-connection/v1alpha1/reference"
)

const (
	prefix   = "/cluster-connection/v1alpha1"
	declPath = prefix + "/clusters/self/declarations"
	keysPath = prefix + "/clusters/self/keys"
	repPath  = prefix + "/clusters/self/reports"
	regPath  = prefix + "/registrations"
)

// testKeys are three RSA 3072 keys, generated once: generation takes most of a second each.
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

// clock is a settable clock.
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

// harness is a reference server over httptest, its fixture, and a client of both.
type harness struct {
	t       *testing.T
	clock   *clock
	base    string
	http    *http.Client
	fixture http.Handler
}

type options struct {
	pageSize          int
	registrationLimit int
	random            io.Reader
}

func newHarness(t *testing.T, opts ...func(*options)) *harness {
	t.Helper()
	o := options{pageSize: 100, random: rand.Reader}
	for _, set := range opts {
		set(&o)
	}
	var handler http.Handler
	ts := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		handler.ServeHTTP(w, r)
	}))
	ts.StartTLS()
	t.Cleanup(ts.Close)
	c := &clock{now: time.Date(2026, 11, 24, 12, 0, 0, 0, time.UTC)}
	server := reference.New(reference.Config{
		BaseURL: ts.URL, PageSize: o.pageSize, RegistrationLimit: o.registrationLimit,
		Now: c.Now, Random: o.random,
	})
	handler = server.Handler()
	return &harness{t: t, clock: c, base: ts.URL, http: ts.Client(), fixture: server.Fixture()}
}

func pageSize(n int) func(*options) { return func(o *options) { o.pageSize = n } }

func registrationLimit(n int) func(*options) { return func(o *options) { o.registrationLimit = n } }

func random(r io.Reader) func(*options) { return func(o *options) { o.random = r } }

// answer is a response read whole.
type answer struct {
	status int
	header http.Header
	body   []byte
}

func (a answer) json(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(a.body, &out), string(a.body))
	return out
}

// errorCode reads T-5's error member, requiring the message member beside it.
func (a answer) errorCode(t *testing.T) string {
	t.Helper()
	body := a.json(t)
	require.Contains(t, body, "message", string(a.body))
	code, _ := body["error"].(string)
	return code
}

func (h *harness) do(method, path, bearer, contentType string, body []byte) answer {
	h.t.Helper()
	req, err := http.NewRequest(method, h.base+path, bytes.NewReader(body))
	require.NoError(h.t, err)
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	if bearer != "" {
		req.Header.Set("Authorization", "Bearer "+bearer)
	}
	resp, err := h.http.Do(req)
	require.NoError(h.t, err)
	defer func() { _ = resp.Body.Close() }()
	read, err := io.ReadAll(resp.Body)
	require.NoError(h.t, err)
	return answer{status: resp.StatusCode, header: resp.Header, body: read}
}

func (h *harness) post(path, bearer string, body any) answer {
	h.t.Helper()
	encoded, err := json.Marshal(body)
	require.NoError(h.t, err)
	return h.do(http.MethodPost, path, bearer, "application/json", encoded)
}

// fix runs one fixture action and requires it to succeed.
func (h *harness) fix(action string, q reference.FixtureRequest) map[string]any {
	h.t.Helper()
	status, body := h.tryFix(action, q)
	require.Equal(h.t, http.StatusOK, status, "%s: %s", action, body)
	var out map[string]any
	require.NoError(h.t, json.Unmarshal(body, &out))
	return out
}

func (h *harness) tryFix(action string, q reference.FixtureRequest) (int, []byte) {
	h.t.Helper()
	encoded, err := json.Marshal(q)
	require.NoError(h.t, err)
	rec := httptest.NewRecorder()
	h.fixture.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/"+action, bytes.NewReader(encoded)))
	return rec.Code, rec.Body.Bytes()
}

// mint mints a registration token for a new record, answering the token and the record.
func (h *harness) mint() (string, reference.GRN) {
	h.t.Helper()
	got := h.fix("mint-token", reference.FixtureRequest{})
	return got["registrationToken"].(string), reference.GRN(got["cluster"].(string))
}

func publicJWK(t *testing.T, key *rsa.PrivateKey) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(jose.JSONWebKey{Key: &key.PublicKey, Algorithm: "RS256", Use: "sig"})
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(encoded, &out))
	return out
}

func registration(token string, jwk map[string]any) map[string]any {
	return map[string]any{"registrationToken": token, "jwk": jwk, "connector": map[string]any{"version": "test"}}
}

// register registers key on a new record and answers the record.
func (h *harness) register(key *rsa.PrivateKey) reference.GRN {
	h.t.Helper()
	token, grn := h.mint()
	got := h.post(regPath, "", registration(token, publicJWK(h.t, key)))
	require.Equal(h.t, http.StatusCreated, got.status, string(got.body))
	return grn
}

func kid(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	sum, err := (&jose.JSONWebKey{Key: &key.PublicKey}).Thumbprint(crypto.SHA256)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(sum)
}

// signed is a compact RS256 JWS over claims, by key, naming kidOf's thumbprint and typ.
func signed(t *testing.T, key, kidOf *rsa.PrivateKey, typ string, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: kid(t, kidOf)}},
		(&jose.SignerOptions{}).WithType(jose.ContentType(typ)),
	)
	require.NoError(t, err)
	payload, err := json.Marshal(claims)
	require.NoError(t, err)
	jws, err := signer.Sign(payload)
	require.NoError(t, err)
	compact, err := jws.CompactSerialize()
	require.NoError(t, err)
	return compact
}

func freshJTI() string {
	return rand.Text()
}

// claims are a JWS's claims for client to audience at the harness's now.
func (h *harness) claims(client reference.GRN, audience string) map[string]any {
	now := h.clock.Now().Unix()
	return map[string]any{
		"iss": string(client), "sub": string(client), "aud": audience,
		"iat": now, "exp": now + 60, "jti": freshJTI(),
	}
}

// tryExchange exchanges an assertion for a token.
func (h *harness) tryExchange(assertion string, client reference.GRN) answer {
	h.t.Helper()
	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_id":             {string(client)},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {assertion},
	}
	return h.do(http.MethodPost, "/oauth/token", "", "application/x-www-form-urlencoded", []byte(form.Encode()))
}

// exchange exchanges a fresh assertion by key for a token, requiring success.
func (h *harness) exchange(client reference.GRN, key *rsa.PrivateKey) string {
	h.t.Helper()
	got := h.tryExchange(signed(h.t, key, key, "JWT", h.claims(client, h.base+"/oauth/token")), client)
	require.Equal(h.t, http.StatusOK, got.status, string(got.body))
	return got.json(h.t)["access_token"].(string)
}

// poll reads the declarations after cursor, with wait seconds.
func (h *harness) poll(bearer, after string, wait int) answer {
	h.t.Helper()
	q := url.Values{"wait": {strconv.Itoa(wait)}}
	if after != "" {
		q.Set("after", after)
	}
	return h.do(http.MethodGet, declPath+"?"+q.Encode(), bearer, "", nil)
}

// page is a declarations answer.
type page struct {
	Cursor  string `json:"cursor"`
	Full    bool   `json:"full"`
	More    bool   `json:"more"`
	Changes []struct {
		Op         string          `json:"op"`
		ID         string          `json:"id"`
		Generation int64           `json:"generation"`
		Object     json.RawMessage `json:"object"`
	} `json:"changes"`
}

func (a answer) page(t *testing.T) page {
	t.Helper()
	require.Equal(t, http.StatusOK, a.status, string(a.body))
	var p page
	require.NoError(t, json.Unmarshal(a.body, &p))
	return p
}

func (p page) ids() []string {
	out := make([]string, 0, len(p.Changes))
	for _, c := range p.Changes {
		out = append(out, c.Op+":"+c.ID)
	}
	return out
}

// bot is a valid declared object, by interface 1's schema.
func bot(name string) map[string]any {
	return map[string]any{
		"apiVersion": "bot.garam.sh/v1alpha1", "kind": "Bot",
		"metadata": map[string]any{"namespace": "bots", "name": name},
		"spec": map[string]any{
			"persona": map[string]any{"role": "reviewer"}, "agent": map[string]any{"harness": "claude-code"},
			"permissions": map[string]any{"secrets": []any{"bot-tokens/token"}},
		},
	}
}

func (h *harness) declare(grn reference.GRN, id string, object map[string]any) string {
	h.t.Helper()
	encoded, err := json.Marshal(object)
	require.NoError(h.t, err)
	return h.fix("declare", reference.FixtureRequest{Cluster: grn, ID: id, Object: encoded})["id"].(string)
}
