//go:build conformance

package server

import (
	"crypto/rsa"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/garam-agent-operator/interfaces/cluster-connection/v1alpha1/conformance/internal/wire"
)

// rotation changes a rotation request's claims before they are signed.
type rotation struct {
	authorization, proof func(map[string]any)
}

// rotationBody is ROT-1's request for record: successor's public half, an authorization signed by
// current naming successor's thumbprint, and a proof signed by successor.
func rotationBody(t *testing.T, record wire.Record, current, successor *rsa.PrivateKey,
	change ...rotation) map[string]any {
	t.Helper()
	audience := target.BaseURL + wire.KeysPath
	authorization := record.Claims(audience)
	authorization["jkt"] = wire.Kid(t, successor)
	proof := record.Claims(audience)
	for _, c := range change {
		if c.authorization != nil {
			c.authorization(authorization)
		}
		if c.proof != nil {
			c.proof(proof)
		}
	}
	return map[string]any{
		"jwk":           wire.PublicJWK(t, successor),
		"authorization": wire.Signed(t, current, current, "key-rotation+jwt", authorization),
		"proof":         wire.Signed(t, successor, successor, "key-proof+jwt", proof),
	}
}

// rotate sends a proved rotation of record's key to successor and requires it to be taken.
func rotate(t *testing.T, record wire.Record, bearer string, successor *rsa.PrivateKey) map[string]any {
	t.Helper()
	got := target.Post(t, wire.KeysPath, bearer, rotationBody(t, record, record.Key, successor))
	require.Equal(t, http.StatusCreated, got.Status, string(got.Body))
	return got.JSON(t)
}

// refusedAsUnproved requires a rotation body to be refused as ROT-2 says, and then, as a control, the
// same rotation proved to be taken.
func refusedAsUnproved(t *testing.T, body func(record wire.Record, successor *rsa.PrivateKey) map[string]any) {
	t.Helper()
	record, bearer := target.Connect(t)
	successor := wire.Key(t)

	got := target.Post(t, wire.KeysPath, bearer, body(record, successor))

	assert.Equal(t, http.StatusBadRequest, got.Status, string(got.Body))
	assert.Equal(t, "invalid_proof", got.ErrorCode(t))
	rotate(t, record, bearer, successor)
}

func acceptsProvedSuccessor(t *testing.T) {
	record, bearer := target.Connect(t)
	successor := wire.Key(t)
	before := time.Now()

	body := rotate(t, record, bearer, successor)

	assert.Equal(t, wire.Kid(t, successor), body["kid"], "ROT-3: the successor's kid")
	retires, err := time.Parse(time.RFC3339, stringOf(body["predecessorRetiresAt"]))
	require.NoError(t, err, "ROT-3: predecessorRetiresAt")
	assert.False(t, retires.Before(before.Add(24*time.Hour)), "ROT-3: at least 24 hours away")
	assert.NotEmpty(t, target.Exchange(t, record, successor), "the successor is live")
	assert.NotEmpty(t, target.Exchange(t, record, record.Key), "the predecessor is still live")
}

func refusesBadProof(t *testing.T) {
	cases := map[string]func(record wire.Record, successor *rsa.PrivateKey) map[string]any{
		"signed by another key": func(record wire.Record, successor *rsa.PrivateKey) map[string]any {
			body := rotationBody(t, record, record.Key, successor)
			body["proof"] = wire.Signed(t, wire.Key(t), successor, "key-proof+jwt", record.Claims(target.BaseURL+wire.KeysPath))
			return body
		},
		"for another audience": func(record wire.Record, successor *rsa.PrivateKey) map[string]any {
			return rotationBody(t, record, record.Key, successor,
				rotation{proof: func(c map[string]any) { c["aud"] = record.TokenURL }})
		},
		"expired": func(record wire.Record, successor *rsa.PrivateKey) map[string]any {
			return rotationBody(t, record, record.Key, successor, rotation{proof: func(c map[string]any) {
				c["iat"], c["exp"] = time.Now().Unix()-180, time.Now().Unix()-120
			}})
		},
		"of the wrong typ": func(record wire.Record, successor *rsa.PrivateKey) map[string]any {
			body := rotationBody(t, record, record.Key, successor)
			body["proof"] = wire.Signed(t, successor, successor, "key-rotation+jwt", record.Claims(target.BaseURL+wire.KeysPath))
			return body
		},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) { refusedAsUnproved(t, body) })
	}
}

func refusesReplayedProof(t *testing.T) {
	record, bearer := target.Connect(t)
	body := rotationBody(t, record, record.Key, wire.Key(t))
	require.Equal(t, http.StatusCreated, target.Post(t, wire.KeysPath, bearer, body).Status, "control: first use")

	got := target.Post(t, wire.KeysPath, bearer, body)

	assert.Equal(t, http.StatusBadRequest, got.Status, "the same jti twice: %s", got.Body)
	assert.Equal(t, "invalid_proof", got.ErrorCode(t))
}

func refusesUnsignedRotation(t *testing.T) {
	cases := map[string]func(record wire.Record, successor *rsa.PrivateKey) map[string]any{
		"authorization signed by the successor": func(record wire.Record, successor *rsa.PrivateKey) map[string]any {
			return rotationBody(t, record, successor, successor)
		},
		"no authorization: the bearer token alone": func(record wire.Record, successor *rsa.PrivateKey) map[string]any {
			body := rotationBody(t, record, record.Key, successor)
			delete(body, "authorization")
			return body
		},
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) { refusedAsUnproved(t, body) })
	}
}

func refusesAuthorizationForAnotherSuccessor(t *testing.T) {
	refusedAsUnproved(t, func(record wire.Record, successor *rsa.PrivateKey) map[string]any {
		return rotationBody(t, record, record.Key, successor,
			rotation{authorization: func(c map[string]any) { c["jkt"] = wire.Kid(t, wire.Key(t)) }})
	})
}

// refusesAuthorizationByRetiredKey retires a key the one way a black box can without waiting 24 hours:
// by registering the record again, which withdraws every key it held (REG-6).
func refusesAuthorizationByRetiredKey(t *testing.T) {
	first, _ := target.Connect(t)
	token, _ := target.Mint(t, "--cluster", first.Cluster)
	second := target.RegisterKey(t, token, wire.Key(t))
	bearer := target.Exchange(t, second, second.Key)
	successor := wire.Key(t)

	byRetired := target.Post(t, wire.KeysPath, bearer, rotationBody(t, second, first.Key, successor))

	assert.Equal(t, http.StatusBadRequest, byRetired.Status, string(byRetired.Body))
	assert.Equal(t, "invalid_proof", byRetired.ErrorCode(t))
	rotate(t, second, bearer, successor)
}

func repeatAnswersFirstSuccess(t *testing.T) {
	record, bearer := target.Connect(t)
	successor := wire.Key(t)
	first := rotate(t, record, bearer, successor)

	repeat := rotate(t, record, bearer, successor)

	assert.Equal(t, first["kid"], repeat["kid"])
	assert.Equal(t, first["predecessorRetiresAt"], repeat["predecessorRetiresAt"])
}

func refusesThirdKey(t *testing.T) {
	record, bearer := target.Connect(t)
	rotate(t, record, bearer, wire.Key(t))

	got := target.Post(t, wire.KeysPath, bearer, rotationBody(t, record, record.Key, wire.Key(t)))

	assert.Equal(t, http.StatusConflict, got.Status, string(got.Body))
	assert.Equal(t, "too_many_keys", got.ErrorCode(t))
}

func refusesKeyHeldBefore(t *testing.T) {
	cases := map[string]func(t *testing.T) (wire.Record, string, *rsa.PrivateKey){
		"the key registration bound": func(t *testing.T) (wire.Record, string, *rsa.PrivateKey) {
			record, bearer := target.Connect(t)
			return record, bearer, record.Key
		},
		"a key a later registration withdrew": func(t *testing.T) (wire.Record, string, *rsa.PrivateKey) {
			first, _ := target.Connect(t)
			token, _ := target.Mint(t, "--cluster", first.Cluster)
			second := target.RegisterKey(t, token, wire.Key(t))
			return second, target.Exchange(t, second, second.Key), first.Key
		},
	}
	for name, setup := range cases {
		t.Run(name, func(t *testing.T) {
			record, bearer, held := setup(t)

			got := target.Post(t, wire.KeysPath, bearer, rotationBody(t, record, record.Key, held))

			assert.Equal(t, http.StatusBadRequest, got.Status, string(got.Body))
			assert.Equal(t, "invalid_request", got.ErrorCode(t))
			rotate(t, record, bearer, wire.Key(t))
		})
	}
}

// predecessorLive24h holds what a run can of ROT-3 without waiting a day: the rollover ends at least 24
// hours after the rotation, and until then the predecessor exchanges and its token is taken.
func predecessorLive24h(t *testing.T) {
	record, bearer := target.Connect(t)
	before := time.Now()

	body := rotate(t, record, bearer, wire.Key(t))

	retires, err := time.Parse(time.RFC3339, stringOf(body["predecessorRetiresAt"]))
	require.NoError(t, err)
	assert.False(t, retires.Before(before.Add(24*time.Hour)))
	assert.Equal(t, http.StatusOK, target.Poll(t, bearer, "", 0).Status, "a token under the predecessor is taken")
	assert.Equal(t, http.StatusOK, target.Poll(t, target.Exchange(t, record, record.Key), "", 0).Status,
		"the predecessor still exchanges")
}

func stringOf(v any) string {
	s, _ := v.(string)
	return s
}
