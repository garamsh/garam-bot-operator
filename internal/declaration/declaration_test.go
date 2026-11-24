package declaration_test

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	botv1 "github.com/garamsh/garam-bot/api/declaration/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/garamsh/garam-agent-operator/interfaces/cluster-connection/v1alpha1/reference"
	"github.com/garamsh/garam-agent-operator/internal/declaration"
)

func ref(namespace, name string) declaration.Ref {
	return declaration.Ref{Kind: kindPool, Namespace: namespace, Name: name}
}

func TestRun_AppliesTheFullSetEachMarkedAsThisRecordsObject(t *testing.T) {
	h := newHarness(t)
	labelled := pool(watched, "a", 3)
	labelled["metadata"].(map[string]any)["labels"] = map[string]any{team: "a"}
	first := h.declare(poolA, labelled)
	h.declare("pool-b", pool(other, "b", 5))

	service := h.run(h.config())

	h.settled(service, func(s declaration.Status) bool { return len(s.Declarations) == 2 })
	applied, ok := h.objects.get(ref(watched, "a"))
	require.True(t, ok)
	marker, err := botv1.ManagementOf(applied)
	require.NoError(t, err)
	got, garamOwned := marker.Garam()
	require.True(t, garamOwned, "DECL-11: applied as a garam-owned object")
	assert.Equal(t, botv1.GaramDeclaration{Cluster: h.record, ID: poolA, Generation: first}, got)
	assert.Equal(t, int64(3), applied.Object["spec"].(map[string]any)[maxField])
	assert.Equal(t, map[string]string{team: "a", botv1.LabelManagedBy: string(botv1.OwnerGaram)}, applied.GetLabels(),
		"the declared labels, and the marker")
	_, ok = h.objects.get(ref(other, "b"))
	assert.True(t, ok)
	result, _ := outcome(mustStatus(t, service), poolA)
	assert.Equal(t, declaration.Result{ID: poolA, Generation: first, Outcome: declaration.Applied}, result)
}

func TestRun_AppliesAFullSetOnlyAtItsLastPage(t *testing.T) {
	h := newHarness(t)
	h.declare(poolA, pool(watched, "a", 1))
	h.declare("pool-b", pool(watched, "b", 1))
	release := make(chan struct{})
	var held atomic.Bool
	h.setIntercept(func(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
		if r.URL.Path != declPath {
			return false
		}
		switch r.URL.Query().Get("after") {
		case "":
			// The full set's first page, with more set.
			got := serveJSON(t, next, r)
			got["cursor"], got["more"] = "first-page", true
			writeJSON(t, w, got)
			return true
		case "first-page":
			held.Store(true)
			<-release
			// Its last page: the same changes, from no cursor, with more false.
			r.URL.RawQuery = "wait=0"
			next.ServeHTTP(w, r)
			return true
		}
		return false
	})

	service := h.run(h.config())

	require.Eventually(t, held.Load, eventually, tick)
	_, ready := service.Status()
	assert.False(t, ready, "DECL-5: a full set is not complete before the page with more false")
	assert.Empty(t, h.objects.written())
	close(release)
	h.settled(service, func(s declaration.Status) bool { return len(s.Declarations) == 2 })
}

func TestRun_ReadsTheFullSetAgainAfter410(t *testing.T) {
	h := newHarness(t)
	h.declare(poolA, pool(watched, "a", 1))
	service := h.run(h.config())
	h.settled(service, hasOutcome(poolA, declaration.Applied))

	h.fix("expire-cursors", referenceCluster(h))
	h.declare("pool-b", pool(watched, "b", 1))

	h.settled(service, hasOutcome("pool-b", declaration.Applied))
	full := 0
	for _, query := range h.pollsSent() {
		if !strings.Contains(query, "after=") {
			full++
		}
		assert.Contains(t, query, "wait=50", "every poll asks for DECL-3's longest hold")
	}
	assert.Equal(t, 2, full, "DECL-6: the full set was read again after the 410")
}

func TestRun_NeverAppliesALowerGenerationThanTheOneApplied(t *testing.T) {
	h := newHarness(t)
	h.objects.put(garamOwned(watched, "a", h.record, poolA, 7))
	generation := h.declare(poolA, pool(watched, "a", 9))
	require.Less(t, generation, int64(7), "setup: the declaration's generation is below the one applied")

	service := h.run(h.config())

	status := h.settled(service, hasOutcome(poolA, declaration.Applied))
	result, _ := outcome(status, poolA)
	assert.Equal(t, int64(7), result.Generation, "the generation applied is reported")
	assert.Empty(t, h.objects.written(), "DECL-10")
}

func TestRun_AppliesTheSameGenerationAgainOnEveryChange(t *testing.T) {
	h := newHarness(t)
	h.declare(poolA, pool(watched, "a", 3))
	service := h.run(h.config())
	h.settled(service, hasOutcome(poolA, declaration.Applied))
	require.Eventually(t, func() bool { return len(h.objects.written()) >= 1 }, eventually, tick)
	edited, _ := h.objects.get(ref(watched, "a"))
	edited.Object["spec"].(map[string]any)[maxField] = int64(99)

	h.objects.put(edited)

	require.Eventually(t, func() bool {
		got, _ := h.objects.get(ref(watched, "a"))
		return got.Object["spec"].(map[string]any)[maxField] == int64(3)
	}, eventually, tick, "the hand edit is reverted at the pass the change woke")
}

func TestRun_AppliesEveryDeclarationAgainEvery10Minutes(t *testing.T) {
	h := newHarness(t)
	h.declare(poolA, pool(watched, "a", 3))
	var mu sync.Mutex
	resync := make(chan time.Time)
	asked := map[time.Duration]int{}
	cfg := h.config()
	cfg.After = func(d time.Duration) <-chan time.Time {
		mu.Lock()
		defer mu.Unlock()
		asked[d]++
		if d == 10*time.Minute {
			return resync
		}
		return time.After(d)
	}
	service := h.run(cfg)
	h.settled(service, hasOutcome(poolA, declaration.Applied))
	require.Eventually(t, func() bool { return len(h.objects.asked()) >= 1 }, eventually, tick)
	asks := len(h.objects.asked())

	select {
	case resync <- time.Now():
	case <-time.After(eventually):
		t.Fatal("no pass waited on the 10-minute resync")
	}

	require.Eventually(t, func() bool { return len(h.objects.asked()) > asks }, eventually, tick,
		"every held declaration is applied again with nothing else to wake it")
	mu.Lock()
	defer mu.Unlock()
	assert.Positive(t, asked[10*time.Minute])
}

func TestRun_RefusesANameHeldByAnotherObject(t *testing.T) {
	// Each holder, and what the conflict's message says holds the name.
	cases := []struct {
		holder string
		says   string
		object func(h *harness) *unstructured.Unstructured
	}{
		{"a cluster-owned object", "a cluster-owned object", func(*harness) *unstructured.Unstructured {
			return object(watched, "a", nil)
		}},
		{"a garam-owned object of another record", "another cluster record", func(*harness) *unstructured.Unstructured {
			return garamOwned(watched, "a", "grn:acme/iam/cluster/another", poolA, 1)
		}},
		{"this record's object for another declaration", "declaration pool-z", func(h *harness) *unstructured.Unstructured {
			return garamOwned(watched, "a", h.record, "pool-z", 1)
		}},
		{"an object whose marker is malformed", "malformed", func(*harness) *unstructured.Unstructured {
			return object(watched, "a", map[string]string{botv1.LabelManagedBy: string(botv1.OwnerGaram)})
		}},
	}
	for _, c := range cases {
		t.Run(c.holder, func(t *testing.T) {
			h := newHarness(t)
			held := c.object(h)
			h.objects.put(held)
			h.declare(poolA, pool(watched, "a", 3))
			h.declare("pool-b", pool(watched, "b", 3))

			service := h.run(h.config())

			status := h.settled(service, func(s declaration.Status) bool {
				return hasOutcome(poolA, declaration.Conflict)(s) && hasOutcome("pool-b", declaration.Applied)(s)
			})
			result, _ := outcome(status, poolA)
			assert.Contains(t, result.Message, c.says)
			got, _ := h.objects.get(ref(watched, "a"))
			assert.Equal(t, held.Object["spec"], got.Object["spec"], "DECL-14: the object holding the name is left as it is")
			assert.Equal(t, held.GetLabels(), got.GetLabels())
			assert.NotContains(t, h.objects.written(), "apply BotPool bots/a")
			assert.Contains(t, h.objects.written(), "create BotPool bots/b", "control: the other declaration was applied")
		})
	}
}

func TestRun_RefusesAnInvalidDeclarationAndAppliesNothingOfIt(t *testing.T) {
	bot := map[string]any{
		"persona": map[string]any{"role": "r"}, "agent": map[string]any{"harness": "claude-code"},
		"permissions": map[string]any{"secrets": []any{"sk-live-not-a-reference"}},
	}
	cases := map[string]map[string]any{
		"an undeclared kind (DECL-7)": declaredObject("v1", "ConfigMap", watched, "a", map[string]any{}),
		"no namespace (DECL-7)":       declaredObject(declaration.APIVersion, kindPool, "", "a", map[string]any{maxField: 1}),
		"an image (DECL-9)": declaredObject(declaration.APIVersion, kindPool, watched, "a",
			map[string]any{maxField: 1, "image": "ghcr.io/x/y:1"}),
		"a secret value in place of a reference (DECL-8)": declaredObject(declaration.APIVersion, "Bot", watched, "a", bot),
	}
	for name, invalid := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.declare("pool-b", pool(watched, "b", 1))
			h.setIntercept(injectChange(t, poolA, 1, invalid))

			service := h.run(h.config())

			status := h.settled(service, func(s declaration.Status) bool {
				return hasOutcome(poolA, declaration.Invalid)(s) && hasOutcome("pool-b", declaration.Applied)(s)
			})
			result, _ := outcome(status, poolA)
			assert.NotEmpty(t, result.Message)
			assert.Equal(t, []string{"create BotPool bots/b"}, h.objects.written(), "DECL-12: nothing of it is applied")
		})
	}
}

func TestRun_ReportsADeclarationForANamespaceItDoesNotWatch(t *testing.T) {
	h := newHarness(t)
	h.declare(poolA, pool("elsewhere", "a", 1))

	service := h.run(h.config())

	h.settled(service, hasOutcome(poolA, declaration.NamespaceNotWatched))
	assert.Empty(t, h.objects.written())
}

func TestRun_TakesTheAPIServersRefusalAsInvalid(t *testing.T) {
	h := newHarness(t)
	h.objects.refuse = func(op string, o *unstructured.Unstructured) error {
		if o.GetName() == "a" {
			return apierrors.NewInvalid(schema.GroupKind{Group: "bot.garam.sh", Kind: kindPool}, "a", nil)
		}
		return nil
	}
	h.declare(poolA, pool(watched, "a", 1))

	service := h.run(h.config())

	status := h.settled(service, hasOutcome(poolA, declaration.Invalid))
	result, _ := outcome(status, poolA)
	assert.Contains(t, result.Message, "the API server refused the object")
}

func TestRun_DeletesOnlyOnATombstone(t *testing.T) {
	h := newHarness(t)
	h.declare(poolA, pool(watched, "a", 1))
	h.declare("pool-b", pool(watched, "b", 1))
	service := h.run(h.config())
	h.settled(service, func(s declaration.Status) bool { return len(s.Declarations) == 2 })

	h.tombstone(poolA)

	h.settled(service, func(s declaration.Status) bool { return len(s.Declarations) == 1 })
	require.Eventually(t, func() bool { _, ok := h.objects.get(ref(watched, "a")); return !ok }, eventually, tick)
	_, ok := h.objects.get(ref(watched, "b"))
	assert.True(t, ok, "control: the other declaration's object stays")
}

func TestRun_ReportsAnOwnObjectTheFullSetDoesNotNameAsOrphaned(t *testing.T) {
	h := newHarness(t)
	h.objects.put(garamOwned(watched, "gone", h.record, "pool-gone", 2))
	h.objects.put(garamOwned(watched, "theirs", "grn:acme/iam/cluster/another", "pool-x", 1))
	// The object of a declaration held, at a name its latest generation no longer names.
	h.objects.put(garamOwned(watched, "renamed", h.record, poolA, 1))
	h.declare(poolA, pool(watched, "a", 1))

	service := h.run(h.config())

	status := h.settled(service, hasOutcome(poolA, declaration.Applied))
	assert.Equal(t, []declaration.Ref{ref(watched, "gone"), ref(watched, "renamed")}, status.Orphans,
		"DECL-13; another record's object is not this record's orphan")
	_, ok := h.objects.get(ref(watched, "gone"))
	assert.True(t, ok, "DECL-13: absence from the full set deletes nothing")
}

func TestRun_InReportOnlyAppliesAndDeletesNothing(t *testing.T) {
	h := newHarness(t)
	h.objects.put(garamOwned(watched, "a", h.record, poolA, 1))
	h.objects.put(object(watched, "c", nil))
	h.declare(poolA, pool(watched, "a", 1))
	h.declare("pool-b", pool(watched, "b", 1))
	h.declare("pool-c", pool(watched, "c", 1))
	cfg := h.config()
	cfg.ReportOnly = true
	service := h.run(cfg)
	h.settled(service, hasOutcome("pool-c", declaration.Conflict))

	h.tombstone(poolA)

	status := h.settled(service, func(s declaration.Status) bool { _, held := outcome(s, poolA); return !held })
	assert.True(t, status.ReportOnly)
	assert.Empty(t, h.objects.written(), "DECL-15")
	_, ok := h.objects.get(ref(watched, "a"))
	assert.True(t, ok, "the tombstone deleted nothing")
	assert.Equal(t, []declaration.Ref{ref(watched, "a")}, status.Orphans, "and is still reported")
	_, held := outcome(status, "pool-b")
	assert.False(t, held, "a declaration not applied is not reported applied")
}

func TestRun_ActsOnlyInNamespacesClaimedForItsRelease(t *testing.T) {
	cases := map[string]func(h *harness){
		"no claim":                       func(h *harness) { h.objects.unclaim(other) },
		"a claim naming another release": func(h *harness) { h.objects.claim(other, "garam/another") },
	}
	for name, claim := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			claim(h)
			h.objects.put(garamOwned(other, "stale", h.record, "pool-stale", 1))
			h.declare(poolA, pool(watched, "a", 1))
			h.declare("pool-b", pool(other, "b", 1))

			service := h.run(h.config())

			status := h.settled(service, hasOutcome(poolA, declaration.Applied))
			assert.Equal(t, []string{watched}, status.Namespaces)
			assert.Equal(t, []string{other}, status.ClaimedElsewhere, "CLAIM-2")
			_, held := outcome(status, "pool-b")
			assert.False(t, held, "nothing is reported for a namespace claimed elsewhere")
			assert.Empty(t, status.Orphans, "nor are its objects")
			assert.Equal(t, []string{"create BotPool bots/a"}, h.objects.written(), "nothing is written there")
		})
	}
}

func TestRun_ActsInANamespaceOnceItsClaimAppears(t *testing.T) {
	h := newHarness(t)
	h.objects.unclaim(other)
	h.declare("pool-b", pool(other, "b", 1))
	service := h.run(h.config())
	h.settled(service, func(s declaration.Status) bool { return len(s.ClaimedElsewhere) == 1 })

	h.objects.claim(other, release)

	h.settled(service, hasOutcome("pool-b", declaration.Applied))
}

func TestRun_BacksOffAPollThatWasNotAnswered(t *testing.T) {
	h := newHarness(t)
	failures := 0
	h.setIntercept(func(w http.ResponseWriter, r *http.Request, _ http.Handler) bool {
		if r.URL.Path != declPath || failures == 3 {
			return false
		}
		failures++
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	})
	var mu sync.Mutex
	var waits []time.Duration
	cfg := h.config()
	cfg.Sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		return ctx.Err()
	}
	h.declare(poolA, pool(watched, "a", 1))

	service := h.run(cfg)

	h.settled(service, hasOutcome(poolA, declaration.Applied))
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []time.Duration{time.Second, 2 * time.Second, 4 * time.Second}, waits, "T-6")
}

func TestRun_RunsAPassThatFailedAgain(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	refused := 0
	h.objects.refuse = func(string, *unstructured.Unstructured) error {
		mu.Lock()
		defer mu.Unlock()
		if refused < 2 {
			refused++
			return errors.New("the API server is unavailable")
		}
		return nil
	}
	h.declare(poolA, pool(watched, "a", 1))

	service := h.run(h.config())

	h.settled(service, hasOutcome(poolA, declaration.Applied))
	require.Eventually(t, func() bool { _, ok := h.objects.get(ref(watched, "a")); return ok }, eventually, tick)
}

func mustStatus(t *testing.T, service declaration.Service) declaration.Status {
	t.Helper()
	status, ok := service.Status()
	require.True(t, ok)
	return status
}

func referenceCluster(h *harness) reference.FixtureRequest {
	return reference.FixtureRequest{Cluster: reference.GRN(h.record)}
}

// serveJSON answers what next serves r, as a JSON object.
func serveJSON(t *testing.T, next http.Handler, r *http.Request) map[string]any {
	t.Helper()
	recorded := httptest.NewRecorder()
	next.ServeHTTP(recorded, r)
	var got map[string]any
	require.NoError(t, json.Unmarshal(recorded.Body.Bytes(), &got))
	return got
}

func writeJSON(t *testing.T, w http.ResponseWriter, body any) {
	t.Helper()
	encoded, err := json.Marshal(body)
	require.NoError(t, err)
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(encoded)
}

// injectChange answers the first full set with the reference server's, plus one apply change for
// object, which the reference server would refuse to hold.
func injectChange(t *testing.T, id string, generation int64, object map[string]any) func(http.ResponseWriter, *http.Request, http.Handler) bool {
	return func(w http.ResponseWriter, r *http.Request, next http.Handler) bool {
		if r.URL.Path != declPath || r.URL.Query().Get("after") != "" {
			return false
		}
		got := serveJSON(t, next, r)
		encoded, err := json.Marshal(object)
		require.NoError(t, err)
		got["changes"] = append(got["changes"].([]any),
			map[string]any{"op": "apply", "id": id, "generation": generation, "object": json.RawMessage(encoded)})
		writeJSON(t, w, got)
		return true
	}
}

func TestRun_ReportsNothingAppliedForAWriteThatFailed(t *testing.T) {
	h := newHarness(t)
	h.objects.refuse = func(_ string, o *unstructured.Unstructured) error {
		if o.GetName() == "a" {
			return apierrors.NewForbidden(schema.GroupResource{Group: "bot.garam.sh", Resource: "botpools"}, "a",
				errors.New("the Role grants no create"))
		}
		return nil
	}
	h.declare(poolA, pool(watched, "a", 1))
	h.declare("pool-b", pool(watched, "b", 1))

	service := h.run(h.config())

	status := h.settled(service, hasOutcome("pool-b", declaration.Applied))
	_, reported := outcome(status, poolA)
	assert.False(t, reported, "a create the API server forbade is not reported applied")
	_, ok := h.objects.get(ref(watched, "a"))
	assert.False(t, ok)
}

func TestRun_WaitsARetryAfterOfAtMost60Seconds(t *testing.T) {
	h := newHarness(t)
	throttled := false
	h.setIntercept(func(w http.ResponseWriter, r *http.Request, _ http.Handler) bool {
		if r.URL.Path != declPath || throttled {
			return false
		}
		throttled = true
		w.Header().Set("Retry-After", "3600")
		w.WriteHeader(http.StatusTooManyRequests)
		return true
	})
	var mu sync.Mutex
	var waits []time.Duration
	cfg := h.config()
	cfg.Sleep = func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		return ctx.Err()
	}
	h.declare(poolA, pool(watched, "a", 1))

	service := h.run(cfg)

	h.settled(service, hasOutcome(poolA, declaration.Applied))
	mu.Lock()
	defer mu.Unlock()
	assert.Equal(t, []time.Duration{60 * time.Second}, waits, "an hour asked for is cut to T-6's last wait")
}
