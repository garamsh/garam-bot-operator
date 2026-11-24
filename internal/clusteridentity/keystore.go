package clusteridentity

import "context"

// KeyStore keeps the State in one place in the cluster: the one Secret ADR 0088 decision 4 names.
type KeyStore interface {
	// Load answers the stored State, or ErrNoState.
	Load(ctx context.Context) (State, error)
	// Save replaces the stored State.
	Save(ctx context.Context, state State) error
}
