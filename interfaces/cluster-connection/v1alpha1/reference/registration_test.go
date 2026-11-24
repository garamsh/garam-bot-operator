package reference_test

import (
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/garam-agent-operator/interfaces/cluster-connection/v1alpha1/reference"
)

func TestRegister_AcceptsFreshToken(t *testing.T) {
	h := newHarness(t)
	key := testKeys()[0]
	token, grn := h.mint()

	got := h.post(regPath, "", registration(token, publicJWK(t, key)))

	require.Equal(t, http.StatusCreated, got.status, string(got.body))
	body := got.json(t)
	assert.Equal(t, string(grn), body["cluster"])
	assert.Equal(t, string(grn), body["clientId"])
	assert.Equal(t, h.base+"/oauth/token", body["tokenUrl"])
	assert.Regexp(t, `^grn:[^/]+/iam/cluster/[^/]+$`, body["cluster"])
	// The bound key is live: an exchange under it succeeds and its token reaches a route.
	bearer := h.exchange(grn, key)
	assert.Equal(t, http.StatusOK, h.poll(bearer, "", 0).status)
}

func TestRegister_RefusesSpentToken(t *testing.T) {
	h := newHarness(t)
	token, _ := h.mint()

	first := h.post(regPath, "", registration(token, publicJWK(t, testKeys()[0])))
	second := h.post(regPath, "", registration(token, publicJWK(t, testKeys()[1])))

	require.Equal(t, http.StatusCreated, first.status, "control: the token is good once")
	assert.Equal(t, http.StatusUnauthorized, second.status)
	assert.Equal(t, "registration_refused", second.errorCode(t))
}

func TestRegister_RefusesUnknownTokenSameAsSpent(t *testing.T) {
	h := newHarness(t)
	token, _ := h.mint()
	require.Equal(t, http.StatusCreated, h.post(regPath, "", registration(token, publicJWK(t, testKeys()[0]))).status)

	spent := h.post(regPath, "", registration(token, publicJWK(t, testKeys()[1])))
	unknown := h.post(regPath, "", registration(rand.Text(), publicJWK(t, testKeys()[1])))

	assert.Equal(t, http.StatusUnauthorized, unknown.status)
	assert.Equal(t, spent.status, unknown.status)
	assert.Equal(t, spent.errorCode(t), unknown.errorCode(t))
	assert.Equal(t, spent.json(t)["message"], unknown.json(t)["message"])
}

func TestRegister_RefusesExpiredToken(t *testing.T) {
	h := newHarness(t)
	late, _ := h.mint()
	inTime, _ := h.mint()

	h.clock.advance(time.Hour - time.Second)
	accepted := h.post(regPath, "", registration(inTime, publicJWK(t, testKeys()[0])))
	h.clock.advance(time.Second)
	refused := h.post(regPath, "", registration(late, publicJWK(t, testKeys()[1])))

	require.Equal(t, http.StatusCreated, accepted.status, "control: a token under an hour old registers")
	assert.Equal(t, http.StatusUnauthorized, refused.status)
	assert.Equal(t, "registration_refused", refused.errorCode(t))
}

func TestRegister_RefusesTokenMintedExpired(t *testing.T) {
	h := newHarness(t)
	minted := h.fix("mint-token", reference.FixtureRequest{Expired: true})

	got := h.post(regPath, "", registration(minted["registrationToken"].(string), publicJWK(t, testKeys()[0])))

	assert.Equal(t, http.StatusUnauthorized, got.status)
	assert.Equal(t, "registration_refused", got.errorCode(t))
}

func TestRegister_SpendsTokenOnRefusedKey(t *testing.T) {
	h := newHarness(t)
	token, _ := h.mint()
	private := publicJWK(t, testKeys()[0])
	private["d"] = "AQAB"

	refusedKey := h.post(regPath, "", registration(token, private))
	retried := h.post(regPath, "", registration(token, publicJWK(t, testKeys()[0])))

	assert.Equal(t, http.StatusBadRequest, refusedKey.status)
	assert.Equal(t, http.StatusUnauthorized, retried.status)
	assert.Equal(t, "registration_refused", retried.errorCode(t))
}

func TestRegister_RefusesPrivateJWKMembers(t *testing.T) {
	h := newHarness(t)
	for _, member := range []string{"d", "p", "q", "dp", "dq", "qi"} {
		t.Run(member, func(t *testing.T) {
			token, _ := h.mint()
			jwk := publicJWK(t, testKeys()[0])
			jwk[member] = "AQAB"

			got := h.post(regPath, "", registration(token, jwk))

			assert.Equal(t, http.StatusBadRequest, got.status)
			assert.Equal(t, "invalid_request", got.errorCode(t))
		})
	}
	t.Run("control: the public half alone", func(t *testing.T) {
		token, _ := h.mint()
		assert.Equal(t, http.StatusCreated, h.post(regPath, "", registration(token, publicJWK(t, testKeys()[0]))).status)
	})
}

func TestRegister_RefusesKeyUnder3072Bits(t *testing.T) {
	h := newHarness(t)
	small, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)
	token, _ := h.mint()

	got := h.post(regPath, "", registration(token, publicJWK(t, small)))

	assert.Equal(t, http.StatusBadRequest, got.status)
	assert.Equal(t, "invalid_request", got.errorCode(t))
}

func TestRegister_WithdrawsEarlierKeys(t *testing.T) {
	h := newHarness(t)
	first, second := testKeys()[0], testKeys()[1]
	grn := h.register(first)
	earlier := h.exchange(grn, first)
	require.Equal(t, http.StatusOK, h.poll(earlier, "", 0).status, "control: the first key's token is live")

	again := h.fix("mint-token", reference.FixtureRequest{Cluster: grn})
	require.Equal(t, string(grn), again["cluster"])
	got := h.post(regPath, "", registration(again["registrationToken"].(string), publicJWK(t, second)))
	require.Equal(t, http.StatusCreated, got.status, string(got.body))

	assert.Equal(t, string(grn), got.json(t)["cluster"], "registered again under the same record")
	assert.Equal(t, http.StatusUnauthorized, h.poll(earlier, "", 0).status, "a token under the withdrawn key")
	stale := h.tryExchange(signed(t, first, first, "JWT", h.claims(grn, h.base+"/oauth/token")), grn)
	assert.Equal(t, http.StatusUnauthorized, stale.status, "an exchange under the withdrawn key")
	assert.Equal(t, http.StatusOK, h.poll(h.exchange(grn, second), "", 0).status)
}

func TestMintToken_DrawsDistinctTokensOfAtLeast128Bits(t *testing.T) {
	h := newHarness(t)
	seen := map[string]bool{}
	for range 64 {
		token, _ := h.mint()
		raw, err := base64.RawURLEncoding.DecodeString(token)
		require.NoError(t, err)
		assert.GreaterOrEqual(t, len(raw)*8, 128)
		assert.False(t, seen[token], "a token was drawn twice")
		seen[token] = true
	}
}

func TestRegister_ThrottleSpendsNothing(t *testing.T) {
	h := newHarness(t, registrationLimit(1))
	token, _ := h.mint()
	require.Equal(t, http.StatusUnauthorized,
		h.post(regPath, "", registration(rand.Text(), publicJWK(t, testKeys()[0]))).status,
		"setup: the source's one registration this minute is used")

	throttled := h.post(regPath, "", registration(token, publicJWK(t, testKeys()[0])))
	h.clock.advance(time.Minute)
	later := h.post(regPath, "", registration(token, publicJWK(t, testKeys()[0])))

	assert.Equal(t, http.StatusTooManyRequests, throttled.status)
	assert.Equal(t, "throttled", throttled.errorCode(t))
	assert.Equal(t, "60", throttled.header.Get("Retry-After"))
	assert.Equal(t, http.StatusCreated, later.status, "the token the 429 answered is still good: %s", later.body)
}
