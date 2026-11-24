package reference

import (
	"crypto/rsa"
	"errors"
	"net/http"
)

const (
	// contractHeader and contract name machine-token.v2 on an exchange's answer, as garam does.
	contractHeader = "Garam-Contract-Version"
	contract       = "machine-token.v2"
	// maxExchangeBytes bounds an exchange's form.
	maxExchangeBytes = 16 << 10
	assertionType    = "urn:ietf:params:oauth:client-assertion-type:jwt-bearer"
)

// oauthToken is RFC 6749's token answer.
type oauthToken struct {
	AccessToken string `json:"access_token"`
	TokenType   string `json:"token_type"`
	ExpiresIn   int64  `json:"expires_in"`
}

// oauthError is RFC 6749's error answer; this route answers in OAuth's shape, not T-5's.
type oauthError struct {
	Error       string `json:"error"`
	Description string `json:"error_description"`
}

// exchange answers a client_credentials grant carrying a private_key_jwt assertion signed by a live key
// of the record its client_id names (AUTH-1), with a token of accessTokenLifetime.
func (s *Server) exchange(w http.ResponseWriter, r *http.Request) {
	defer func() {
		if p := recover(); p != nil {
			s.cfg.Log.Error("exchange panicked", "trace_id", traceID(), "panic", p)
			writeJSON(w, http.StatusInternalServerError, oauthError{Error: "server_error", Description: internalError})
		}
	}()
	r.Body = http.MaxBytesReader(w, r.Body, maxExchangeBytes)
	if err := r.ParseForm(); err != nil {
		writeJSON(w, http.StatusBadRequest, oauthError{Error: "invalid_request", Description: "the body is not a form"})
		return
	}
	if r.PostForm.Get("grant_type") != "client_credentials" {
		writeJSON(w, http.StatusBadRequest,
			oauthError{Error: "unsupported_grant_type", Description: "only client_credentials"})
		return
	}
	if r.PostForm.Get("client_assertion_type") != assertionType {
		writeJSON(w, http.StatusBadRequest,
			oauthError{Error: "invalid_request", Description: "client_assertion_type is not jwt-bearer"})
		return
	}
	token, err := s.exchangeAssertion(r.PostForm.Get("client_id"), r.PostForm.Get("client_assertion"))
	if refused, ok := errors.AsType[*refusal](err); ok {
		writeJSON(w, http.StatusUnauthorized, oauthError{Error: "invalid_client", Description: refused.message})
		return
	}
	if err != nil {
		s.cfg.Log.Error("exchange failed", "trace_id", traceID(), "error", err)
		writeJSON(w, http.StatusInternalServerError, oauthError{Error: "server_error", Description: internalError})
		return
	}
	w.Header().Set(contractHeader, contract)
	writeJSON(w, http.StatusOK, oauthToken{
		AccessToken: token,
		TokenType:   "Bearer",
		ExpiresIn:   int64(accessTokenLifetime.Seconds()),
	})
}

// exchangeAssertion verifies an assertion and mints the token it is exchanged for.
func (s *Server) exchangeAssertion(clientID, assertion string) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	unknown := refuse(http.StatusUnauthorized, codeUnauthorized, "the client is unknown or holds no live key")
	c, ok := s.clusters[GRN(clientID)]
	if !ok {
		return "", unknown
	}
	now := s.cfg.Now()
	live := c.liveKeys(now)
	kid, claims, err := verifyJWS(assertion, expectedJWS{
		clientID: clientID,
		audience: s.tokenURL(),
		now:      now,
		keyOf: func(id keyID) (*rsa.PublicKey, bool) {
			k, ok := live[id]
			if !ok {
				return nil, false
			}
			return k.public.key, true
		},
	})
	if err != nil {
		return "", refuse(http.StatusUnauthorized, codeUnauthorized, "assertion: %s", err)
	}
	if s.replayed(s.tokenURL(), claims) {
		return "", refuse(http.StatusUnauthorized, codeUnauthorized, "assertion: jti was seen before")
	}
	token, err := s.secret()
	if err != nil {
		return "", err
	}
	s.accessTokens[digestOf(token)] = accessToken{cluster: c.grn, key: kid, expires: now.Add(accessTokenLifetime)}
	return token, nil
}
