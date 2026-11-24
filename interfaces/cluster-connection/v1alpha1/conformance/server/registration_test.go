//go:build conformance

package server

import (
	"crypto/rand"
	"crypto/rsa"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/garam-agent-operator/interfaces/cluster-connection/v1alpha1/conformance/internal/wire"
)

func acceptsFreshToken(t *testing.T) {
	token, cluster := target.Mint(t)

	record := target.RegisterKey(t, token, wire.Key(t))

	assert.Equal(t, cluster, record.Cluster, "REG-6: the record the token was minted for")
	bearer := target.Exchange(t, record, record.Key)
	assert.Equal(t, http.StatusOK, target.Poll(t, bearer, "", 0).Status, "REG-6: the key is live")
}

func refusesSpentToken(t *testing.T) {
	token, _ := target.Mint(t)
	target.RegisterKey(t, token, wire.Key(t))

	again := target.Register(t, wire.Registration(t, token, wire.Key(t)))

	assert.Equal(t, http.StatusUnauthorized, again.Status, string(again.Body))
	assert.Equal(t, "registration_refused", again.ErrorCode(t))
}

func refusesUnknownTokenSameAsSpent(t *testing.T) {
	token, _ := target.Mint(t)
	target.RegisterKey(t, token, wire.Key(t))

	spent := target.Register(t, wire.Registration(t, token, wire.Key(t)))
	unknown := target.Register(t, wire.Registration(t, rand.Text(), wire.Key(t)))

	assert.Equal(t, http.StatusUnauthorized, unknown.Status)
	assert.Equal(t, spent.Status, unknown.Status, "REG-4: one answer")
	assert.Equal(t, spent.JSON(t), unknown.JSON(t), "REG-4: one answer, so neither tells the other apart")
}

func refusesExpiredToken(t *testing.T) {
	expired, _ := target.Mint(t, "--expired")
	fresh, _ := target.Mint(t)

	refused := target.Register(t, wire.Registration(t, expired, wire.Key(t)))

	assert.Equal(t, http.StatusUnauthorized, refused.Status, string(refused.Body))
	assert.Equal(t, "registration_refused", refused.ErrorCode(t))
	target.RegisterKey(t, fresh, wire.Key(t))
}

func spendsTokenOnRefusedKey(t *testing.T) {
	token, _ := target.Mint(t)
	small, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	refusedKey := target.Register(t, wire.Registration(t, token, small))
	retried := target.Register(t, wire.Registration(t, token, wire.Key(t)))

	assert.Equal(t, http.StatusBadRequest, refusedKey.Status, "REG-1: a key under 3072 bits: %s", refusedKey.Body)
	assert.Equal(t, http.StatusUnauthorized, retried.Status, "REG-3: the refused request spent the token")
	assert.Equal(t, "registration_refused", retried.ErrorCode(t))
}

func refusesPrivateJWKMembers(t *testing.T) {
	for _, member := range []string{"d", "p", "q", "dp", "dq", "qi"} {
		t.Run(member, func(t *testing.T) {
			token, _ := target.Mint(t)
			body := wire.Registration(t, token, wire.Key(t))
			body["jwk"].(map[string]any)[member] = "AQAB"

			got := target.Register(t, body)

			assert.Equal(t, http.StatusBadRequest, got.Status, string(got.Body))
			assert.Equal(t, "invalid_request", got.ErrorCode(t))
		})
	}
	token, _ := target.Mint(t)
	target.RegisterKey(t, token, wire.Key(t))
}

func withdrawsEarlierKeys(t *testing.T) {
	first, earlier := target.Connect(t)
	require.Equal(t, http.StatusOK, target.Poll(t, earlier, "", 0).Status, "control: the first key's token is live")
	token, cluster := target.Mint(t, "--cluster", first.Cluster)
	require.Equal(t, first.Cluster, cluster)

	second := target.RegisterKey(t, token, wire.Key(t))

	assert.Equal(t, first.Cluster, second.Cluster, "registered again under the same record")
	assert.Equal(t, http.StatusUnauthorized, target.Poll(t, earlier, "", 0).Status, "a token under the withdrawn key")
	assert.NotEqual(t, http.StatusOK, target.TryExchange(t, first, first.Key).Status,
		"an exchange under the withdrawn key")
	assert.Equal(t, http.StatusOK, target.Poll(t, target.Exchange(t, second, second.Key), "", 0).Status)
}

// distinctTokensAreUnpredictable holds what a black box can of REG-8: no token is drawn twice, and each
// is long enough to carry 128 bits in an alphabet of at most 64 symbols. Where its bits come from is not
// observable here.
func distinctTokensAreUnpredictable(t *testing.T) {
	seen := map[string]bool{}
	for range 64 {
		token, _ := target.Mint(t)
		assert.GreaterOrEqual(t, len(token), 22, "128 bits need at least 22 symbols of 64")
		assert.False(t, seen[token], "a token was drawn twice")
		seen[token] = true
	}
}

// throttleSpendsNothing spends the source's registration bound with unknown tokens, then requires the
// 429 answered past it to have spent nothing: the same token registers once the bound allows. A server
// that bounds nothing within 2000 requests is not held to it (REG-8's bound is a SHOULD).
func throttleSpendsNothing(t *testing.T) {
	token, _ := target.Mint(t)
	junk := wire.Key(t)
	var throttled wire.Answer
	for range 2000 {
		got := target.Post(t, wire.RegistrationsPath, "", wire.Registration(t, rand.Text(), junk))
		if got.Status == http.StatusTooManyRequests {
			throttled = got
			break
		}
		require.Equal(t, http.StatusUnauthorized, got.Status, "setup: an unknown token is refused")
	}
	if throttled.Status == 0 {
		t.Skip("not applicable: the server bounds no registrations within 2000 requests")
	}
	key := wire.Key(t)

	past := target.Post(t, wire.RegistrationsPath, "", wire.Registration(t, token, key))

	require.Equal(t, http.StatusTooManyRequests, past.Status, "setup: the token is presented past the bound")
	assert.Equal(t, "throttled", past.ErrorCode(t))
	assert.NotEmpty(t, past.Header.Get("Retry-After"), "T-6 needs a Retry-After to wait")
	later := target.Register(t, wire.Registration(t, token, key))
	assert.Equal(t, http.StatusCreated, later.Status, "REG-8: the token the 429 answered is unspent: %s", later.Body)
}
