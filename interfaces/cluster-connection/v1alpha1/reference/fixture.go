package reference

import (
	"encoding/json"
	"net/http"
)

// maxFixtureBytes bounds a fixture request; a declared object is its largest member.
const maxFixtureBytes = 1 << 20

// errNoCluster answers a fixture action naming no record this server holds.
var errNoCluster = refuse(http.StatusNotFound, codeNotFound, "no such cluster record")

// FixtureRequest is the body of every fixture action; each reads the members it names.
type FixtureRequest struct {
	Cluster GRN             `json:"cluster,omitempty"`
	Expired bool            `json:"expired,omitempty"`
	ID      string          `json:"id,omitempty"`
	Object  json.RawMessage `json:"object,omitempty"`
}

// ClusterChanged is the fixture's answer to revoke-keys and delete-cluster.
type ClusterChanged struct {
	Cluster GRN `json:"cluster"`
}

// Fixture serves the fixture subcommands of conformance.md §Suites, each as POST /<name>, over the
// same memory Handler serves. It authenticates nothing, so it is served only where the suite alone reaches it.
func (s *Server) Fixture() http.Handler {
	actions := map[string]func(FixtureRequest) (any, error){
		"mint-token": func(q FixtureRequest) (any, error) { return s.mintToken(q.Cluster, q.Expired) },
		"declare":    func(q FixtureRequest) (any, error) { return s.declare(q.Cluster, q.ID, q.Object) },
		"tombstone":  func(q FixtureRequest) (any, error) { return s.tombstone(q.ID) },
		"latest-report": func(q FixtureRequest) (any, error) {
			return s.latestReport(q.Cluster)
		},
		"revoke-keys":    func(q FixtureRequest) (any, error) { return s.revokeKeys(q.Cluster) },
		"delete-cluster": func(q FixtureRequest) (any, error) { return s.deleteCluster(q.Cluster) },
		"expire-cursors": func(q FixtureRequest) (any, error) { return s.expireCursors(q.Cluster) },
	}
	mux := http.NewServeMux()
	for name, act := range actions {
		mux.Handle("POST /"+name, serve(s.cfg.Log, func(w http.ResponseWriter, r *http.Request) error {
			var q FixtureRequest
			if err := decodeBody(w, r, maxFixtureBytes, bodyTooLarge, &q); err != nil {
				return err
			}
			answer, err := act(q)
			if err != nil {
				return err
			}
			writeJSON(w, http.StatusOK, answer)
			return nil
		}))
	}
	return mux
}

// revokeKeys withdraws every key of a record (AUTH-2).
func (s *Server) revokeKeys(grn GRN) (ClusterChanged, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clusters[grn]
	if !ok {
		return ClusterChanged{}, errNoCluster
	}
	c.keys = map[keyID]*recordKey{}
	c.notify()
	return ClusterChanged{Cluster: grn}, nil
}

// deleteCluster deletes a record (AUTH-2). Its declarations stay, so their identifiers are never placed again.
func (s *Server) deleteCluster(grn GRN) (ClusterChanged, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clusters[grn]
	if !ok {
		return ClusterChanged{}, errNoCluster
	}
	delete(s.clusters, grn)
	c.notify()
	return ClusterChanged{Cluster: grn}, nil
}
