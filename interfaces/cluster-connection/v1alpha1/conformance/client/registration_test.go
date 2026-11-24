//go:build conformance

package client

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/garam-agent-operator/interfaces/cluster-connection/v1alpha1/reference"
)

func generatesKeySendsPublicHalf(t *testing.T) {
	h := newHarness(t)

	h.start()

	h.connected()
	registrations := h.sent(registrationsPath)
	require.Len(t, registrations, 1)
	sent := body(t, registrations[0])
	assert.NotEmpty(t, sent["registrationToken"], "REG-1")
	connector, _ := sent["connector"].(map[string]any)
	assert.NotEmpty(t, connector["version"], "REG-1: connector.version")
	jwk, _ := sent["jwk"].(map[string]any)
	for _, member := range []string{"d", "p", "q", "dp", "dq", "qi"} {
		assert.NotContains(t, jwk, member, "REG-2: no private member")
	}
	key, _ := publicKey(t, jwk)
	assert.GreaterOrEqual(t, key.N.BitLen(), 3072, "REG-1: an RSA key of at least 3072 bits")
	for _, s := range h.sent(tokenPath) {
		assert.NotContains(t, string(s.body), `"d":`, "REG-2: no private half in any later request")
	}
}

// refusedRegistration requires a client whose registration was answered as answerWith to send no other
// with the same token, and, as a control, to register once a new token is given.
func refusedRegistration(t *testing.T, answerWith func(w http.ResponseWriter)) {
	t.Helper()
	h := newHarness(t)
	h.setIntercept(once(registrationsPath, func(w http.ResponseWriter, _ *http.Request, _ http.Handler) {
		answerWith(w)
	}))
	h.start()
	require.Eventually(t, func() bool { return len(h.sent(registrationsPath)) == 1 }, eventually, tick)

	require.Never(t, func() bool { return len(h.sent(registrationsPath)) > 1 }, 15*time.Second, tick,
		"REG-7: the same token is not sent again")

	minted := h.fix("mint-token", reference.FixtureRequest{Cluster: h.record})
	h.writeToken(minted["registrationToken"].(string))
	h.connected()
	registrations := h.sent(registrationsPath)
	first, last := body(t, registrations[0]), body(t, registrations[len(registrations)-1])
	assert.NotEqual(t, first["registrationToken"], last["registrationToken"],
		"control: the new token is the one sent")
}

func noRetryAfterRefusal(t *testing.T) {
	refusedRegistration(t, func(w http.ResponseWriter) { answer(w, http.StatusUnauthorized, "registration_refused") })
}

func noRetryAfterNoAnswer(t *testing.T) {
	refusedRegistration(t, drop)
}

func retriesAfter429(t *testing.T) {
	h := newHarness(t)
	h.setIntercept(once(registrationsPath, func(w http.ResponseWriter, _ *http.Request, _ http.Handler) {
		w.Header().Set("Retry-After", "3")
		answer(w, http.StatusTooManyRequests, "throttled")
	}))

	h.start()

	h.connected()
	registrations := h.sent(registrationsPath)
	require.Len(t, registrations, 2, "REG-7: sent again after the 429, and once taken, not again")
	assert.GreaterOrEqual(t, registrations[1].at.Sub(registrations[0].at), 3*time.Second, "T-6: Retry-After waited")
	assert.Equal(t, body(t, registrations[0])["registrationToken"], body(t, registrations[1])["registrationToken"],
		"REG-7: the 429 spent nothing, so the same token is sent")
}
