package reference

import (
	"encoding/json"
	"math"
	"net"
	"net/http"
	"time"
)

const (
	// registrationTokenLifetime is REG-5's bound.
	registrationTokenLifetime = time.Hour
	// maxRegistrationBytes bounds a registration body; a 3072-bit JWK is well under it.
	maxRegistrationBytes = 64 << 10
	// throttleWindow is the period RegistrationLimit counts over.
	throttleWindow = time.Minute
	// maxConnectorVersion is openapi.yaml's bound on connector.version.
	maxConnectorVersion = 64
)

// registrationRequest is RegistrationRequest of openapi.yaml. Only the token is decoded with the body,
// so a token the server could read is spent before anything else is judged (REG-3).
type registrationRequest struct {
	RegistrationToken *string         `json:"registrationToken"`
	JWK               json.RawMessage `json:"jwk"`
	Connector         json.RawMessage `json:"connector"`
}

// Registration is REG-6's answer.
type Registration struct {
	Cluster  GRN    `json:"cluster"`
	ClientID string `json:"clientId"`
	TokenURL string `json:"tokenUrl"`
}

// window counts the registration requests of one source address in one period.
type window struct {
	start time.Time
	count int
}

func (s *Server) register(w http.ResponseWriter, r *http.Request) error {
	if wait := s.throttled(r); wait > 0 {
		refused := refuse(http.StatusTooManyRequests, codeThrottled, "too many registrations from this address")
		refused.retryAfter = wait
		return refused
	}
	var req registrationRequest
	if err := decodeBody(w, r, maxRegistrationBytes, bodyTooLarge, &req); err != nil {
		return err
	}
	if req.RegistrationToken == nil || *req.RegistrationToken == "" {
		return refuse(http.StatusBadRequest, codeInvalidRequest, "registrationToken is missing")
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	spent := digestOf(*req.RegistrationToken)
	token, known := s.registrationTokens[spent]
	delete(s.registrationTokens, spent)
	c, exists := s.clusters[token.cluster]
	if !known || !exists || !s.cfg.Now().Before(token.expires) {
		return refuse(http.StatusUnauthorized, codeRegistrationRefused, "the registration token is spent, expired or unknown")
	}
	key, err := parsePublicJWK(req.JWK)
	if err != nil {
		return refuse(http.StatusBadRequest, codeInvalidRequest, "%s", err)
	}
	var connector struct {
		Version string `json:"version"`
	}
	if json.Unmarshal(req.Connector, &connector) != nil || connector.Version == "" ||
		len(connector.Version) > maxConnectorVersion {
		return refuse(http.StatusBadRequest, codeInvalidRequest, "connector.version is missing or over 64 characters")
	}
	c.keys = map[keyID]*recordKey{key.id: {public: key}}
	c.held[key.id] = true
	c.notify()
	writeJSON(w, http.StatusCreated, Registration{Cluster: c.grn, ClientID: string(c.grn), TokenURL: s.tokenURL()})
	return nil
}

// throttled counts a registration request against its source address, answering the seconds to wait
// when the address is past RegistrationLimit and 0 otherwise. It reads nothing of the body (REG-8).
func (s *Server) throttled(r *http.Request) int {
	if s.cfg.RegistrationLimit <= 0 {
		return 0
	}
	source, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		source = r.RemoteAddr
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.cfg.Now()
	counted := s.throttle[source]
	if now.Sub(counted.start) >= throttleWindow {
		counted = window{start: now}
	}
	if counted.count >= s.cfg.RegistrationLimit {
		remaining := counted.start.Add(throttleWindow).Sub(now)
		return max(1, int(math.Ceil(remaining.Seconds())))
	}
	counted.count++
	s.throttle[source] = counted
	return 0
}

// MintedToken is the fixture's mint-token answer.
type MintedToken struct {
	RegistrationToken string `json:"registrationToken"`
	Cluster           GRN    `json:"cluster"`
}

// mintToken mints a registration token for the record named, or for a new record where none is named.
// An expired token is minted as if past REG-5's hour.
func (s *Server) mintToken(grn GRN, expired bool) (MintedToken, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clusters[grn]
	if grn == "" {
		created, err := s.newCluster()
		if err != nil {
			return MintedToken{}, err
		}
		c, ok = created, true
	}
	if !ok {
		return MintedToken{}, errNoCluster
	}
	token, err := s.secret()
	if err != nil {
		return MintedToken{}, err
	}
	expires := s.cfg.Now().Add(registrationTokenLifetime)
	if expired {
		expires = s.cfg.Now().Add(-time.Second)
	}
	s.registrationTokens[digestOf(token)] = registrationToken{cluster: c.grn, expires: expires}
	return MintedToken{RegistrationToken: token, Cluster: c.grn}, nil
}
