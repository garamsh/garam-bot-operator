package clusteridentity

import (
	"context"
	"fmt"
	"time"
)

const (
	// firstBackoff and lastBackoff are T-6's wait after a 5xx or no answer: 1 second, doubling to 60.
	firstBackoff = time.Second
	lastBackoff  = 60 * time.Second
	// rotationRetry is how long Run waits after a rotation that did not take place before trying again.
	rotationRetry = time.Hour
	// The typ headers of ROT-1's two JWSs.
	authorizationType = "key-rotation+jwt"
	proofType         = "key-proof+jwt"
)

func (c *connection) Rotate(ctx context.Context) error {
	state, _ := c.current()
	if state.Successor == nil {
		successor, err := c.s.cfg.GenerateKey()
		if err != nil {
			return fmt.Errorf("generate a successor key: %w", err)
		}
		state.Successor = successor
		// ROT-5: the successor is stored before the rotation is sent.
		if err := c.s.cfg.Store.Save(ctx, state); err != nil {
			return fmt.Errorf("store the successor key: %w", err)
		}
		c.set(state)
	}
	return c.resolve(ctx, false)
}

// resolve sends the stored rotation until garam settles it (ROT-5). interrupted is an earlier send whose
// answer never arrived, after which the successor is tried at the exchange before anything else.
func (c *connection) resolve(ctx context.Context, interrupted bool) error {
	if interrupted && c.exchangesWithSuccessor(ctx) {
		return c.promote(ctx)
	}
	wait := firstBackoff
	for {
		got := c.sendRotation(ctx)
		switch got.outcome {
		case accepted:
			return c.promote(ctx)
		case conflicted:
			// A 409 may answer a rotation that took place under an earlier, lost answer.
			if c.exchangesWithSuccessor(ctx) {
				return c.promote(ctx)
			}
			return c.abandon(ctx, ErrTooManyKeys)
		case refused:
			return c.abandon(ctx, fmt.Errorf("%w: answered %d", ErrRotationRefused, got.status))
		case throttled:
			if err := c.s.cfg.Sleep(ctx, got.retryAfter); err != nil {
				return err
			}
		case lost:
			if c.exchangesWithSuccessor(ctx) {
				return c.promote(ctx)
			}
			c.s.cfg.Log.Warn("key rotation unanswered; sending it again", "status", got.status, "wait", wait)
			if err := c.s.cfg.Sleep(ctx, wait); err != nil {
				return err
			}
			wait = min(2*wait, lastBackoff)
		}
	}
}

// sendRotation sends ROT-1's request, with freshly signed JWSs: an authorization by the current key and
// a proof by the successor.
func (c *connection) sendRotation(ctx context.Context) answer {
	state, _ := c.current()
	client := state.Record.ClientID
	successorKID, err := thumbprint(state.Successor)
	if err != nil {
		return answer{outcome: lost}
	}
	authorization, err := c.s.signRotation(state.Key, authorizationType, client, successorKID)
	if err != nil {
		return answer{outcome: lost}
	}
	proof, err := c.s.signRotation(state.Successor, proofType, client, "")
	if err != nil {
		return answer{outcome: lost}
	}
	return c.s.post(ctx, c.Client(), keysPath, map[string]any{
		"jwk":           publicJWK(state.Successor),
		"authorization": authorization,
		"proof":         proof,
	})
}

// exchangesWithSuccessor answers whether an exchange signed by the successor succeeds, which means the
// rotation took place (ROT-5).
func (c *connection) exchangesWithSuccessor(ctx context.Context) bool {
	state, _ := c.current()
	probe := c.s.newSource(staticIdentity(signerOf(state.Successor, *state.Record)), state.Record.TokenURL)
	_, err := probe.Token(ctx)
	return err == nil
}

// promote makes the successor the key the record signs with, from the next request on.
func (c *connection) promote(ctx context.Context) error {
	state, _ := c.current()
	state.Key, state.Successor, state.KeySince = state.Successor, nil, c.s.cfg.Now()
	c.set(state)
	if err := c.s.cfg.Store.Save(ctx, state); err != nil {
		// The successor stays stored, so a restart finds the rotation interrupted and tries it again.
		return fmt.Errorf("keep the rotated key: %w", err)
	}
	c.s.cfg.Log.Info("rotated the cluster's key", "cluster", state.Record.Cluster)
	return nil
}

// abandon drops the successor, so the predecessor goes on signing, and answers why.
func (c *connection) abandon(ctx context.Context, why error) error {
	state, _ := c.current()
	state.Successor = nil
	c.set(state)
	if err := c.s.cfg.Store.Save(ctx, state); err != nil {
		return fmt.Errorf("%w; and drop the successor: %w", why, err)
	}
	return why
}

func (c *connection) Run(ctx context.Context) error {
	for {
		state, _ := c.current()
		if wait := state.KeySince.Add(rotationPeriod).Sub(c.s.cfg.Now()); wait > 0 {
			if err := c.s.cfg.Sleep(ctx, wait); err != nil {
				return err
			}
		}
		err := c.Rotate(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if err != nil {
			c.s.cfg.Log.Error("key rotation did not take place; the current key goes on signing", "error", err)
			if err := c.s.cfg.Sleep(ctx, rotationRetry); err != nil {
				return err
			}
		}
	}
}
