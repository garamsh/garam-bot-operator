//go:build conformance

package client

import (
	"crypto"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"net/url"
	"testing"

	"github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/require"
)

// signedBy is a compact JWS read whole: its header and claims, and the key it verifies under.
type signedBy struct {
	kid    string
	typ    string
	claims map[string]any
}

// verify reads compact as an RS256 JWS and requires it to verify under key.
func verify(t *testing.T, compact string, key *rsa.PublicKey) signedBy {
	t.Helper()
	jws, err := jose.ParseSigned(compact, []jose.SignatureAlgorithm{jose.RS256})
	require.NoError(t, err, "an RS256 compact JWS")
	payload, err := jws.Verify(key)
	require.NoError(t, err, "the JWS verifies under the key named")
	header := jws.Signatures[0].Header
	out := signedBy{kid: header.KeyID}
	out.typ, _ = header.ExtraHeaders[jose.HeaderType].(string)
	require.NoError(t, json.Unmarshal(payload, &out.claims))
	return out
}

// header reads a compact JWS's header without verifying it.
func header(t *testing.T, compact string) (kid string) {
	t.Helper()
	jws, err := jose.ParseSigned(compact, []jose.SignatureAlgorithm{jose.RS256})
	require.NoError(t, err)
	return jws.Signatures[0].Header.KeyID
}

// publicKey reads an RFC 7517 JWK as an RSA public key, and answers its RFC 7638 thumbprint.
func publicKey(t *testing.T, jwk any) (*rsa.PublicKey, string) {
	t.Helper()
	encoded, err := json.Marshal(jwk)
	require.NoError(t, err)
	var parsed jose.JSONWebKey
	require.NoError(t, parsed.UnmarshalJSON(encoded))
	key, ok := parsed.Key.(*rsa.PublicKey)
	require.True(t, ok, "an RSA public key")
	sum, err := parsed.Thumbprint(crypto.SHA256)
	require.NoError(t, err)
	return key, base64.RawURLEncoding.EncodeToString(sum)
}

// assertion is the client_assertion of an exchange request's form.
func assertion(t *testing.T, s seen) string {
	t.Helper()
	form, err := url.ParseQuery(string(s.body))
	require.NoError(t, err)
	return form.Get("client_assertion")
}

// body reads a request's JSON body.
func body(t *testing.T, s seen) map[string]any {
	t.Helper()
	var out map[string]any
	require.NoError(t, json.Unmarshal(s.body, &out), string(s.body))
	return out
}
