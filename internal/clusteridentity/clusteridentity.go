package clusteridentity

import (
	"crypto/rsa"
	"errors"
	"time"
)

// GRN names a cluster record, grn:<org>/iam/cluster/<id>.
type GRN string

// Record is what a registration answered (REG-6): the record, the client_id its tokens are exchanged
// under, and the tokenUrl they are exchanged at.
type Record struct {
	Cluster  GRN
	ClientID string
	TokenURL string
}

// State is what the KeyStore keeps. Key is always present, because it is stored before it is ever sent.
type State struct {
	// Key is the key the record signs with.
	Key *rsa.PrivateKey
	// KeySince is when Key became the record's key; the next rotation is due 30 days after it.
	KeySince time.Time
	// Record is nil until a registration has succeeded.
	Record *Record
	// Attempted is the SHA-256, in hex, of the registration token last presented, while Record is nil, so
	// the same token is never presented again after an answer or a lost one (REG-7). It holds no token.
	Attempted string
	// Successor is nil unless a rotation has been stored and not yet resolved (ROT-5).
	Successor *rsa.PrivateKey
}

var (
	// ErrNoState is a KeyStore holding no State yet.
	ErrNoState = errors.New("no cluster identity is stored")
	// ErrTooManyKeys is a rotation garam refused because the record already holds two live keys, none of
	// them the successor sent (ROT-4, ROT-5). The predecessor goes on signing.
	ErrTooManyKeys = errors.New("garam holds another key live on this record; the rotation did not take place")
	// ErrRotationRefused is a rotation garam refused for any other reason. The predecessor goes on signing.
	ErrRotationRefused = errors.New("garam refused the key rotation")
	// ErrAnotherWriter is a Save refused because another process wrote the KeyStore after this one read
	// it. Nothing is written over what the other wrote: the connection takes it up, and Run stops.
	ErrAnotherWriter = errors.New("another process wrote the cluster's identity after this one read it")
)
