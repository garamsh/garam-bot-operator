package clusteridentity

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
)

// keepAttempts is how often the record a registration answered is saved before the connector stops.
const keepAttempts = 5

// registration is REG-6's answer.
type registration struct {
	Cluster  GRN    `json:"cluster"`
	ClientID string `json:"clientId"`
	TokenURL string `json:"tokenUrl"`
}

func (s *service) Establish(ctx context.Context) (Connection, error) {
	state, err := s.cfg.Store.Load(ctx)
	if errors.Is(err, ErrNoState) {
		// The key is stored with the first registration attempt, before it is ever sent.
		var key *rsa.PrivateKey
		key, err = s.cfg.GenerateKey()
		state = State{Key: key}
	}
	if err != nil {
		return nil, fmt.Errorf("load the cluster's identity: %w", err)
	}
	for state.Record == nil {
		if state, err = s.register(ctx, state); err != nil {
			return nil, err
		}
	}
	// A rotation a restart interrupted is Run's to resolve: the current key is live until it is.
	return newConnection(s, state), nil
}

// register presents the token file's token once, unless it is the token presented last. It answers the
// State as it stands after the attempt, which holds a Record only on success.
func (s *service) register(ctx context.Context, state State) (State, error) {
	token, err := readToken(s.cfg.TokenFile)
	if err != nil && !s.tokenUnreadable {
		// By path alone: the error is the file system's, and the wait for the file is otherwise silent.
		s.cfg.Log.Error("the registration token file cannot be read; reading it again until it can",
			"path", s.cfg.TokenFile)
	}
	s.tokenUnreadable = err != nil
	attempt := tokenDigest(token)
	if err != nil || token == "" || attempt == state.Attempted {
		return state, s.cfg.Sleep(ctx, s.cfg.TokenPoll)
	}
	// Recorded, with the key, before the token is sent: the key a registration binds is never lost, and
	// neither an answer nor a lost one is ever followed by the same token.
	state.Attempted = attempt
	if err := s.cfg.Store.Save(ctx, state); err != nil {
		return state, fmt.Errorf("record the registration attempt: %w", err)
	}
	got := s.post(ctx, s.cfg.HTTP, registrationPath, map[string]any{
		"registrationToken": token,
		"jwk":               publicJWK(state.Key),
		"connector":         map[string]string{"version": s.cfg.Version},
	})
	var answered registration
	switch {
	case got.outcome == throttled:
		// REG-7: a 429 spent nothing, so the same token is presented again after the wait.
		state.Attempted = ""
		if err := s.cfg.Store.Save(ctx, state); err != nil {
			return state, fmt.Errorf("clear the throttled registration attempt: %w", err)
		}
		s.cfg.Log.Warn("registration throttled; presenting the token again after the wait", "retry_after", got.retryAfter)
		return state, s.cfg.Sleep(ctx, got.retryAfter)
	case got.outcome != accepted:
		s.cfg.Log.Error("registration failed; waiting for a new registration token", "status", got.status)
		return state, nil
	case json.Unmarshal(got.body, &answered) != nil || answered.Cluster == "" || answered.ClientID == "" ||
		answered.TokenURL == "":
		s.cfg.Log.Error("registration answered without a record; waiting for a new registration token", "status", got.status)
		return state, nil
	}
	state.Record = &Record{Cluster: answered.Cluster, ClientID: answered.ClientID, TokenURL: answered.TokenURL}
	state.KeySince = s.cfg.Now()
	state.Attempted = ""
	if err := s.keepRecord(ctx, state); err != nil {
		return state, fmt.Errorf("keep the registered record: %w", err)
	}
	s.cfg.Log.Info("registered", "cluster", answered.Cluster)
	return state, nil
}

// keepRecord saves the State a registration answered, again after a failure, up to keepAttempts times:
// the token is spent, so a record lost here is lost until a new token is given.
func (s *service) keepRecord(ctx context.Context, state State) error {
	wait := firstBackoff
	for attempt := 1; ; attempt++ {
		err := s.cfg.Store.Save(ctx, state)
		if err == nil || errors.Is(err, ErrAnotherWriter) || attempt == keepAttempts {
			return err
		}
		s.cfg.Log.Warn("the registered record was not stored; storing it again", "error", err, "wait", wait)
		if err := s.cfg.Sleep(ctx, wait); err != nil {
			return err
		}
		wait = min(2*wait, lastBackoff)
	}
}

// readToken reads the registration token file, without the line end a deployer's tooling may have added.
func readToken(path string) (string, error) {
	read, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read the registration token file: %w", err)
	}
	return strings.TrimSpace(string(read)), nil
}

// tokenDigest is the SHA-256 of a token in hex: what is kept of it, never the token.
func tokenDigest(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
