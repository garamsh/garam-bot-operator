package clusteridentity

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
)

const (
	protocolPrefix   = "/cluster-connection/v1alpha1"
	registrationPath = protocolPrefix + "/registrations"
	keysPath         = protocolPrefix + "/clusters/self/keys"
	// signedLifetime is exp - iat of ROT-1's JWSs, at most 60 seconds.
	signedLifetime = 60 * time.Second
	// maxAnswerBytes bounds an answer read.
	maxAnswerBytes = 64 << 10
	// jtiBytes is the random length of a jti: 256 bits.
	jtiBytes = 32
)

// outcome is how a request was answered, as T-5 and T-6 have a client act on it: by status alone.
type outcome int

const (
	// accepted is a 2xx.
	accepted outcome = iota + 1
	// throttled is a 429, retried after its Retry-After (T-6).
	throttled
	// conflicted is a 409.
	conflicted
	// refused is any other 4xx but 401.
	refused
	// lost is a 5xx, no answer, or a 401 the one re-exchange did not cure: each is backed off (T-6, AUTH-3).
	lost
)

// answer is one request's outcome, its status, the wait a 429 asked for, and the body of a 2xx.
type answer struct {
	outcome    outcome
	status     int
	retryAfter time.Duration
	body       []byte
}

// post sends body as JSON to path over client, and reads the answer by its status alone. A request
// unanswered within RequestTimeout is lost, so no retry loop waits on one send for good.
func (s *service) post(ctx context.Context, client *http.Client, path string, body any) answer {
	ctx, cancel := context.WithTimeout(ctx, s.cfg.RequestTimeout)
	defer cancel()
	encoded, err := json.Marshal(body)
	if err != nil {
		return answer{outcome: lost}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.BaseURL+path, bytes.NewReader(encoded))
	if err != nil {
		return answer{outcome: lost}
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return answer{outcome: lost}
	}
	defer func() { _ = response.Body.Close() }()
	read, err := io.ReadAll(io.LimitReader(response.Body, maxAnswerBytes))
	status := response.StatusCode
	switch {
	case status >= 200 && status < 300 && err == nil:
		return answer{outcome: accepted, status: status, body: read}
	case status == http.StatusTooManyRequests:
		return answer{outcome: throttled, status: status, retryAfter: retryAfter(response.Header.Get("Retry-After"))}
	case status == http.StatusUnauthorized:
		return answer{outcome: lost, status: status}
	case status == http.StatusConflict:
		return answer{outcome: conflicted, status: status}
	case status >= 400 && status < 500:
		return answer{outcome: refused, status: status}
	}
	return answer{outcome: lost, status: status}
}

// retryAfter reads Retry-After as seconds, and as T-6's first wait where it is missing or unreadable.
func retryAfter(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds < 1 {
		return firstBackoff
	}
	return time.Duration(seconds) * time.Second
}

// publicJWK is REG-1's and ROT-1's jwk: the public half alone (REG-2).
func publicJWK(key *rsa.PrivateKey) jose.JSONWebKey {
	return jose.JSONWebKey{Key: &key.PublicKey, Algorithm: string(jose.RS256), Use: "sig"}
}

// thumbprint is a key's RFC 7638 SHA-256 thumbprint, base64url without padding: its kid.
func thumbprint(key *rsa.PrivateKey) (string, error) {
	sum, err := (&jose.JSONWebKey{Key: &key.PublicKey}).Thumbprint(crypto.SHA256)
	if err != nil {
		return "", fmt.Errorf("compute a key's thumbprint: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(sum), nil
}

// rotationClaims are ROT-1's claims: the registered ones, and the successor's thumbprint where set.
type rotationClaims struct {
	jwt.Claims
	JKT string `json:"jkt,omitempty"`
}

// signRotation signs one of ROT-1's JWSs by key, typed typ, for client, carrying jkt where set.
func (s *service) signRotation(key *rsa.PrivateKey, typ, client, jkt string) (string, error) {
	kid, err := thumbprint(key)
	if err != nil {
		return "", err
	}
	signer, err := jose.NewSigner(
		jose.SigningKey{Algorithm: jose.RS256, Key: jose.JSONWebKey{Key: key, KeyID: kid}},
		(&jose.SignerOptions{}).WithType(jose.ContentType(typ)),
	)
	if err != nil {
		return "", fmt.Errorf("prepare a rotation signer: %w", err)
	}
	nonce := make([]byte, jtiBytes)
	if _, err := io.ReadFull(s.cfg.Random, nonce); err != nil {
		return "", fmt.Errorf("draw a jti: %w", err)
	}
	issued := s.cfg.Now().Truncate(time.Second)
	return jwt.Signed(signer).Claims(rotationClaims{
		Issuer:   client,
		Subject:  client,
		Audience: jwt.Audience{s.cfg.BaseURL + keysPath},
		IssuedAt: jwt.NewNumericDate(issued),
		Expiry:   jwt.NewNumericDate(issued.Add(signedLifetime)),
		ID:       base64.RawURLEncoding.EncodeToString(nonce),
		JKT:      jkt,
	}).Serialize()
}
