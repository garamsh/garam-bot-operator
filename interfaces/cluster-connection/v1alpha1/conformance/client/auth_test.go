//go:build conformance

package client

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// registeredKey is the public half and thumbprint of the key the client registered.
func (h *harness) registeredKey(t *testing.T) (key any, kid string) {
	t.Helper()
	registrations := h.sent(registrationsPath)
	require.NotEmpty(t, registrations)
	jwk := body(t, registrations[len(registrations)-1])["jwk"]
	_, kid = publicKey(t, jwk)
	return jwk, kid
}

func reExchangesOnceOn401(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.connected()
	jwk, kid := h.registeredKey(t)
	key, _ := publicKey(t, jwk)
	exchanges := h.sent(tokenPath)
	require.NotEmpty(t, exchanges)
	signed := verify(t, assertion(t, exchanges[0]), key)
	assert.Equal(t, kid, signed.kid, "AUTH-1: kid is the live key's thumbprint")
	assert.Equal(t, string(h.record), signed.claims["iss"], "AUTH-1: iss is clientId")
	assert.Equal(t, string(h.record), signed.claims["sub"], "AUTH-1: sub is clientId")
	assert.Equal(t, h.base+tokenPath, signed.claims["aud"], "AUTH-1: aud is tokenUrl")
	for _, s := range h.after(time.Time{}) {
		if s.path != registrationsPath && s.path != tokenPath {
			assert.NotEmpty(t, s.bearer, "AUTH-1: %s %s carries a bearer token", s.method, s.path)
		}
	}
	refusedAt := time.Now()
	var refused string
	h.setIntercept(once(declarationsPath, func(w http.ResponseWriter, r *http.Request, _ http.Handler) {
		refused = r.Header.Get("Authorization")
		answer(w, http.StatusUnauthorized, "unauthorized")
	}))
	h.wake()

	require.Eventually(t, func() bool {
		return len(declarationsAfter(h, refusedAt)) >= 2
	}, eventually, tick, "AUTH-3: the request is repeated")

	later := h.after(refusedAt)
	first, second := indexOf(later, declarationsPath, 0), indexOf(later, declarationsPath, 1)
	between := 0
	for _, s := range later[first+1 : second] {
		if s.path == tokenPath {
			between++
		}
	}
	assert.Equal(t, 1, between, "AUTH-3: one exchange, then the request once more")
	assert.NotEqual(t, refused, "Bearer "+later[second].bearer, "the repeat carries the new token")
}

func declarationsAfter(h *harness, at time.Time) []seen {
	var out []seen
	for _, s := range h.after(at) {
		if s.path == declarationsPath {
			out = append(out, s)
		}
	}
	return out
}

// indexOf is the index in requests of the nth request to path.
func indexOf(requests []seen, path string, n int) int {
	for i, s := range requests {
		if s.path == path {
			if n == 0 {
				return i
			}
			n--
		}
	}
	return -1
}
