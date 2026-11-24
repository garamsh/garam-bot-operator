package clusteridentity

import (
	"context"
	"crypto/rsa"
	"errors"
	"net/http"
	"sync"

	"github.com/garamsh/garam-agent-operator/internal/machinetoken"
)

// connection is an established identity. The key lives here and in the KeyStore, and nowhere else.
type connection struct {
	s *service

	// rotating is held for the whole of a rotation, so two never run at once.
	rotating sync.Mutex

	mu    sync.Mutex
	state State
	// source exchanges under state.Key. A promotion replaces it, so no token of the predecessor is held on.
	source *machinetoken.Source
}

func newConnection(s *service, state State) *connection {
	c := &connection{s: s, state: state}
	c.source = s.newSource(c, state.Record.TokenURL)
	return c
}

func (c *connection) Cluster() GRN {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state.Record.Cluster
}

// Signer is the machinetoken.Identity every exchange of this connection signs as: the current key.
func (c *connection) Signer() (machinetoken.Signer, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return signerOf(c.state.Key, *c.state.Record), nil
}

// Client follows redirects as the configured client does: the token is carried on every hop, so the
// connector's client follows none.
func (c *connection) Client() *http.Client {
	return &http.Client{Transport: bearer{c: c, base: c.s.transport()}, CheckRedirect: c.s.cfg.HTTP.CheckRedirect}
}

// set replaces the state held, and the source with it where the key changed.
func (c *connection) set(state State) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if state.Key != c.state.Key {
		c.source = c.s.newSource(c, state.Record.TokenURL)
	}
	c.state = state
}

// save stores state. Where another process wrote the store since, nothing is written over it: the
// connection takes up what is stored, and the error is ErrAnotherWriter, on which Run stops.
func (c *connection) save(ctx context.Context, state State) error {
	err := c.s.cfg.Store.Save(ctx, state)
	if errors.Is(err, ErrAnotherWriter) {
		if stored, loadErr := c.s.cfg.Store.Load(ctx); loadErr == nil && stored.Record != nil {
			c.set(stored)
		}
	}
	return err
}

func (c *connection) current() (State, *machinetoken.Source) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.state, c.source
}

// bearer carries the connection's token, through the source held when each request is sent.
type bearer struct {
	c    *connection
	base http.RoundTripper
}

func (b bearer) RoundTrip(request *http.Request) (*http.Response, error) {
	_, source := b.c.current()
	return (&machinetoken.Transport{Source: source, Base: b.base}).RoundTrip(request)
}

// staticIdentity is an Identity of one key, for the exchange ROT-5 tries with a successor.
type staticIdentity machinetoken.Signer

func (s staticIdentity) Signer() (machinetoken.Signer, error) { return machinetoken.Signer(s), nil }

// signerOf is key signing as record's client.
func signerOf(key *rsa.PrivateKey, record Record) machinetoken.Signer {
	return machinetoken.Signer{Key: key, ClientID: record.ClientID, TokenURL: record.TokenURL}
}

func (s *service) newSource(identity machinetoken.Identity, tokenURL string) *machinetoken.Source {
	return machinetoken.NewSource(machinetoken.Config{
		Endpoint: tokenURL,
		Identity: identity,
		HTTP:     s.cfg.HTTP,
		Now:      s.cfg.Now,
		Random:   s.cfg.Random,
	})
}

// transport is the base every request is sent over: the configured client's, verifying the root.
func (s *service) transport() http.RoundTripper {
	if s.cfg.HTTP.Transport != nil {
		return s.cfg.HTTP.Transport
	}
	return http.DefaultTransport
}
