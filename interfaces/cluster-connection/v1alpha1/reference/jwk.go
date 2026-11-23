package reference

import (
	"crypto"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

// keyID is a key's RFC 7638 SHA-256 thumbprint, base64url without padding: the kid every JWS here names.
type keyID string

// publicKey is a parsed PublicJwk of openapi.yaml.
type publicKey struct {
	id  keyID
	key *rsa.PublicKey
}

const (
	// minKeyBits is REG-1's floor on the RSA modulus.
	minKeyBits = 3072
	// maxSignedLifetime is exp - iat at most, for an exchange assertion and for ROT-1's JWSs.
	maxSignedLifetime = 60 * time.Second
)

// privateMembers are the JWK members naming a private parameter, which PublicJwk refuses.
var privateMembers = []string{"d", "p", "q", "dp", "dq", "qi"}

// parsePublicJWK proves raw is the public half of an RSA key of at least 3072 bits, and nothing more.
func parsePublicJWK(raw json.RawMessage) (publicKey, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(raw, &members); err != nil {
		return publicKey{}, errors.New("jwk is not a JSON object")
	}
	for _, name := range privateMembers {
		if _, ok := members[name]; ok {
			return publicKey{}, fmt.Errorf("jwk carries the private member %q", name)
		}
	}
	for name, want := range map[string]string{"kty": "RSA", "alg": "RS256", "use": "sig"} {
		value, ok := members[name]
		if !ok && name != "kty" {
			continue
		}
		var got string
		if err := json.Unmarshal(value, &got); err != nil || got != want {
			return publicKey{}, fmt.Errorf("jwk member %q is not %q", name, want)
		}
	}
	var parsed jose.JSONWebKey
	if err := parsed.UnmarshalJSON(raw); err != nil {
		return publicKey{}, errors.New("jwk is not a valid RSA public key")
	}
	key, ok := parsed.Key.(*rsa.PublicKey)
	if !ok {
		return publicKey{}, errors.New("jwk is not an RSA public key")
	}
	if key.N.BitLen() < minKeyBits {
		return publicKey{}, fmt.Errorf("jwk's modulus is %d bits, under %d", key.N.BitLen(), minKeyBits)
	}
	id, err := thumbprint(key)
	if err != nil {
		return publicKey{}, err
	}
	return publicKey{id: id, key: key}, nil
}

func thumbprint(key *rsa.PublicKey) (keyID, error) {
	sum, err := (&jose.JSONWebKey{Key: key}).Thumbprint(crypto.SHA256)
	if err != nil {
		return "", fmt.Errorf("compute a thumbprint: %w", err)
	}
	return keyID(base64.RawURLEncoding.EncodeToString(sum)), nil
}

// signedClaims are the claims of a JWS this server verifies: the registered ones, and ROT-1's jkt.
type signedClaims struct {
	jwt.Claims
	JKT string `json:"jkt,omitempty"`
}

// expectedJWS is what a compact JWS must hold to verify.
type expectedJWS struct {
	// typ is the header typ required, or empty where any is accepted.
	typ      string
	clientID string
	audience string
	now      time.Time
	// keyOf answers the key a header kid names, where that key may sign.
	keyOf func(keyID) (*rsa.PublicKey, bool)
}

// verifyJWS proves compact is an RS256 JWS, signed by the key its kid names, whose iss and sub are the
// client, whose aud names the audience, which carries a jti, and which is unexpired with exp at most
// 60 seconds after iat. It answers the kid and the claims.
func verifyJWS(compact string, want expectedJWS) (keyID, signedClaims, error) {
	token, err := jwt.ParseSigned(compact, []jose.SignatureAlgorithm{jose.RS256})
	if err != nil {
		return "", signedClaims{}, errors.New("not a compact RS256 JWS")
	}
	header := token.Headers[0]
	if want.typ != "" {
		if typ, _ := header.ExtraHeaders[jose.HeaderType].(string); typ != want.typ {
			return "", signedClaims{}, fmt.Errorf("typ is not %q", want.typ)
		}
	}
	kid := keyID(header.KeyID)
	key, ok := want.keyOf(kid)
	if !ok {
		return "", signedClaims{}, errors.New("kid names no key that may sign this")
	}
	var claims signedClaims
	if err := token.Claims(key, &claims); err != nil {
		return "", signedClaims{}, errors.New("signature does not verify")
	}
	if claims.ID == "" || claims.IssuedAt == nil || claims.Expiry == nil {
		return "", signedClaims{}, errors.New("jti, iat or exp is missing")
	}
	if claims.Expiry.Time().Sub(claims.IssuedAt.Time()) > maxSignedLifetime {
		return "", signedClaims{}, errors.New("exp is more than 60 seconds after iat")
	}
	err = claims.ValidateWithLeeway(jwt.Expected{
		Issuer:      want.clientID,
		Subject:     want.clientID,
		AnyAudience: jwt.Audience{want.audience},
		Time:        want.now,
	}, 0)
	if err != nil {
		return "", signedClaims{}, fmt.Errorf("claims do not hold: %w", err)
	}
	return kid, claims, nil
}
