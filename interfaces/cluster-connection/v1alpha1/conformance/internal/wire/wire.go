// Package wire is the server suite's own client of cluster-connection v1alpha1: it reaches a server
// only through the protocol and the server fixture conformance.md names, never through its internals.
package wire

import (
	"bytes"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/require"
)

// The routes of README.md, relative to the base URL (T-3).
const (
	Prefix            = "/cluster-connection/v1alpha1"
	RegistrationsPath = Prefix + "/registrations"
	KeysPath          = Prefix + "/clusters/self/keys"
	DeclarationsPath  = Prefix + "/clusters/self/declarations"
	ReportsPath       = Prefix + "/clusters/self/reports"
)

// The environment a server suite run is given.
const (
	EnvBaseURL   = "CONFORMANCE_BASE_URL"
	EnvTrustFile = "CONFORMANCE_TRUST_FILE"
	EnvFixture   = "CONFORMANCE_FIXTURE"
)

// Target is the server under test: its base URL, a client trusting only its configured root (T-2),
// and the fixture command.
type Target struct {
	BaseURL string
	HTTP    *http.Client
	Fixture []string
}

// FromEnv reads the target from the environment a run is given.
func FromEnv() (*Target, error) {
	base, trust, fixture := os.Getenv(EnvBaseURL), os.Getenv(EnvTrustFile), strings.Fields(os.Getenv(EnvFixture))
	if base == "" || trust == "" || len(fixture) == 0 {
		return nil, fmt.Errorf("%s, %s and %s name the server under test, and one is not set", EnvBaseURL,
			EnvTrustFile, EnvFixture)
	}
	pem, err := os.ReadFile(trust)
	if err != nil {
		return nil, fmt.Errorf("read the trust root: %w", err)
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, fmt.Errorf("%s holds no PEM certificate", trust)
	}
	client := &http.Client{Timeout: 70 * time.Second, Transport: &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: roots, MinVersion: tls.VersionTLS12},
	}}
	return &Target{BaseURL: strings.TrimRight(base, "/"), HTTP: client, Fixture: fixture}, nil
}

// Answer is a response read whole.
type Answer struct {
	Status int
	Header http.Header
	Body   []byte
}

// JSON is the answer's body as one JSON object.
func (a Answer) JSON(t *testing.T) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(a.Body, &out), "status %d: %s", a.Status, a.Body)
	return out
}

// ErrorCode is T-5's error member, requiring the message member beside it.
func (a Answer) ErrorCode(t *testing.T) string {
	t.Helper()
	body := a.JSON(t)
	require.Contains(t, body, "message", string(a.Body))
	code, _ := body["error"].(string)
	return code
}

// Do sends one request to path, relative to the base URL, under bearer where it is not empty.
func (s *Target) Do(t *testing.T, method, path, bearer, contentType string, body []byte) Answer {
	t.Helper()
	return s.DoURL(t, method, s.BaseURL+path, bearer, contentType, body)
}

// DoURL sends one request to an absolute URL.
func (s *Target) DoURL(t *testing.T, method, target, bearer, contentType string, body []byte) Answer {
	t.Helper()
	request, err := http.NewRequest(method, target, bytes.NewReader(body))
	require.NoError(t, err)
	if contentType != "" {
		request.Header.Set("Content-Type", contentType)
	}
	if bearer != "" {
		request.Header.Set("Authorization", "Bearer "+bearer)
	}
	response, err := s.HTTP.Do(request)
	require.NoError(t, err)
	defer func() { _ = response.Body.Close() }()
	read, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	return Answer{Status: response.StatusCode, Header: response.Header, Body: read}
}

// Post sends body as JSON.
func (s *Target) Post(t *testing.T, path, bearer string, body any) Answer {
	t.Helper()
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	return s.Do(t, http.MethodPost, path, bearer, "application/json", encoded)
}

// Fix runs one fixture subcommand and requires it to succeed, answering what it printed.
func (s *Target) Fix(t *testing.T, args ...string) map[string]any {
	t.Helper()
	out, err := s.TryFix(t, args...)
	require.NoError(t, err)
	return out
}

// TryFix runs one fixture subcommand, answering what it printed, or an error carrying it where the
// subcommand exited non-zero.
func (s *Target) TryFix(t *testing.T, args ...string) (map[string]any, error) {
	t.Helper()
	command := exec.Command(s.Fixture[0], append(append([]string{}, s.Fixture[1:]...), args...)...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	err := command.Run()
	var out map[string]any
	if decodeErr := json.Unmarshal(stdout.Bytes(), &out); decodeErr != nil && err == nil {
		err = fmt.Errorf("the fixture printed no JSON object: %q", stdout.String())
	}
	if err != nil {
		return out, fmt.Errorf("fixture %v: %w: %s%s", args, err, stdout.String(), stderr.String())
	}
	return out, nil
}

// Declare places object on cluster through the fixture, as the declaration id names or as a new one
// where id is empty, answering the fixture's error where it refused it.
func (s *Target) Declare(t *testing.T, cluster, id string, object map[string]any) (string, error) {
	t.Helper()
	file := filepath.Join(t.TempDir(), "declaration.json")
	encoded, err := json.Marshal(map[string]any{"cluster": cluster, "id": id, "object": object})
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(file, encoded, 0o600))
	got, err := s.TryFix(t, "declare", file)
	if err != nil {
		return "", err
	}
	declared, _ := got["id"].(string)
	return declared, nil
}

// MustDeclare places object and requires the fixture to take it.
func (s *Target) MustDeclare(t *testing.T, cluster, id string, object map[string]any) string {
	t.Helper()
	declared, err := s.Declare(t, cluster, id, object)
	require.NoError(t, err)
	return declared
}

// Mint mints a registration token, for the record named or a new one, answering the token and record.
func (s *Target) Mint(t *testing.T, args ...string) (token, cluster string) {
	t.Helper()
	got := s.Fix(t, append([]string{"mint-token"}, args...)...)
	token, _ = got["registrationToken"].(string)
	cluster, _ = got["cluster"].(string)
	require.NotEmpty(t, token, "mint-token printed no registrationToken")
	return token, cluster
}

// Registration is a registration request (REG-1) for token and the public half of key.
func Registration(t *testing.T, token string, key *rsa.PrivateKey) map[string]any {
	t.Helper()
	return map[string]any{"registrationToken": token, "jwk": PublicJWK(t, key),
		"connector": map[string]any{"version": "conformance"}}
}

// Register sends a registration. A 429 spends nothing (REG-8), so it is waited out as T-6 says and sent
// again; any other answer is the answer.
func (s *Target) Register(t *testing.T, body map[string]any) Answer {
	t.Helper()
	for {
		got := s.Post(t, RegistrationsPath, "", body)
		if got.Status != http.StatusTooManyRequests {
			return got
		}
		time.Sleep(retryAfter(got.Header.Get("Retry-After")))
	}
}

func retryAfter(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds < 1 {
		return time.Second
	}
	return time.Duration(seconds) * time.Second
}

// Record is a registered cluster record and the key it signs with.
type Record struct {
	Cluster, ClientID, TokenURL string
	Key                         *rsa.PrivateKey
}

// Connect registers a new record under a fresh key and answers it with a token exchanged under it.
func (s *Target) Connect(t *testing.T) (Record, string) {
	t.Helper()
	token, _ := s.Mint(t)
	record := s.RegisterKey(t, token, Key(t))
	return record, s.Exchange(t, record, record.Key)
}

// RegisterKey registers key with token and requires success (REG-6).
func (s *Target) RegisterKey(t *testing.T, token string, key *rsa.PrivateKey) Record {
	t.Helper()
	got := s.Register(t, Registration(t, token, key))
	require.Equal(t, http.StatusCreated, got.Status, string(got.Body))
	body := got.JSON(t)
	record := Record{Key: key}
	record.Cluster, _ = body["cluster"].(string)
	record.ClientID, _ = body["clientId"].(string)
	record.TokenURL, _ = body["tokenUrl"].(string)
	require.NotEmpty(t, record.Cluster, "REG-6: cluster")
	require.NotEmpty(t, record.ClientID, "REG-6: clientId")
	require.NotEmpty(t, record.TokenURL, "REG-6: tokenUrl")
	return record
}

// Claims are a JWS's claims for record's client to audience, now, with a fresh jti.
func (r Record) Claims(audience string) map[string]any {
	now := time.Now().Unix()
	return map[string]any{"iss": r.ClientID, "sub": r.ClientID, "aud": audience, "iat": now, "exp": now + 60,
		"jti": rand.Text()}
}

// TryExchange exchanges a private_key_jwt assertion signed by key for a token (AUTH-1).
func (s *Target) TryExchange(t *testing.T, record Record, key *rsa.PrivateKey) Answer {
	t.Helper()
	form := url.Values{
		"grant_type":            {"client_credentials"},
		"client_id":             {record.ClientID},
		"client_assertion_type": {"urn:ietf:params:oauth:client-assertion-type:jwt-bearer"},
		"client_assertion":      {Signed(t, key, key, "JWT", record.Claims(record.TokenURL))},
	}
	return s.DoURL(t, http.MethodPost, record.TokenURL, "", "application/x-www-form-urlencoded",
		[]byte(form.Encode()))
}

// Exchange exchanges under key and requires a token.
func (s *Target) Exchange(t *testing.T, record Record, key *rsa.PrivateKey) string {
	t.Helper()
	got := s.TryExchange(t, record, key)
	require.Equal(t, http.StatusOK, got.Status, string(got.Body))
	token, _ := got.JSON(t)["access_token"].(string)
	require.NotEmpty(t, token, "the exchange answered no access_token")
	return token
}

// Poll reads the declarations after cursor, holding up to wait seconds.
func (s *Target) Poll(t *testing.T, bearer, after string, wait int) Answer {
	t.Helper()
	q := url.Values{"wait": {strconv.Itoa(wait)}}
	if after != "" {
		q.Set("after", after)
	}
	return s.Do(t, http.MethodGet, DeclarationsPath+"?"+q.Encode(), bearer, "", nil)
}

// Page is a DeclarationPage.
type Page struct {
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

// Page reads the answer as a DeclarationPage, requiring a 200.
func (a Answer) Page(t *testing.T) Page {
	t.Helper()
	require.Equal(t, http.StatusOK, a.Status, string(a.Body))
	var p Page
	require.NoError(t, json.Unmarshal(a.Body, &p))
	return p
}

// Ops are the page's changes as op:id.
func (p Page) Ops() []string {
	out := make([]string, 0, len(p.Changes))
	for _, c := range p.Changes {
		out = append(out, c.Op+":"+c.ID)
	}
	return out
}

// FullSet reads the full set, following more until it is complete (DECL-1, DECL-5), and answers its
// pages.
func (s *Target) FullSet(t *testing.T, bearer string) []Page {
	t.Helper()
	pages := []Page{s.Poll(t, bearer, "", 0).Page(t)}
	for pages[len(pages)-1].More {
		pages = append(pages, s.Poll(t, bearer, pages[len(pages)-1].Cursor, 0).Page(t))
	}
	return pages
}

// PublicJWK is key's public half as an RFC 7517 JWK.
func PublicJWK(t *testing.T, key *rsa.PrivateKey) map[string]any {
	t.Helper()
	encoded, err := json.Marshal(jose.JSONWebKey{Key: &key.PublicKey, Algorithm: "RS256", Use: "sig"})
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(encoded, &out))
	return out
}

// Kid is key's RFC 7638 thumbprint.
func Kid(t *testing.T, key *rsa.PrivateKey) string {
	t.Helper()
	sum, err := (&jose.JSONWebKey{Key: &key.PublicKey}).Thumbprint(crypto.SHA256)
	require.NoError(t, err)
	return base64.RawURLEncoding.EncodeToString(sum)
}

// Signed is a compact RS256 JWS over claims, signed by key, whose header names kidOf's thumbprint and typ.
func Signed(t *testing.T, key, kidOf *rsa.PrivateKey, typ string, claims map[string]any) string {
	t.Helper()
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: Kid(t, kidOf)}},
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

// keys holds RSA 3072 keys generated ahead of their use, since each takes most of a second.
var keys = make(chan *rsa.PrivateKey, 8)

func init() {
	for range 4 {
		go func() {
			for {
				key, err := rsa.GenerateKey(rand.Reader, 3072)
				if err != nil {
					panic(err)
				}
				keys <- key
			}
		}()
	}
}

// Key is a fresh RSA 3072 key, held by no record yet.
func Key(t *testing.T) *rsa.PrivateKey {
	t.Helper()
	return <-keys
}
