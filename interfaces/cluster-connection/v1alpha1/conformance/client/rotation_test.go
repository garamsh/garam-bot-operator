//go:build conformance

package client

import (
	"crypto/rsa"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// rotationPeriod is the period the rotation tests give the client: long enough for it to connect first,
// short enough to rotate within a test.
const rotationPeriod = "4s"

// rotating starts the client with a short rotation period, ending t as pending where its command line
// cannot set one (ADR 0094), and answers the client.
func (h *harness) rotating(t *testing.T) *process {
	t.Helper()
	supports(t, "rotation-period", "a rotation within a run")
	return h.start(rotatingEvery(rotationPeriod))
}

// firstRotation waits for the client's first rotation request and answers it.
func (h *harness) firstRotation(t *testing.T) seen {
	t.Helper()
	require.Eventually(t, func() bool { return len(h.sent(keysPath)) > 0 }, eventually, tick, "the client rotated")
	return h.sent(keysPath)[0]
}

// successorKid is the thumbprint of the successor a rotation request names.
func successorKid(t *testing.T, rotation seen) string {
	t.Helper()
	_, kid := publicKey(t, body(t, rotation)["jwk"])
	return kid
}

// nextExchange waits for the first exchange after at, and answers the kid its assertion names.
func (h *harness) nextExchange(t *testing.T, at time.Time) (seen, string) {
	t.Helper()
	var exchange seen
	require.Eventually(t, func() bool {
		for _, s := range h.after(at) {
			if s.path == tokenPath {
				exchange = s
				return true
			}
		}
		return false
	}, eventually, tick, "the client exchanged")
	return exchange, header(t, assertion(t, exchange))
}

// firstAfter is the first request the server was sent after at.
func (h *harness) firstAfter(t *testing.T, at time.Time) seen {
	t.Helper()
	require.Eventually(t, func() bool { return len(h.after(at)) > 0 }, eventually, tick)
	return h.after(at)[0]
}

func signsAuthorizationWithCurrentKey(t *testing.T) {
	h := newHarness(t)
	h.rotating(t)
	h.connected()
	jwk, current := h.registeredKey(t)
	currentKey, _ := publicKey(t, jwk)

	rotation := h.firstRotation(t)

	sent := body(t, rotation)
	successor, successorID := publicKey(t, sent["jwk"])
	authorization := verify(t, sent["authorization"].(string), currentKey)
	assert.Equal(t, current, authorization.kid, "ROT-1: authorization's kid is the current key's thumbprint")
	assert.Equal(t, "key-rotation+jwt", authorization.typ)
	assert.Equal(t, successorID, authorization.claims["jkt"], "ROT-1: jkt is the successor's thumbprint")
	proof := verify(t, sent["proof"].(string), successor)
	assert.Equal(t, successorID, proof.kid, "ROT-1: proof's kid is the successor's thumbprint")
	assert.Equal(t, "key-proof+jwt", proof.typ)
	for name, signed := range map[string]signedBy{"authorization": authorization, "proof": proof} {
		assert.Equal(t, string(h.record), signed.claims["iss"], "%s: iss", name)
		assert.Equal(t, string(h.record), signed.claims["sub"], "%s: sub", name)
		assert.Equal(t, h.base+keysPath, signed.claims["aud"], "%s: aud is the route's absolute URL", name)
		assert.NotEmpty(t, signed.claims["jti"], "%s: jti", name)
		iat, _ := signed.claims["iat"].(float64)
		exp, _ := signed.claims["exp"].(float64)
		assert.LessOrEqual(t, exp-iat, 60.0, "%s: exp at most 60 seconds after iat", name)
	}
	assert.NotEqual(t, authorization.claims["jti"], proof.claims["jti"], "each JWS has a fresh jti")
}

// storesSuccessorBeforeSending kills the client while its rotation request is in flight, so whatever it
// did not store before sending is lost, and requires the restarted client to try the successor first.
func storesSuccessorBeforeSending(t *testing.T) {
	h := newHarness(t)
	client := h.rotating(t)
	h.connected()
	var killedAt time.Time
	h.setIntercept(once(keysPath, func(w http.ResponseWriter, _ *http.Request, _ http.Handler) {
		client.kill()
		killedAt = time.Now()
		drop(w)
	}))
	rotation := h.firstRotation(t)
	require.Eventually(t, client.exited, eventually, tick, "setup: the client was killed mid-rotation")

	client.restart()

	first := h.firstAfter(t, killedAt)
	assert.Equal(t, tokenPath, first.path, "ROT-5: an exchange comes before anything else")
	if first.path == tokenPath {
		assert.Equal(t, successorKid(t, rotation), header(t, assertion(t, first)),
			"ROT-5: with the successor stored before the request was sent")
	}
}

func signsWithSuccessorAfterAnswer(t *testing.T) {
	h := newHarness(t)
	h.rotating(t)
	h.connected()
	var answeredAt time.Time
	h.setIntercept(once(keysPath, func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		next.ServeHTTP(w, r)
		answeredAt = time.Now()
	}))
	rotation := h.firstRotation(t)
	require.Eventually(t, func() bool { return !answeredAt.IsZero() }, eventually, tick)
	h.setIntercept(once(declarationsPath, func(w http.ResponseWriter, _ *http.Request, _ http.Handler) {
		answer(w, http.StatusUnauthorized, "unauthorized")
	}))
	h.wake()

	_, kid := h.nextExchange(t, answeredAt)

	assert.Equal(t, successorKid(t, rotation), kid, "ROT-5: once an answer arrives, the successor signs")
}

// lostAnswer answers the first rotation request by dropping the connection, after the server took it
// where took is true.
func lostAnswer(took bool) func(w http.ResponseWriter, r *http.Request, next http.Handler) {
	return func(w http.ResponseWriter, r *http.Request, next http.Handler) {
		if took {
			next.ServeHTTP(httptest.NewRecorder(), r)
		}
		drop(w)
	}
}

func triesSuccessorAfterLostAnswer(t *testing.T) {
	h := newHarness(t)
	h.rotating(t)
	h.connected()
	h.setIntercept(once(keysPath, lostAnswer(true)))
	rotation := h.firstRotation(t)

	first := h.firstAfter(t, rotation.at)

	assert.Equal(t, tokenPath, first.path, "ROT-5: an exchange comes before anything else")
	if first.path == tokenPath {
		assert.Equal(t, successorKid(t, rotation), header(t, assertion(t, first)), "ROT-5: with the successor")
	}
	h.declare("", bot(h.watched[0], "after-the-rotation", "reviewer"))
	h.applied(h.watched[0], "after-the-rotation")
}

func triesSuccessorAfter409(t *testing.T) {
	h := newHarness(t)
	h.rotating(t)
	h.connected()
	h.setIntercept(once(keysPath, func(w http.ResponseWriter, _ *http.Request, _ http.Handler) {
		answer(w, http.StatusConflict, "too_many_keys")
	}))
	rotation := h.firstRotation(t)

	first := h.firstAfter(t, rotation.at)

	assert.Equal(t, tokenPath, first.path, "ROT-5: after a 409, an exchange comes before anything else")
	if first.path == tokenPath {
		assert.Equal(t, successorKid(t, rotation), header(t, assertion(t, first)), "ROT-5: with the successor")
	}
}

func repeatAfterLostAnswerUsesFreshJTI(t *testing.T) {
	h := newHarness(t)
	h.rotating(t)
	h.connected()
	h.setIntercept(once(keysPath, lostAnswer(false)))
	first := h.firstRotation(t)

	require.Eventually(t, func() bool { return len(h.sent(keysPath)) >= 2 }, eventually, tick,
		"ROT-5: the successor's exchange failed, so the rotation is repeated")

	repeat := h.sent(keysPath)[1]
	assert.Equal(t, successorKid(t, first), successorKid(t, repeat), "ROT-4: the same successor")
	jwk := body(t, first)["jwk"]
	successor, _ := publicKey(t, jwk)
	for _, member := range []string{"authorization", "proof"} {
		was := verify(t, body(t, first)[member].(string), keyFor(t, h, member, successor))
		is := verify(t, body(t, repeat)[member].(string), keyFor(t, h, member, successor))
		assert.NotEqual(t, was.claims["jti"], is.claims["jti"], "ROT-4: %s is signed afresh, with a new jti", member)
	}
}

// keyFor is the key a rotation's member verifies under: the registered key for authorization, the
// successor for proof.
func keyFor(t *testing.T, h *harness, member string, successor *rsa.PublicKey) *rsa.PublicKey {
	t.Helper()
	if member == "proof" {
		return successor
	}
	jwk, _ := h.registeredKey(t)
	key, _ := publicKey(t, jwk)
	return key
}

func keepsPredecessorAfter409(t *testing.T) {
	h := newHarness(t)
	h.rotating(t)
	h.connected()
	_, predecessor := h.registeredKey(t)
	h.setIntercept(once(keysPath, func(w http.ResponseWriter, _ *http.Request, _ http.Handler) {
		answer(w, http.StatusConflict, "too_many_keys")
	}))
	rotation := h.firstRotation(t)
	tried, kid := h.nextExchange(t, rotation.at)
	require.Equal(t, successorKid(t, rotation), kid, "setup: the successor was tried, and garam holds it nowhere")
	h.setIntercept(once(declarationsPath, func(w http.ResponseWriter, _ *http.Request, _ http.Handler) {
		answer(w, http.StatusUnauthorized, "unauthorized")
	}))
	h.wake()

	_, kid = h.nextExchange(t, tried.at)

	assert.Equal(t, predecessor, kid, "ROT-5: after a 409 and a failed exchange, the predecessor still signs")
}
