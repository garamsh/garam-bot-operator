package clusteridentity_test

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/garam-agent-operator/internal/clusteridentity"
)

// rotationSent is a rotation request as the server was sent it.
type rotationSent struct {
	JWK           map[string]any `json:"jwk"`
	Authorization string         `json:"authorization"`
	Proof         string         `json:"proof"`
}

func (h *harness) rotations() []rotationSent {
	sent := h.sent(keysPath)
	out := make([]rotationSent, 0, len(sent))
	for _, s := range sent {
		var r rotationSent
		require.NoError(h.t, json.Unmarshal(s.body, &r))
		out = append(out, r)
	}
	return out
}

// claims reads a compact JWS's claims, without verifying it.
func claims(t *testing.T, compact string) map[string]any {
	t.Helper()
	parts := strings.Split(compact, ".")
	require.Len(t, parts, 3)
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	require.NoError(t, err)
	var out map[string]any
	require.NoError(t, json.Unmarshal(raw, &out))
	return out
}

func TestClient_ExchangesOnceMoreOn401(t *testing.T) {
	h := newHarness(t)
	connection := h.establish()
	require.Equal(t, http.StatusOK, poll(t, connection.Client(), h.base), "setup: a token is held")
	refusedOnce := false
	h.setIntercept(func(w http.ResponseWriter, r *http.Request, _ []byte, _ http.Handler) bool {
		if r.URL.Path != declPath || refusedOnce {
			return false
		}
		refusedOnce = true
		w.WriteHeader(http.StatusUnauthorized)
		return true
	})
	exchanges := len(h.sent(tokenPath))

	status := poll(t, connection.Client(), h.base)

	assert.Equal(t, http.StatusOK, status, "the request is repeated once under a fresh token")
	assert.Len(t, h.sent(tokenPath), exchanges+1, "AUTH-3: one exchange more")
}

func TestRotate_SignsTheAuthorizationWithTheCurrentKeyAndThenSignsWithTheSuccessor(t *testing.T) {
	h := newHarness(t)
	connection := h.establish()
	current, successor := testKeys()[0], testKeys()[1]

	require.NoError(t, connection.Rotate(context.Background()))

	sent := h.rotations()
	require.Len(t, sent, 1)
	assert.Equal(t, kid(t, current), headerKID(t, sent[0].Authorization), "ROT-1: the authorization is the current key's")
	assert.Equal(t, kid(t, successor), claims(t, sent[0].Authorization)["jkt"])
	assert.Equal(t, kid(t, successor), headerKID(t, sent[0].Proof))
	assert.Same(t, successor, h.store.last().Key)
	assert.Nil(t, h.store.last().Successor)
	require.Equal(t, http.StatusOK, poll(t, connection.Client(), h.base))
	kids := h.exchangeKIDs()
	assert.Equal(t, kid(t, successor), kids[len(kids)-1], "ROT-5: once answered, it signs with the successor")
}

func TestRotate_StoresTheSuccessorBeforeSendingIt(t *testing.T) {
	h := newHarness(t)
	connection := h.establish()
	var storedWhenSent clusteridentity.State
	h.setIntercept(func(_ http.ResponseWriter, r *http.Request, _ []byte, _ http.Handler) bool {
		if r.URL.Path == keysPath {
			storedWhenSent = h.store.last()
		}
		return false
	})

	require.NoError(t, connection.Rotate(context.Background()))

	require.NotNil(t, storedWhenSent.Successor)
	assert.Same(t, testKeys()[1], storedWhenSent.Successor)
	assert.Same(t, testKeys()[0], storedWhenSent.Key)
}

func TestRotate_TriesTheSuccessorAfterALostAnswer(t *testing.T) {
	h := newHarness(t)
	connection := h.establish()
	h.setIntercept(func(w http.ResponseWriter, r *http.Request, _ []byte, next http.Handler) bool {
		if r.URL.Path == keysPath {
			answerThenDrop(t, w, r, next)
			return true
		}
		return false
	})

	require.NoError(t, connection.Rotate(context.Background()))

	assert.Len(t, h.rotations(), 1, "the exchange with the successor proved the rotation; none was sent again")
	kids := h.exchangeKIDs()
	assert.Equal(t, kid(t, testKeys()[1]), kids[len(kids)-1])
	assert.Same(t, testKeys()[1], h.store.last().Key)
}

func TestRotate_RepeatsWithFreshJWSsWhenTheLostRotationDidNotTakePlace(t *testing.T) {
	h := newHarness(t)
	connection := h.establish()
	dropped := false
	h.setIntercept(func(w http.ResponseWriter, r *http.Request, _ []byte, _ http.Handler) bool {
		if r.URL.Path != keysPath || dropped {
			return false
		}
		dropped = true
		drop(t, w)
		return true
	})

	require.NoError(t, connection.Rotate(context.Background()))

	sent := h.rotations()
	require.Len(t, sent, 2)
	assert.NotEqual(t, claims(t, sent[0].Authorization)["jti"], claims(t, sent[1].Authorization)["jti"])
	assert.NotEqual(t, claims(t, sent[0].Proof)["jti"], claims(t, sent[1].Proof)["jti"])
	assert.Equal(t, sent[0].JWK, sent[1].JWK, "the same stored successor is sent again")
	kids := h.exchangeKIDs()
	assert.Contains(t, kids, kid(t, testKeys()[1]), "the successor was tried at the exchange before the repeat")
	assert.Equal(t, time.Second, h.sleeps[len(h.sleeps)-1], "T-6: at least 1 second before the same request")
	assert.Same(t, testKeys()[1], h.store.last().Key)
}

func TestRotate_BacksOffA401TheReexchangeDidNotCure(t *testing.T) {
	h := newHarness(t)
	connection := h.establish()
	refusals := 0
	h.setIntercept(func(w http.ResponseWriter, r *http.Request, _ []byte, _ http.Handler) bool {
		// The first send and its one repeat under a fresh token are both refused.
		if r.URL.Path != keysPath || refusals == 2 {
			return false
		}
		refusals++
		w.WriteHeader(http.StatusUnauthorized)
		return true
	})

	require.NoError(t, connection.Rotate(context.Background()))

	assert.Len(t, h.rotations(), 3, "AUTH-3's one repeat, then the send after the back-off")
	assert.Contains(t, h.sleeps, time.Second, "T-6: backed off, not abandoned")
	assert.Same(t, testKeys()[1], h.store.last().Key)
}

func TestRotate_After409(t *testing.T) {
	t.Run("an exchange with the successor proves the rotation took place", func(t *testing.T) {
		h := newHarness(t)
		connection := h.establish()
		h.setIntercept(func(w http.ResponseWriter, r *http.Request, _ []byte, next http.Handler) bool {
			if r.URL.Path != keysPath {
				return false
			}
			next.ServeHTTP(&discard{header: http.Header{}}, r)
			w.WriteHeader(http.StatusConflict)
			return true
		})

		require.NoError(t, connection.Rotate(context.Background()))

		assert.Same(t, testKeys()[1], h.store.last().Key)
	})
	t.Run("a failed exchange keeps the predecessor", func(t *testing.T) {
		h := newHarness(t)
		connection := h.establish()
		h.setIntercept(func(w http.ResponseWriter, r *http.Request, _ []byte, _ http.Handler) bool {
			if r.URL.Path != keysPath {
				return false
			}
			w.WriteHeader(http.StatusConflict)
			return true
		})

		err := connection.Rotate(context.Background())

		require.ErrorIs(t, err, clusteridentity.ErrTooManyKeys)
		kids := h.exchangeKIDs()
		assert.Equal(t, kid(t, testKeys()[1]), kids[len(kids)-1], "ROT-5: the successor was tried first")
		assert.Same(t, testKeys()[0], h.store.last().Key)
		assert.Nil(t, h.store.last().Successor)
		// A 401 forces the next exchange, to show which key it signs with.
		refusedOnce := false
		h.setIntercept(func(w http.ResponseWriter, r *http.Request, _ []byte, _ http.Handler) bool {
			if r.URL.Path != declPath || refusedOnce {
				return false
			}
			refusedOnce = true
			w.WriteHeader(http.StatusUnauthorized)
			return true
		})
		require.Equal(t, http.StatusOK, poll(t, connection.Client(), h.base))
		kids = h.exchangeKIDs()
		assert.Equal(t, kid(t, testKeys()[0]), kids[len(kids)-1], "the predecessor goes on signing")
	})
	t.Run("an exchange that failed without a refusal is backed off, and the successor kept", func(t *testing.T) {
		h := newHarness(t)
		connection := h.establish()
		successor := kid(t, testKeys()[1])
		unavailable := false
		h.setIntercept(func(w http.ResponseWriter, r *http.Request, body []byte, _ http.Handler) bool {
			switch r.URL.Path {
			case keysPath:
				w.WriteHeader(http.StatusConflict)
				return true
			case tokenPath:
				values, _ := urlValues(body)
				if headerKID(t, values.Get("client_assertion")) == successor && !unavailable {
					unavailable = true
					w.WriteHeader(http.StatusServiceUnavailable)
					return true
				}
			}
			return false
		})

		err := connection.Rotate(context.Background())

		require.ErrorIs(t, err, clusteridentity.ErrTooManyKeys, "the second exchange was refused")
		assert.Len(t, h.rotations(), 2, "a 503 at the exchange shows nothing, so the rotation was sent again")
		assert.Equal(t, time.Second, h.sleeps[len(h.sleeps)-1], "T-6: after 1 second")
	})
}

func TestRotate_After409KeepsTheSuccessorWhenItsExchangeIsRefusedOtherwise(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusForbidden} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			h := newHarness(t)
			connection := h.establish()
			successor := kid(t, testKeys()[1])
			h.setIntercept(func(w http.ResponseWriter, r *http.Request, body []byte, _ http.Handler) bool {
				switch r.URL.Path {
				case keysPath:
					w.WriteHeader(http.StatusConflict)
					return true
				case tokenPath:
					if values, _ := urlValues(body); headerKID(t, values.Get("client_assertion")) == successor {
						w.Header().Set("Content-Type", "application/json")
						w.WriteHeader(status)
						_, _ = w.Write([]byte(`{"error":"invalid_request"}`))
						return true
					}
				}
				return false
			})
			stop := errors.New("stop")
			h.onSleep = func(int, time.Duration) error { return stop }

			err := connection.Rotate(context.Background())

			require.ErrorIs(t, err, stop, "backed off, not abandoned")
			require.NotNil(t, h.store.last().Successor, "a successor that may be live stays stored")
			assert.Same(t, testKeys()[1], h.store.last().Successor)
			assert.Same(t, testKeys()[0], h.store.last().Key)
		})
	}
}

func TestRotate_GivesUpASendLeftUnansweredAndSendsItAgain(t *testing.T) {
	h := newHarness(t)
	h.requestTimeout = 500 * time.Millisecond
	connection := h.establish()
	hung := false
	h.setIntercept(func(_ http.ResponseWriter, r *http.Request, _ []byte, _ http.Handler) bool {
		if r.URL.Path != keysPath || hung {
			return false
		}
		hung = true
		<-r.Context().Done()
		return true
	})
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()

	require.NoError(t, connection.Rotate(ctx))

	assert.Len(t, h.rotations(), 2, "the send held open was given up within RequestTimeout")
	assert.Same(t, testKeys()[1], h.store.last().Key)
}

func TestRotate_BacksOffFrom1SecondDoublingTo60(t *testing.T) {
	h := newHarness(t)
	connection := h.establish()
	h.setIntercept(func(w http.ResponseWriter, r *http.Request, _ []byte, _ http.Handler) bool {
		if r.URL.Path != keysPath {
			return false
		}
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	})
	waited := len(h.sleeps)
	stop := errors.New("stop")
	h.onSleep = func(n int, _ time.Duration) error {
		if n == waited+9 {
			return stop
		}
		return nil
	}

	require.ErrorIs(t, connection.Rotate(context.Background()), stop)

	s := time.Second
	assert.Equal(t, []time.Duration{s, 2 * s, 4 * s, 8 * s, 16 * s, 32 * s, 60 * s, 60 * s, 60 * s}, h.sleeps[waited:])
}

// failingReader is a Random that cannot be read.
type failingReader struct{}

func (failingReader) Read([]byte) (int, error) { return 0, errors.New("no randomness") }

func TestRotate_AnswersAJWSItCannotSignWithoutSendingOrWaiting(t *testing.T) {
	h := newHarness(t)
	h.random = failingReader{}
	connection := h.establish()
	waited := len(h.sleeps)

	err := connection.Rotate(context.Background())

	require.ErrorContains(t, err, "sign the rotation's authorization")
	assert.Empty(t, h.rotations())
	assert.Len(t, h.sleeps, waited, "a local failure is not retried as if garam had not answered")
}

func TestRotate_RunsOneRotationAtATime(t *testing.T) {
	h := newHarness(t)
	connection := h.establish()
	held, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	h.setIntercept(func(_ http.ResponseWriter, r *http.Request, _ []byte, _ http.Handler) bool {
		if r.URL.Path == keysPath {
			once.Do(func() {
				close(held)
				<-release
			})
		}
		return false
	})
	first, second := make(chan error, 1), make(chan error, 1)
	go func() { first <- connection.Rotate(context.Background()) }()
	<-held

	go func() { second <- connection.Rotate(context.Background()) }()

	assert.Never(t, func() bool { return len(h.sent(keysPath)) > 1 }, 300*time.Millisecond, 10*time.Millisecond,
		"the second rotation waits for the first")
	close(release)
	require.NoError(t, <-first)
	// The second rotates from the successor while the predecessor is in its rollover: garam holds two
	// live keys already.
	require.ErrorIs(t, <-second, clusteridentity.ErrTooManyKeys)
}

func TestRun_StopsWhenAnotherProcessWroteTheIdentity(t *testing.T) {
	h := newHarness(t)
	connection := h.establish()
	h.store.fail = func(state clusteridentity.State) error {
		if state.Successor != nil {
			return clusteridentity.ErrAnotherWriter
		}
		return nil
	}

	err := connection.Run(context.Background())

	require.ErrorIs(t, err, clusteridentity.ErrAnotherWriter)
	assert.Empty(t, h.rotations(), "nothing is sent over a store another process wrote")
}

func TestRun_ResolvesARotationARestartInterruptedBeforeAnythingElse(t *testing.T) {
	h := newHarness(t)
	connection := h.establish()
	successor := kid(t, testKeys()[1])
	h.setIntercept(func(w http.ResponseWriter, r *http.Request, body []byte, next http.Handler) bool {
		switch r.URL.Path {
		case keysPath:
			answerThenDrop(t, w, r, next)
			return true
		case tokenPath:
			if values, _ := urlValues(body); headerKID(t, values.Get("client_assertion")) == successor {
				w.WriteHeader(http.StatusBadGateway)
				return true
			}
		}
		return false
	})
	stop := errors.New("the process stops")
	h.onSleep = func(int, time.Duration) error { return stop }
	require.ErrorIs(t, connection.Rotate(context.Background()), stop)
	require.NotNil(t, h.store.last().Successor, "setup: the successor is stored, its rotation unresolved")
	h.setIntercept(nil)
	h.onSleep = nil

	exchanges := len(h.sent(tokenPath))

	restarted, err := h.service().Establish(context.Background())

	require.NoError(t, err)
	assert.Len(t, h.sent(tokenPath), exchanges, "Establish leaves the rotation to Run: the current key is live")
	waited := len(h.sleeps)
	h.onSleep = func(int, time.Duration) error { return stop }
	require.ErrorIs(t, restarted.Run(context.Background()), stop)
	assert.Len(t, h.rotations(), 1, "the exchange with the successor came before anything else")
	assert.Same(t, testKeys()[1], h.store.last().Key, "resolved at once, not at the next due date")
	assert.Equal(t, []time.Duration{30 * 24 * time.Hour}, h.sleeps[waited:], "then the next rotation is waited for")
	assert.Equal(t, http.StatusOK, poll(t, restarted.Client(), h.base))
}

func TestRun_RotatesEvery30Days(t *testing.T) {
	h := newHarness(t)
	connection := h.establish()
	stop := errors.New("stop")
	h.onSleep = func(n int, _ time.Duration) error {
		if n == 2 {
			return stop
		}
		return nil
	}

	err := connection.Run(context.Background())

	require.ErrorIs(t, err, stop)
	require.Len(t, h.sleeps, 2)
	assert.Equal(t, []time.Duration{30 * 24 * time.Hour, 30 * 24 * time.Hour}, h.sleeps)
	assert.Len(t, h.rotations(), 1)
	assert.Same(t, testKeys()[1], h.store.last().Key)
}

func TestRun_RotatesEveryConfiguredPeriod(t *testing.T) {
	h := newHarness(t)
	h.rotationPeriod = 90 * time.Minute
	connection := h.establish()
	stop := errors.New("stop")
	h.onSleep = func(n int, _ time.Duration) error {
		if n == 2 {
			return stop
		}
		return nil
	}

	err := connection.Run(context.Background())

	require.ErrorIs(t, err, stop)
	assert.Equal(t, []time.Duration{90 * time.Minute, 90 * time.Minute}, h.sleeps, "ADR 0094: the period configured")
	assert.Len(t, h.rotations(), 1)
}

func TestIdentity_IsNeverLoggedNorWrittenToDisk(t *testing.T) {
	h := newHarness(t)
	scratch := t.TempDir()
	t.Setenv("TMPDIR", scratch)
	token, _ := h.mint()
	connection, err := h.service().Establish(context.Background())
	require.NoError(t, err)
	require.NoError(t, connection.Rotate(context.Background()))

	logged := h.logged()
	assert.NotContains(t, logged, token)
	assert.NotContains(t, logged, "PRIVATE KEY")
	for _, key := range testKeys()[:2] {
		assert.NotContains(t, logged, key.D.String())
		assert.NotContains(t, logged, base64.RawURLEncoding.EncodeToString(key.D.Bytes()))
	}
	assert.Contains(t, logged, "registered", "control: the log was captured")
	written, err := os.ReadDir(scratch)
	require.NoError(t, err)
	assert.Empty(t, written, "nothing was written to the temporary directory")
	inDir, err := os.ReadDir(h.dir)
	require.NoError(t, err)
	require.Len(t, inDir, 1)
	assert.Equal(t, "registration-token", inDir[0].Name(), "the only file is the token file the test wrote")
}

// discard is a ResponseWriter that keeps nothing.
type discard struct{ header http.Header }

func (d *discard) Header() http.Header         { return d.header }
func (d *discard) Write(p []byte) (int, error) { return len(p), nil }
func (d *discard) WriteHeader(int)             {}
