//go:build conformance

package client

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	botv1 "github.com/garamsh/garam-bot/api/declaration/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"

	"github.com/garamsh/garam-agent-operator/interfaces/cluster-connection/v1alpha1/reference"
)

// marked requires o to be garam-owned, marked as the record's object for id at generation.
func marked(t *testing.T, o *unstructured.Unstructured, record reference.GRN, id string, generation int64) {
	t.Helper()
	management, err := botv1.ManagementOf(o)
	require.NoError(t, err, "DECL-11: the marker is interface 1's")
	declared, garamOwned := management.Garam()
	require.True(t, garamOwned, "DECL-11: owned by garam")
	assert.Equal(t, string(record), declared.Cluster, "DECL-11: the marker names the record")
	assert.Equal(t, id, declared.ID, "DECL-11: and the declaration")
	assert.Equal(t, generation, declared.Generation, "DECL-11: and its generation")
}

func markedBy(record string) func(*unstructured.Unstructured) { return ownedBy(record, "theirs") }

// ownedBy marks an object as record's, for its declaration id at generation 1.
func ownedBy(record, id string) func(*unstructured.Unstructured) {
	return func(o *unstructured.Unstructured) {
		if err := botv1.MarkGaramOwned(o, botv1.GaramDeclaration{Cluster: record, ID: id, Generation: 1}); err != nil {
			panic(err)
		}
	}
}

const anotherRecord = "grn:acme/iam/cluster/another"

func appliesFullSet(t *testing.T) {
	h := newHarness(t)
	ns := h.watched[0]
	a, b := h.declare("", bot(ns, "a", "reviewer")), h.declare("", bot(ns, "b", "writer"))

	h.start()

	assert.Equal(t, "reviewer", role(h.applied(ns, "a")), "DECL-1: the full set is applied")
	assert.Equal(t, "writer", role(h.applied(ns, "b")))
	h.report(func(r map[string]any) bool { return hasOutcome(a, "applied")(r) && hasOutcome(b, "applied")(r) },
		"each declaration is reported applied")
}

// waitsForMoreFalse holds the full set's last page back, and requires the client not to take the set
// as complete before it: an object only that page names is not reported orphaned in between.
func waitsForMoreFalse(t *testing.T) {
	h := newHarness(t, pageSize(1))
	ns := h.watched[0]
	h.declare("", bot(ns, "a", "reviewer"))
	h.declare("", bot(ns, "b", "reviewer"))
	last := h.declare("", bot(ns, "c", "reviewer"))
	h.put(ns, "c", "reviewer", ownedBy(string(h.record), last))
	var count atomic.Int32
	held := make(chan struct{})
	h.setIntercept(func(_ http.ResponseWriter, r *http.Request, _ []byte, _ http.Handler) bool {
		if r.URL.Path == declarationsPath && count.Add(1) == 3 {
			<-held
		}
		return false
	})

	h.start()

	require.Eventually(t, func() bool { return count.Load() >= 3 }, eventually, tick,
		"DECL-5: the client asks again for each page while more is true")
	time.Sleep(3 * time.Second)
	for _, r := range h.reports() {
		if c, ok := reportedObjects(r)["c"]; ok {
			assert.NotEqual(t, true, c["orphaned"], "DECL-5: the full set is not complete before more is false")
		}
	}
	close(held)
	h.report(func(r map[string]any) bool { return carries("a", "b", "c")(r) && hasOutcome(last, "applied")(r) },
		"control: the whole set is applied once its last page arrives")
}

func rereadsOn410(t *testing.T) {
	h := newHarness(t)
	ns := h.watched[0]
	h.declare("", bot(ns, "a", "reviewer"))
	h.start()
	h.applied(ns, "a")
	expired := time.Now()

	h.fix("expire-cursors", reference.FixtureRequest{Cluster: h.record})

	require.Eventually(t, func() bool {
		for _, s := range declarationsAfter(h, expired) {
			if !strings.Contains(s.query, "after=") {
				return true
			}
		}
		return false
	}, eventually, tick, "DECL-6: after a 410 the client reads the full set")
	h.declare("", bot(ns, "b", "reviewer"))
	h.applied(ns, "b")
}

// injected answers the next declarations request with changes, keeping the cursor it was asked after.
func injected(changes ...map[string]any) intercept {
	var done atomic.Bool
	return func(w http.ResponseWriter, r *http.Request, _ []byte, _ http.Handler) bool {
		after := r.URL.Query().Get("after")
		if r.URL.Path != declarationsPath || after == "" || !done.CompareAndSwap(false, true) {
			return false
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"cursor": after, "changes": changes})
		return true
	}
}

func ignoresLowerGeneration(t *testing.T) {
	h := newHarness(t)
	ns := h.watched[0]
	id := h.declare("", bot(ns, "a", "first"))
	h.declare(id, bot(ns, "a", "second"))
	h.start()
	require.Eventually(t, func() bool {
		o, ok := h.object(ns, "a")
		return ok && role(o) == "second"
	}, eventually, tick, "setup: generation 2 is applied")

	h.setIntercept(injected(map[string]any{"op": "apply", "id": id, "generation": 1, "object": bot(ns, "a", "first")}))

	require.Never(t, func() bool {
		o, ok := h.object(ns, "a")
		return ok && role(o) != "second"
	}, 5*time.Second, tick, "DECL-10: a lower generation is not applied")
	h.declare(id, bot(ns, "a", "third"))
	require.Eventually(t, func() bool {
		o, ok := h.object(ns, "a")
		return ok && role(o) == "third"
	}, eventually, tick, "control: a higher one is")
}

func marksOwnRecord(t *testing.T) {
	h := newHarness(t)
	ns := h.watched[0]
	id := h.declare("", bot(ns, "a", "reviewer"))
	h.declare(id, bot(ns, "a", "writer"))

	h.start()

	require.Eventually(t, func() bool {
		o, ok := h.object(ns, "a")
		return ok && role(o) == "writer"
	}, eventually, tick)
	o, _ := h.object(ns, "a")
	marked(t, o, h.record, id, 2)
}

// conflicting requires a declaration whose name a Bot already holds to be reported as a conflict and
// that Bot to be left as it was.
func conflicting(t *testing.T, mark func(*unstructured.Unstructured)) {
	t.Helper()
	h := newHarness(t)
	ns := h.watched[0]
	held := h.put(ns, "a", "kept", mark)
	id := h.declare("", bot(ns, "a", "declared"))
	other := h.declare("", bot(ns, "b", "declared"))

	h.start()

	h.report(hasOutcome(id, "conflict"), "DECL-11: reported as a conflict")
	h.applied(ns, "b")
	h.report(hasOutcome(other, "applied"), "control: a declaration of a free name is applied")
	o, _ := h.object(ns, "a")
	assert.Equal(t, "kept", role(o))
	assert.Equal(t, held.GetResourceVersion(), o.GetResourceVersion(), "DECL-14: not changed")
}

func refusesNameHeldByCluster(t *testing.T) { conflicting(t, nil) }

func refusesNameHeldByAnotherRecord(t *testing.T) { conflicting(t, markedBy(anotherRecord)) }

func refusesInvalid(t *testing.T) {
	h := newHarness(t)
	ns := h.watched[0]
	invalid := bot(ns, "invalid", "reviewer")
	invalid["spec"].(map[string]any)["agent"].(map[string]any)["image"] = "registry.example/bot:1"
	h.start()
	h.connected()
	h.setIntercept(injected(map[string]any{"op": "apply", "id": "invalid-1", "generation": 1, "object": invalid}))
	valid := h.declare("", bot(ns, "valid", "reviewer"))

	h.report(hasOutcome("invalid-1", "invalid"), "DECL-12: reported as invalid")
	h.applied(ns, "valid")
	h.report(hasOutcome(valid, "applied"), "control")
	_, applied := h.object(ns, "invalid")
	assert.False(t, applied, "DECL-12: not applied")
}

func reportsUnwatchedNamespace(t *testing.T) {
	h := newHarness(t)
	unwatched := fmt.Sprintf("unwatched-%d", serial.Add(1))
	h.namespace(unwatched)
	id := h.declare("", bot(unwatched, "a", "reviewer"))
	h.start()

	h.report(hasOutcome(id, "namespaceNotWatched"), "DECL-12: reported namespaceNotWatched")
	_, applied := h.object(unwatched, "a")
	assert.False(t, applied)
}

func deletesOnlyOnTombstone(t *testing.T) {
	h := newHarness(t)
	ns := h.watched[0]
	id := h.declare("", bot(ns, "a", "reviewer"))
	h.start()
	h.applied(ns, "a")

	h.fix("tombstone", reference.FixtureRequest{ID: id})

	require.Eventually(t, func() bool {
		_, ok := h.object(ns, "a")
		return !ok
	}, eventually, tick, "DECL-13: deleted on its tombstone")
}

// reportsOrphan has the client read a full set that does not name an object it applied, with the
// tombstone purged before the client saw it: the object stays and is reported orphaned.
func reportsOrphan(t *testing.T) {
	h := newHarness(t)
	ns := h.watched[0]
	id := h.declare("", bot(ns, "a", "reviewer"))
	h.start()
	h.applied(ns, "a")
	release := make(chan struct{})
	var heldAt atomic.Int64
	h.setIntercept(func(_ http.ResponseWriter, r *http.Request, _ []byte, _ http.Handler) bool {
		if r.URL.Path == declarationsPath {
			heldAt.CompareAndSwap(0, time.Now().UnixNano())
			<-release
		}
		return false
	})
	h.declare("", bot(ns, "b", "reviewer"))
	h.applied(ns, "b")
	require.Eventually(t, func() bool { return heldAt.Load() != 0 }, eventually, tick, "setup: the next poll is held")

	h.fix("tombstone", reference.FixtureRequest{ID: id})
	h.fix("expire-cursors", reference.FixtureRequest{Cluster: h.record})
	close(release)

	h.report(func(r map[string]any) bool {
		a, ok := reportedObjects(r)["a"]
		return ok && a["orphaned"] == true
	}, "DECL-13: reported orphaned")
	_, kept := h.object(ns, "a")
	assert.True(t, kept, "DECL-13: not deleted because a full set does not name it")
}

func leavesClusterOwnedAlone(t *testing.T) {
	h := newHarness(t)
	ns := h.watched[0]
	made := h.put(ns, "made-here", "kept", nil)
	theirs := h.put(ns, "theirs", "kept", markedBy(anotherRecord))
	id := h.declare("", bot(ns, "a", "reviewer"))
	h.start()
	h.applied(ns, "a")

	h.fix("tombstone", reference.FixtureRequest{ID: id})
	require.Eventually(t, func() bool {
		_, ok := h.object(ns, "a")
		return !ok
	}, eventually, tick, "setup: the tombstone was carried out")
	h.fix("expire-cursors", reference.FixtureRequest{Cluster: h.record})
	h.declare("", bot(ns, "after-the-full-set", "reviewer"))
	h.applied(ns, "after-the-full-set")

	for _, held := range []*unstructured.Unstructured{made, theirs} {
		o, ok := h.object(ns, held.GetName())
		require.True(t, ok, "DECL-14: %s is not deleted", held.GetName())
		assert.Equal(t, held.GetResourceVersion(), o.GetResourceVersion(), "DECL-14: %s is not changed", held.GetName())
	}
}

func reportOnlyAppliesNothing(t *testing.T) {
	h := newHarness(t)
	supports(t, "report-only", "a client that reports only")
	ns := h.watched[0]
	own := h.declare("", bot(ns, "own", "reviewer"))
	h.put(ns, "own", "reviewer", ownedBy(string(h.record), own))
	h.declare("", bot(ns, "a", "reviewer"))

	h.start(reportingOnly())

	h.report(func(r map[string]any) bool {
		connector, _ := r["connector"].(map[string]any)
		return connector["mode"] == "reportOnly" && carries("own")(r)
	}, "DECL-15: it still reports, as reportOnly")
	h.fix("tombstone", reference.FixtureRequest{ID: own})
	require.Never(t, func() bool {
		_, applied := h.object(ns, "a")
		_, kept := h.object(ns, "own")
		return applied || !kept
	}, 5*time.Second, tick, "DECL-15: nothing applied and nothing deleted")
}
