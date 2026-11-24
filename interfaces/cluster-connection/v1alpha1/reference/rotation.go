package reference

import (
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"time"
)

const (
	// rolloverPeriod is how long a predecessor stays live after a rotation (ROT-3).
	rolloverPeriod = 24 * time.Hour
	// maxRotationBytes bounds a rotation body.
	maxRotationBytes = 64 << 10
	// The typ headers of ROT-1's two JWSs.
	authorizationType = "key-rotation+jwt"
	proofType         = "key-proof+jwt"
)

// keyRotationRequest is KeyRotationRequest of openapi.yaml.
type keyRotationRequest struct {
	JWK           json.RawMessage `json:"jwk"`
	Authorization string          `json:"authorization"`
	Proof         string          `json:"proof"`
}

// KeyRotation is ROT-3's answer.
type KeyRotation struct {
	KID                  keyID     `json:"kid"`
	PredecessorRetiresAt time.Time `json:"predecessorRetiresAt"`
}

func (s *Server) rotateKey(w http.ResponseWriter, r *http.Request) error {
	if err := s.authenticateFirst(r); err != nil {
		return err
	}
	var req keyRotationRequest
	if err := decodeBody(w, r, maxRotationBytes, bodyTooLarge, &req); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.authenticate(r)
	if err != nil {
		return err
	}
	successor, err := parsePublicJWK(req.JWK)
	if err != nil {
		return refuse(http.StatusBadRequest, codeInvalidRequest, "%s", err)
	}
	now := s.cfg.Now()
	live := c.liveKeys(now)
	audience := s.cfg.BaseURL + keysPath
	invalid := func(which string, reason any) error {
		return refuse(http.StatusBadRequest, codeInvalidProof, "%s: %v", which, reason)
	}

	_, authorization, err := verifyJWS(req.Authorization, expectedJWS{
		typ: authorizationType, clientID: string(c.grn), audience: audience, now: now,
		keyOf: func(id keyID) (*rsa.PublicKey, bool) {
			k, ok := live[id]
			if !ok {
				return nil, false
			}
			return k.public.key, true
		},
	})
	if err != nil {
		return invalid("authorization", err)
	}
	if authorization.JKT != string(successor.id) {
		return invalid("authorization", "jkt is not the thumbprint of jwk")
	}
	_, proof, err := verifyJWS(req.Proof, expectedJWS{
		typ: proofType, clientID: string(c.grn), audience: audience, now: now,
		keyOf: func(id keyID) (*rsa.PublicKey, bool) { return successor.key, id == successor.id },
	})
	if err != nil {
		return invalid("proof", err)
	}
	if s.replayed(audience, authorization) || s.replayed(audience, proof) {
		return invalid("authorization or proof", "jti was seen before")
	}

	if existing, ok := live[successor.id]; ok && existing.rotation != nil {
		writeJSON(w, http.StatusCreated, existing.rotation)
		return nil
	}
	if c.held[successor.id] {
		return refuse(http.StatusBadRequest, codeInvalidRequest, "jwk is a key the record has held, not a new successor")
	}
	if len(live) >= 2 {
		return refuse(http.StatusConflict, codeTooManyKeys, "the record already holds two live keys")
	}
	retires := now.Add(rolloverPeriod).UTC()
	for _, k := range live {
		if k.retiresAt.IsZero() || k.retiresAt.After(retires) {
			k.retiresAt = retires
		}
	}
	answer := &KeyRotation{KID: successor.id, PredecessorRetiresAt: retires}
	c.keys[successor.id] = &recordKey{public: successor, rotation: answer}
	c.held[successor.id] = true
	writeJSON(w, http.StatusCreated, answer)
	return nil
}
