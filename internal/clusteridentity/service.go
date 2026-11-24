package clusteridentity

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"
)

const (
	// keyBits is the size of every key generated here (ADR 0088 decision 3).
	keyBits = 3072
	// rotationPeriod is how long a key signs before the connector rotates it (ADR 0088 decision 6).
	rotationPeriod = 30 * 24 * time.Hour
	// defaultTokenPoll is how often a connector waiting for a new registration token reads the file again.
	defaultTokenPoll = 10 * time.Second
)

// Config is what a Service is built from.
type Config struct {
	// BaseURL is the cluster-connection base URL, without a trailing slash (T-3).
	BaseURL string
	// HTTP trusts the configured root and nothing an answer carries (T-2). It presents no credential.
	HTTP *http.Client
	// TokenFile holds the registration token. It is read only while the cluster is not registered.
	TokenFile string
	// Version is the connector's version, sent at registration (REG-1).
	Version string
	Store   KeyStore
	Log     *slog.Logger
	// Now, Random, Sleep, GenerateKey and TokenPoll default to the clock, crypto/rand, a timer, an RSA
	// 3072 key from crypto/rand, and 10 seconds.
	Now         func() time.Time
	Random      io.Reader
	Sleep       func(ctx context.Context, d time.Duration) error
	GenerateKey func() (*rsa.PrivateKey, error)
	TokenPoll   time.Duration
}

// Service establishes a cluster's identity at garam.
type Service interface {
	// Establish answers the cluster's Connection: from the stored State, or by registering once with the
	// token file's token. It waits for a new token after a refused or unanswered registration (REG-7),
	// and resolves a rotation a restart interrupted (ROT-5).
	Establish(ctx context.Context) (Connection, error)
}

// Connection is an established identity.
type Connection interface {
	Cluster() GRN
	// Client carries the record's token on every request, and exchanges once more on a 401 (AUTH-1, AUTH-3).
	Client() *http.Client
	// Rotate replaces the record's key now (ROT-1 to ROT-5).
	Rotate(ctx context.Context) error
	// Run rotates the key every 30 days until ctx ends.
	Run(ctx context.Context) error
}

type service struct {
	cfg Config
}

// NewService returns a Service established under cfg.
func NewService(cfg Config) Service {
	if cfg.Log == nil {
		cfg.Log = slog.New(slog.DiscardHandler)
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Random == nil {
		cfg.Random = rand.Reader
	}
	if cfg.Sleep == nil {
		cfg.Sleep = sleep
	}
	if cfg.GenerateKey == nil {
		cfg.GenerateKey = func() (*rsa.PrivateKey, error) { return rsa.GenerateKey(rand.Reader, keyBits) }
	}
	if cfg.TokenPoll <= 0 {
		cfg.TokenPoll = defaultTokenPoll
	}
	cfg.BaseURL = strings.TrimRight(cfg.BaseURL, "/")
	return &service{cfg: cfg}
}

// sleep waits d, or until ctx ends.
func sleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
