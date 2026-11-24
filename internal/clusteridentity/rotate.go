package clusteridentity

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/garamsh/garam-agent-operator/internal/machinetoken"
)

const (
	// firstBackoff and lastBackoff are T-6's wait after a 5xx, no answer, or a 409 the exchange could not
	// settle: 1 second, doubling to 60.
	firstBackoff = time.Second
	lastBackoff  = 60 * time.Second
	// rotationRetry is how long Run waits after a rotation that did not take place before trying again.
	rotationRetry = time.Hour
	// The typ headers of ROT-1's two JWSs.
	authorizationType = "key-rotation+jwt"
	proofType         = "key-proof+jwt"
)

func (c *connection) Rotate(ctx context.Context) error {
	c.rotating.Lock()
	defer c.rotating.Unlock()
	state, _ := c.current()
	// A successor already stored was sent, or may have been, by a rotation whose answer never arrived
	// here: a restart, or a rotation that stopped unresolved.
	interrupted := state.Successor != nil
	if !interrupted {
		successor, err := c.s.cfg.GenerateKey()
		if err != nil {
			return fmt.Errorf("generate a successor key: %w", err)
		}
		state.Successor = successor
		// ROT-5: the successor is stored before the rotation is sent.
		if err := c.save(ctx, state); err != nil {
			return fmt.Errorf("store the successor key: %w", err)
		}
		c.set(state)
	}
	return c.resolve(ctx, interrupted)
}

// resolve sends the stored rotation until garam settles it (ROT-5). interrupted is an earlier send whose
// answer never arrived, after which the successor is tried at the exchange before anything else.
func (c *connection) resolve(ctx context.Context, interrupted bool) error {
	if interrupted && c.successorExchange(ctx) == tookPlace {
		return c.promote(ctx)
	}
	wait := firstBackoff
	backOff := func(why string, status int) error {
		c.s.cfg.Log.Warn(why+"; sending the rotation again", "status", status, "wait", wait)
		if err := c.s.cfg.Sleep(ctx, wait); err != nil {
			return err
		}
		wait = min(2*wait, lastBackoff)
		return nil
	}
	for {
		got, err := c.sendRotation(ctx)
		if err != nil {
			return err
		}
		switch got.outcome {
		case accepted:
			return c.promote(ctx)
		case conflicted:
			// A 409 may answer a rotation that took place under an earlier, lost answer. Only an exchange
			// garam refused as invalid_client shows it did not; one that failed otherwise shows nothing.
			switch c.successorExchange(ctx) {
			case tookPlace:
				return c.promote(ctx)
			case notTaken:
				return c.abandon(ctx, ErrTooManyKeys)
			case unsettled:
				if err := backOff("key rotation answered 409, and the successor's exchange failed", got.status); err != nil {
					return err
				}
			}
		case refused:
			return c.abandon(ctx, fmt.Errorf("%w: answered %d", ErrRotationRefused, got.status))
		case throttled:
			if err := c.s.cfg.Sleep(ctx, got.retryAfter); err != nil {
				return err
			}
		case lost:
			if c.successorExchange(ctx) == tookPlace {
				return c.promote(ctx)
			}
			if err := backOff("key rotation unanswered", got.status); err != nil {
				return err
			}
		}
	}
}

// sendRotation sends ROT-1's request, with freshly signed JWSs: an authorization by the current key and
// a proof by the successor. A JWS that cannot be signed here is an error, never a request.
func (c *connection) sendRotation(ctx context.Context) (answer, error) {
	state, _ := c.current()
	client := state.Record.ClientID
	successorKID, err := thumbprint(state.Successor)
	if err != nil {
		return answer{}, err
	}
	authorization, err := c.s.signRotation(state.Key, authorizationType, client, successorKID)
	if err != nil {
		return answer{}, fmt.Errorf("sign the rotation's authorization: %w", err)
	}
	proof, err := c.s.signRotation(state.Successor, proofType, client, "")
	if err != nil {
		return answer{}, fmt.Errorf("sign the rotation's proof: %w", err)
	}
	return c.s.post(ctx, c.Client(), keysPath, map[string]any{
		"jwk":           publicJWK(state.Successor),
		"authorization": authorization,
		"proof":         proof,
	}), nil
}

// settled is what an exchange signed by the successor shows of a rotation.
type settled int

const (
	// tookPlace is a token exchanged: the successor is live.
	tookPlace settled = iota + 1
	// notTaken is an exchange garam refused as invalid_client: the successor is not live.
	notTaken
	// unsettled is an exchange that failed otherwise, which shows nothing: a 400 from a skewed clock, a 403,
	// a 429, a 5xx or no answer. Abandoning on one could drop a live successor.
	unsettled
)

// successorExchange tries an exchange signed by the successor (ROT-5).
func (c *connection) successorExchange(ctx context.Context) settled {
	state, _ := c.current()
	probe := c.s.newSource(staticIdentity(signerOf(state.Successor, *state.Record)), state.Record.TokenURL)
	_, err := probe.Token(ctx)
	var failed *machinetoken.Error
	switch {
	case err == nil:
		return tookPlace
	case errors.As(err, &failed) && failed.Reason == machinetoken.ReasonInvalidClient:
		return notTaken
	}
	return unsettled
}

// promote makes the successor the key the record signs with, from the next request on.
func (c *connection) promote(ctx context.Context) error {
	state, _ := c.current()
	state.Key, state.Successor, state.KeySince = state.Successor, nil, c.s.cfg.Now()
	c.set(state)
	if err := c.save(ctx, state); err != nil {
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
	if err := c.save(ctx, state); err != nil {
		return fmt.Errorf("%w; and drop the successor: %w", why, err)
	}
	return why
}

func (c *connection) Run(ctx context.Context) error {
	for {
		state, _ := c.current()
		// A successor stored is a rotation a restart, or an earlier one here, left unresolved: it is
		// resolved now, not at the next due date.
		if wait := state.KeySince.Add(c.s.cfg.RotationPeriod).Sub(c.s.cfg.Now()); wait > 0 && state.Successor == nil {
			if err := c.s.cfg.Sleep(ctx, wait); err != nil {
				return err
			}
		}
		err := c.Rotate(ctx)
		switch {
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.Is(err, ErrAnotherWriter):
			return err
		case err != nil:
			c.s.cfg.Log.Error("key rotation did not take place; the current key goes on signing", "error", err)
			if err := c.s.cfg.Sleep(ctx, rotationRetry); err != nil {
				return err
			}
		}
	}
}
