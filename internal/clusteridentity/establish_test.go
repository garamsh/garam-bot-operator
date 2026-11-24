package clusteridentity_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/garam-agent-operator/internal/clusteridentity"
)

func TestEstablish_RegistersWithAKeyItGenerated(t *testing.T) {
	h := newHarness(t)
	_, grn := h.mint()

	connection, err := h.service().Establish(context.Background())

	require.NoError(t, err)
	assert.Equal(t, clusteridentity.GRN(grn), connection.Cluster())
	registrations := h.sent(regPath)
	require.Len(t, registrations, 1)
	var sent struct {
		JWK       map[string]any    `json:"jwk"`
		Connector map[string]string `json:"connector"`
	}
	require.NoError(t, json.Unmarshal(registrations[0].body, &sent))
	for _, private := range []string{"d", "p", "q", "dp", "dq", "qi"} {
		assert.NotContains(t, sent.JWK, private, "REG-2: the private half is never sent")
	}
	assert.Equal(t, "test", sent.Connector["version"])
	stored := h.store.last()
	assert.Same(t, testKeys()[0], stored.Key, "the key generated is the key stored")
	assert.Equal(t, clusteridentity.GRN(grn), stored.Record.Cluster)
	assert.Equal(t, http.StatusOK, poll(t, connection.Client(), h.base), "the registered key authenticates")
	assert.Equal(t, []string{kid(t, testKeys()[0])}, h.exchangeKIDs())
}

func TestEstablish_StoresTheKeyBeforeSendingIt(t *testing.T) {
	h := newHarness(t)
	h.mint()
	var storedWhenSent clusteridentity.State
	h.setIntercept(func(_ http.ResponseWriter, r *http.Request, _ []byte, _ http.Handler) bool {
		if r.URL.Path == regPath {
			storedWhenSent = h.store.last()
		}
		return false
	})

	_, err := h.service().Establish(context.Background())

	require.NoError(t, err)
	assert.Same(t, testKeys()[0], storedWhenSent.Key)
	assert.NotEmpty(t, storedWhenSent.Attempted, "the attempt is recorded before the token is sent")
}

func TestEstablish_DoesNotPresentATokenAgain(t *testing.T) {
	for name, fail := range map[string]func(t *testing.T, w http.ResponseWriter){
		"after a refusal": func(_ *testing.T, w http.ResponseWriter) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"error":"registration_refused","message":"refused"}`))
		},
		"after a 5xx":     func(_ *testing.T, w http.ResponseWriter) { w.WriteHeader(http.StatusBadGateway) },
		"after no answer": func(t *testing.T, w http.ResponseWriter) { drop(t, w) },
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.mint()
			first := true
			h.setIntercept(func(w http.ResponseWriter, r *http.Request, _ []byte, _ http.Handler) bool {
				if r.URL.Path != regPath || !first {
					return false
				}
				first = false
				fail(t, w)
				return true
			})
			polled := false
			h.onSleep = func(n int, _ time.Duration) error {
				if n == 3 {
					polled = true
					assert.Len(t, h.sent(regPath), 1, "three polls later the same token was not presented again")
					h.mint()
				}
				return nil
			}

			connection, err := h.service().Establish(context.Background())

			require.NoError(t, err)
			require.True(t, polled, "it waited for a new token rather than presenting the same one")
			assert.Len(t, h.sent(regPath), 2, "control: a new token is presented once")
			assert.Equal(t, http.StatusOK, poll(t, connection.Client(), h.base))
			assert.NotContains(t, h.logged(), registrationToken(t, h.sent(regPath)[0].body), "the token is never logged")
		})
	}
}

func TestEstablish_DoesNotPresentTheAttemptedTokenAfterARestart(t *testing.T) {
	h := newHarness(t)
	h.mint()
	h.setIntercept(func(w http.ResponseWriter, r *http.Request, _ []byte, _ http.Handler) bool {
		if r.URL.Path == regPath {
			drop(t, w)
			return true
		}
		return false
	})
	stop := errors.New("stop")
	h.onSleep = func(int, time.Duration) error { return stop }
	_, err := h.service().Establish(context.Background())
	require.ErrorIs(t, err, stop, "setup: the first process stops while waiting for a new token")
	require.Len(t, h.sent(regPath), 1)

	h.onSleep = func(n int, _ time.Duration) error {
		if n == 3 {
			return stop
		}
		return nil
	}
	_, err = h.service().Establish(context.Background())

	require.ErrorIs(t, err, stop)
	assert.Len(t, h.sent(regPath), 1, "the restarted process does not present the token the first one sent")
}

func TestEstablish_PresentsTheTokenAgainAfter429(t *testing.T) {
	h := newHarness(t)
	h.mint()
	first := true
	h.setIntercept(func(w http.ResponseWriter, r *http.Request, _ []byte, _ http.Handler) bool {
		if r.URL.Path != regPath || !first {
			return false
		}
		first = false
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		return true
	})

	connection, err := h.service().Establish(context.Background())

	require.NoError(t, err)
	registrations := h.sent(regPath)
	require.Len(t, registrations, 2)
	assert.Equal(t, registrationToken(t, registrations[0].body), registrationToken(t, registrations[1].body))
	assert.Equal(t, 7*time.Second, h.sleeps[0], "T-6: at least the Retry-After it was sent")
	assert.Equal(t, http.StatusOK, poll(t, connection.Client(), h.base))
}

func TestEstablish_LoadsAStoredIdentityWithoutRegistering(t *testing.T) {
	h := newHarness(t)
	h.establish()
	before := len(h.sent(regPath))

	connection, err := h.service().Establish(context.Background())

	require.NoError(t, err)
	assert.Len(t, h.sent(regPath), before, "no registration")
	assert.Equal(t, http.StatusOK, poll(t, connection.Client(), h.base))
}

func registrationToken(t *testing.T, body []byte) string {
	t.Helper()
	var sent struct {
		Token string `json:"registrationToken"`
	}
	require.NoError(t, json.Unmarshal(body, &sent))
	return sent.Token
}
