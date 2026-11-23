package reference_test

import (
	"crypto/rsa"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/garam-agent-operator/interfaces/cluster-connection/v1alpha1/reference"
)

// rotation is a rotation request whose authorization is signed by current and whose proof by successor.
type rotation struct {
	current, successor *rsa.PrivateKey
	// authorizationClaims and proofClaims, where set, change the claims before signing.
	authorizationClaims, proofClaims func(map[string]any)
}

func (h *harness) rotationBody(grn reference.GRN, r rotation) map[string]any {
	h.t.Helper()
	audience := h.base + keysPath
	authorization := h.claims(grn, audience)
	authorization["jkt"] = kid(h.t, r.successor)
	if r.authorizationClaims != nil {
		r.authorizationClaims(authorization)
	}
	proof := h.claims(grn, audience)
	if r.proofClaims != nil {
		r.proofClaims(proof)
	}
	return map[string]any{
		"jwk":           publicJWK(h.t, r.successor),
		"authorization": signed(h.t, r.current, r.current, "key-rotation+jwt", authorization),
		"proof":         signed(h.t, r.successor, r.successor, "key-proof+jwt", proof),
	}
}

func TestRotateKey_AcceptsProvedSuccessor(t *testing.T) {
	h := newHarness(t)
	current, successor := testKeys()[0], testKeys()[1]
	grn := h.register(current)
	bearer := h.exchange(grn, current)

	got := h.post(keysPath, bearer, h.rotationBody(grn, rotation{current: current, successor: successor}))

	require.Equal(t, http.StatusCreated, got.status, string(got.body))
	body := got.json(t)
	assert.Equal(t, kid(t, successor), body["kid"])
	retires, err := time.Parse(time.RFC3339, body["predecessorRetiresAt"].(string))
	require.NoError(t, err)
	assert.Equal(t, h.clock.Now().Add(24*time.Hour), retires)
	assert.NotEmpty(t, h.exchange(grn, successor), "the successor is live")
	assert.NotEmpty(t, h.exchange(grn, current), "the predecessor is still live")
}

func TestRotateKey_RefusesWhatDoesNotProve(t *testing.T) {
	current, successor, other := testKeys()[0], testKeys()[1], testKeys()[2]
	cases := map[string]func(h *harness, grn reference.GRN) map[string]any{
		"proof signed by another key": func(h *harness, grn reference.GRN) map[string]any {
			body := h.rotationBody(grn, rotation{current: current, successor: successor})
			body["proof"] = signed(t, other, successor, "key-proof+jwt", h.claims(grn, h.base+keysPath))
			return body
		},
		"authorization without the current key's signature": func(h *harness, grn reference.GRN) map[string]any {
			return h.rotationBody(grn, rotation{current: successor, successor: successor})
		},
		"authorization for another successor": func(h *harness, grn reference.GRN) map[string]any {
			return h.rotationBody(grn, rotation{current: current, successor: successor,
				authorizationClaims: func(c map[string]any) { c["jkt"] = kid(t, other) }})
		},
		"proof for another audience": func(h *harness, grn reference.GRN) map[string]any {
			return h.rotationBody(grn, rotation{current: current, successor: successor,
				proofClaims: func(c map[string]any) { c["aud"] = h.base + "/oauth/token" }})
		},
		"authorization expired": func(h *harness, grn reference.GRN) map[string]any {
			return h.rotationBody(grn, rotation{current: current, successor: successor,
				authorizationClaims: func(c map[string]any) {
					c["iat"], c["exp"] = h.clock.Now().Unix()-120, h.clock.Now().Unix()-60
				}})
		},
		"authorization of the wrong typ": func(h *harness, grn reference.GRN) map[string]any {
			body := h.rotationBody(grn, rotation{current: current, successor: successor})
			c := h.claims(grn, h.base+keysPath)
			c["jkt"] = kid(t, successor)
			body["authorization"] = signed(t, current, current, "key-proof+jwt", c)
			return body
		},
		"no authorization: the bearer token alone": func(h *harness, grn reference.GRN) map[string]any {
			body := h.rotationBody(grn, rotation{current: current, successor: successor})
			delete(body, "authorization")
			return body
		},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			grn := h.register(current)
			bearer := h.exchange(grn, current)

			got := h.post(keysPath, bearer, body(h, grn))

			assert.Equal(t, http.StatusBadRequest, got.status, string(got.body))
			assert.Equal(t, "invalid_proof", got.errorCode(t))
			assert.Equal(t, http.StatusCreated,
				h.post(keysPath, bearer, h.rotationBody(grn, rotation{current: current, successor: successor})).status,
				"control: the same rotation, proved, is accepted")
		})
	}
}

func TestRotateKey_RefusesReplayedProof(t *testing.T) {
	h := newHarness(t)
	current, successor := testKeys()[0], testKeys()[1]
	grn := h.register(current)
	bearer := h.exchange(grn, current)
	body := h.rotationBody(grn, rotation{current: current, successor: successor})
	require.Equal(t, http.StatusCreated, h.post(keysPath, bearer, body).status, "control: first use")

	got := h.post(keysPath, bearer, body)

	assert.Equal(t, http.StatusBadRequest, got.status)
	assert.Equal(t, "invalid_proof", got.errorCode(t))
}

func TestRotateKey_RefusesAuthorizationByRetiredKey(t *testing.T) {
	h := newHarness(t)
	first, second, third := testKeys()[0], testKeys()[1], testKeys()[2]
	grn := h.register(first)
	bearer := h.exchange(grn, first)
	require.Equal(t, http.StatusCreated,
		h.post(keysPath, bearer, h.rotationBody(grn, rotation{current: first, successor: second})).status)
	h.clock.advance(24 * time.Hour)
	bearer = h.exchange(grn, second)

	byRetired := h.post(keysPath, bearer, h.rotationBody(grn, rotation{current: first, successor: third}))
	byLive := h.post(keysPath, bearer, h.rotationBody(grn, rotation{current: second, successor: third}))

	assert.Equal(t, http.StatusBadRequest, byRetired.status)
	assert.Equal(t, "invalid_proof", byRetired.errorCode(t))
	assert.Equal(t, http.StatusCreated, byLive.status, "control: the live key authorizes the same successor")
}

func TestRotateKey_RepeatAnswersFirstSuccess(t *testing.T) {
	h := newHarness(t)
	current, successor := testKeys()[0], testKeys()[1]
	grn := h.register(current)
	bearer := h.exchange(grn, current)
	first := h.post(keysPath, bearer, h.rotationBody(grn, rotation{current: current, successor: successor}))
	require.Equal(t, http.StatusCreated, first.status)

	h.clock.advance(time.Minute)
	repeat := h.post(keysPath, bearer, h.rotationBody(grn, rotation{current: current, successor: successor}))

	require.Equal(t, http.StatusCreated, repeat.status, string(repeat.body))
	assert.Equal(t, first.json(t), repeat.json(t))
}

func TestRotateKey_RefusesThirdKey(t *testing.T) {
	h := newHarness(t)
	first, second, third := testKeys()[0], testKeys()[1], testKeys()[2]
	grn := h.register(first)
	bearer := h.exchange(grn, first)
	require.Equal(t, http.StatusCreated,
		h.post(keysPath, bearer, h.rotationBody(grn, rotation{current: first, successor: second})).status)

	got := h.post(keysPath, bearer, h.rotationBody(grn, rotation{current: first, successor: third}))

	assert.Equal(t, http.StatusConflict, got.status)
	assert.Equal(t, "too_many_keys", got.errorCode(t))
}

func TestRotateKey_PredecessorLive24Hours(t *testing.T) {
	h := newHarness(t)
	current, successor := testKeys()[0], testKeys()[1]
	grn := h.register(current)
	bearer := h.exchange(grn, current)
	require.Equal(t, http.StatusCreated,
		h.post(keysPath, bearer, h.rotationBody(grn, rotation{current: current, successor: successor})).status)

	h.clock.advance(24*time.Hour - time.Second)
	assert.NotEmpty(t, h.exchange(grn, current), "live a second before the rollover ends")
	h.clock.advance(time.Second)
	retired := h.tryExchange(signed(t, current, current, "JWT", h.claims(grn, h.base+"/oauth/token")), grn)

	assert.Equal(t, http.StatusUnauthorized, retired.status)
	assert.NotEmpty(t, h.exchange(grn, successor))
}
