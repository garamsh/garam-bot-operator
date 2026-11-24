package reference_test

import (
	"crypto/rand"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/garam-agent-operator/interfaces/cluster-connection/v1alpha1/reference"
)

// protectedRoutes are the routes AUTH-1 puts behind a bearer token, each with a body it accepts.
func protectedRoutes(h *harness) map[string]func(bearer string) answer {
	return map[string]func(string) answer{
		"declarations": func(bearer string) answer { return h.poll(bearer, "", 0) },
		"reports": func(bearer string) answer {
			return h.post(repPath, bearer, report(h.clock.Now(), nil, nil))
		},
	}
}

func TestAuth_RefusesMissingToken(t *testing.T) {
	h := newHarness(t)
	grn := h.register(testKeys()[0])
	bearer := h.exchange(grn, testKeys()[0])
	for name, call := range protectedRoutes(h) {
		t.Run(name, func(t *testing.T) {
			require.Less(t, call(bearer).status, 300, "control: the token is accepted")
			missing := call("")
			assert.Equal(t, http.StatusUnauthorized, missing.status)
			assert.Equal(t, "unauthorized", missing.errorCode(t))
			assert.Equal(t, http.StatusUnauthorized, call("not-a-token").status)
		})
	}
	unauthorizedRotation := h.post(keysPath, "", map[string]any{})
	assert.Equal(t, http.StatusUnauthorized, unauthorizedRotation.status)
}

// TestAuth_RefusesMissingTokenBeforeReadingTheRequest holds that a request with no token is 401 even
// where what it carries would be refused 400 under a live token.
func TestAuth_RefusesMissingTokenBeforeReadingTheRequest(t *testing.T) {
	h := newHarness(t)
	_, bearer := h.connected()
	cases := map[string]func(bearer string) answer{
		"declarations with wait over 50": func(bearer string) answer {
			return h.do(http.MethodGet, declPath+"?wait=51", bearer, "", nil)
		},
		"rotation with a body that is not JSON": func(bearer string) answer {
			return h.do(http.MethodPost, keysPath, bearer, "application/json", []byte("{"))
		},
	}
	for name, call := range cases {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, http.StatusBadRequest, call(bearer).status, "control: under a live token it is a 400")
			missing := call("")
			assert.Equal(t, http.StatusUnauthorized, missing.status)
			assert.Equal(t, "unauthorized", missing.errorCode(t))
		})
	}
}

func TestAuth_RefusesAfterClusterDeleted(t *testing.T) {
	h := newHarness(t)
	grn := h.register(testKeys()[0])
	bearer := h.exchange(grn, testKeys()[0])
	require.Equal(t, http.StatusOK, h.poll(bearer, "", 0).status, "control: live before the deletion")

	h.fix("delete-cluster", reference.FixtureRequest{Cluster: grn})

	assert.Equal(t, http.StatusUnauthorized, h.poll(bearer, "", 0).status)
	assert.Equal(t, http.StatusUnauthorized,
		h.tryExchange(signed(t, testKeys()[0], testKeys()[0], "JWT", h.claims(grn, h.base+"/oauth/token")), grn).status)
}

func TestAuth_RefusesAfterKeyRevoked(t *testing.T) {
	h := newHarness(t)
	grn := h.register(testKeys()[0])
	bearer := h.exchange(grn, testKeys()[0])
	require.Equal(t, http.StatusOK, h.poll(bearer, "", 0).status, "control: live before the revocation")

	h.fix("revoke-keys", reference.FixtureRequest{Cluster: grn})

	assert.Equal(t, http.StatusUnauthorized, h.poll(bearer, "", 0).status)
}

func TestAuth_RefusesExpiredToken(t *testing.T) {
	h := newHarness(t)
	grn := h.register(testKeys()[0])
	bearer := h.exchange(grn, testKeys()[0])

	h.clock.advance(time.Hour - time.Second)
	live := h.poll(bearer, "", 0)
	h.clock.advance(time.Second)
	expired := h.poll(bearer, "", 0)

	assert.Equal(t, http.StatusOK, live.status)
	assert.Equal(t, http.StatusUnauthorized, expired.status)
}

func TestAuth_HeldPollAnswers401WhenClusterDeleted(t *testing.T) {
	h := newHarness(t)
	grn := h.register(testKeys()[0])
	bearer := h.exchange(grn, testKeys()[0])
	cursor := h.poll(bearer, "", 0).page(t).Cursor

	done := make(chan answer, 1)
	go func() { done <- h.poll(bearer, cursor, 10) }()
	time.Sleep(200 * time.Millisecond)
	h.fix("delete-cluster", reference.FixtureRequest{Cluster: grn})

	select {
	case got := <-done:
		assert.Equal(t, http.StatusUnauthorized, got.status)
	case <-time.After(2 * time.Second):
		t.Fatal("the held poll was not answered within 2 seconds of the deletion")
	}
}

func TestExchange_AnswersMachineTokenV2(t *testing.T) {
	h := newHarness(t)
	grn := h.register(testKeys()[0])

	got := h.tryExchange(signed(t, testKeys()[0], testKeys()[0], "JWT", h.claims(grn, h.base+"/oauth/token")), grn)

	require.Equal(t, http.StatusOK, got.status, string(got.body))
	assert.Equal(t, "machine-token.v2", got.header.Get("Garam-Contract-Version"))
	body := got.json(t)
	assert.Equal(t, "Bearer", body["token_type"])
	assert.EqualValues(t, 3600, body["expires_in"])
	assert.NotEmpty(t, body["access_token"])
}

// panicking reads crypto/rand until it is armed, and panics after.
type panicking struct{ armed atomic.Bool }

func (p *panicking) Read(b []byte) (int, error) {
	if p.armed.Load() {
		panic("the random source failed")
	}
	return rand.Read(b)
}

func TestExchange_AnswersServerErrorOnPanic(t *testing.T) {
	source := &panicking{}
	h := newHarness(t, random(source))
	grn := h.register(testKeys()[0])
	source.armed.Store(true)

	got := h.tryExchange(signed(t, testKeys()[0], testKeys()[0], "JWT", h.claims(grn, h.base+"/oauth/token")), grn)

	assert.Equal(t, http.StatusInternalServerError, got.status)
	assert.Equal(t, "server_error", got.json(t)["error"])
}

func TestExchange_RefusesAssertionThatDoesNotHold(t *testing.T) {
	h := newHarness(t)
	key, other := testKeys()[0], testKeys()[1]
	grn := h.register(key)
	tokenURL := h.base + "/oauth/token"
	replayed := h.claims(grn, tokenURL)
	require.Equal(t, http.StatusOK, h.tryExchange(signed(t, key, key, "JWT", replayed), grn).status,
		"control: an assertion that holds is exchanged")

	cases := map[string]func() string{
		"jti seen before": func() string { return signed(t, key, key, "JWT", replayed) },
		"signed by a key the record does not hold": func() string {
			return signed(t, other, other, "JWT", h.claims(grn, tokenURL))
		},
		"kid of the record's key, signed by another": func() string {
			return signed(t, other, key, "JWT", h.claims(grn, tokenURL))
		},
		"another audience": func() string { return signed(t, key, key, "JWT", h.claims(grn, h.base+"/elsewhere")) },
		"another issuer": func() string {
			c := h.claims(grn, tokenURL)
			c["iss"] = "grn:reference/iam/cluster/other"
			return signed(t, key, key, "JWT", c)
		},
		"expired": func() string {
			c := h.claims(grn, tokenURL)
			c["iat"], c["exp"] = h.clock.Now().Unix()-120, h.clock.Now().Unix()-60
			return signed(t, key, key, "JWT", c)
		},
		"lives over 60 seconds": func() string {
			c := h.claims(grn, tokenURL)
			c["exp"] = h.clock.Now().Unix() + 61
			return signed(t, key, key, "JWT", c)
		},
	}
	for name, assertion := range cases {
		t.Run(name, func(t *testing.T) {
			got := h.tryExchange(assertion(), grn)
			assert.Equal(t, http.StatusUnauthorized, got.status, string(got.body))
			assert.Equal(t, "invalid_client", got.json(t)["error"])
		})
	}
}
