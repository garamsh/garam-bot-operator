package declaration_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"sync"
	"testing"
	"time"

	botv1 "github.com/garamsh/garam-bot/api/declaration/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/garam-agent-operator/interfaces/cluster-connection/v1alpha1/reference"
	"github.com/garamsh/garam-agent-operator/internal/declaration"
)

// reportedObjects are a report's objects, by name.
func reportedObjects(report map[string]any) map[string]map[string]any {
	out := map[string]map[string]any{}
	objects, _ := report["objects"].([]any)
	for _, o := range objects {
		object, _ := o.(map[string]any)
		name, _ := object["name"].(string)
		out[name] = object
	}
	return out
}

func outcomes(report map[string]any) map[string]string {
	out := map[string]string{}
	declarations, _ := report["declarations"].([]any)
	for _, d := range declarations {
		outcome, _ := d.(map[string]any)
		id, _ := outcome["id"].(string)
		out[id], _ = outcome["outcome"].(string)
	}
	return out
}

func carries(names ...string) func(map[string]any) bool {
	return func(report map[string]any) bool {
		objects := reportedObjects(report)
		for _, name := range names {
			if _, ok := objects[name]; !ok {
				return false
			}
		}
		return true
	}
}

// pacedReports is a Config whose 5-minute report timer fires only when the test sends on the channel
// answered, and whose other waits are real.
func pacedReports(cfg declaration.Config) (declaration.Config, chan time.Time) {
	fire := make(chan time.Time)
	after := cfg.After
	cfg.After = func(d time.Duration) <-chan time.Time {
		if d > time.Minute && d <= 5*time.Minute {
			return fire
		}
		if after != nil {
			return after(d)
		}
		return time.After(d)
	}
	return cfg, fire
}

func TestRun_ReportsTheWholeViewOfTheClaimedNamespaces(t *testing.T) {
	h := newHarness(t)
	h.objects.unclaim(other)
	h.objects.put(object(watched, "cluster-made", map[string]string{team: "a"}))
	h.objects.put(garamOwned(watched, "gone", h.record, "pool-gone", 2))
	h.objects.put(garamOwned(watched, "theirs", "grn:acme/iam/cluster/another", "pool-x", 1))
	h.objects.put(object(watched, "malformed", map[string]string{botv1.LabelManagedBy: string(botv1.OwnerGaram)}))
	h.objects.put(object(other, "elsewhere", nil))
	h.declare(poolA, pool(watched, "a", 3))
	h.declare("pool-n", pool("unwatched", "n", 1))

	h.run(h.config())

	report := h.lastReport(carries("a", "cluster-made", "gone"))
	assert.Equal(t, map[string]any{"version": version, "mode": "apply", "namespaces": []any{watched},
		"claimedElsewhere": []any{other}}, report["connector"])
	assert.Equal(t, map[string]string{poolA: "applied", "pool-n": "namespaceNotWatched"}, outcomes(report))
	assert.NotEmpty(t, report["observedAt"])
	objects := reportedObjects(report)
	assert.ElementsMatch(t, []string{"a", "cluster-made", "gone"}, keys(objects),
		"REP-1: own objects of claimed namespaces alone; another record's and a malformed one are neither (CLAIM-3, OWN-4)")
	applied := objects["a"]
	assert.Equal(t, string(botv1.OwnerGaram), applied["owner"])
	assert.Equal(t, h.record, applied["cluster"])
	assert.Equal(t, poolA, applied["declarationId"])
	assert.Equal(t, false, applied["orphaned"])
	assert.NotContains(t, applied, "spec", "REP-2: a garam-owned object carries no spec")
	assert.Equal(t, map[string]any{}, applied["status"])
	for _, member := range []string{"apiVersion", "kind", "namespace", "uid", "generation"} {
		assert.NotEmpty(t, applied[member], member)
	}
	assert.Equal(t, true, objects["gone"]["orphaned"], "DECL-13")
	made := objects["cluster-made"]
	assert.Equal(t, "cluster", made["owner"])
	assert.Equal(t, map[string]any{maxField: float64(1)}, made["spec"], "REP-2: a cluster-owned object carries its spec")
	assert.NotContains(t, made, "declarationId")
	assert.NotContains(t, made, "orphaned")
}

func TestRun_ReportsWithinMomentsOfAChange(t *testing.T) {
	h := newHarness(t)
	h.declare(poolA, pool(watched, "a", 1))
	cfg, _ := pacedReports(h.config())
	h.run(cfg)
	h.lastReport(carries("a"))

	h.objects.put(object(watched, "new", nil))

	start := time.Now()
	h.lastReport(carries("a", "new"))
	assert.Less(t, time.Since(start), 10*time.Second, "REP-4")
}

func TestRun_ReportsAtLeastEvery5Minutes(t *testing.T) {
	h := newHarness(t)
	h.declare(poolA, pool(watched, "a", 1))
	cfg, fire := pacedReports(h.config())
	service := h.run(cfg)
	h.lastReport(carries("a"))
	sent := len(h.reportsSent())
	before, _ := service.Status()
	h.objects.signal()
	h.settled(service, func(s declaration.Status) bool { return s.ObservedAt.After(before.ObservedAt) })
	require.Never(t, func() bool { return len(h.reportsSent()) > sent }, 300*time.Millisecond, tick,
		"control: a pass that finds the same view does not report it again")

	select {
	case fire <- time.Now():
	case <-time.After(eventually):
		t.Fatal("no report waited on the 5-minute timer")
	}

	require.Eventually(t, func() bool { return len(h.reportsSent()) == sent+1 }, eventually, tick, "REP-4")
}

func TestRun_GoesOnApplyingDeclarationsAfterA413(t *testing.T) {
	h := newHarness(t)
	var mu sync.Mutex
	refusals := 0
	h.setIntercept(func(w http.ResponseWriter, r *http.Request, _ http.Handler) bool {
		if r.URL.Path != reportPath {
			return false
		}
		mu.Lock()
		refusals++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusRequestEntityTooLarge)
		_, _ = w.Write([]byte(`{"error":"report_too_large","message":"over 4 MiB"}`))
		return true
	})
	h.declare(poolA, pool(watched, "a", 1))
	var logged syncBuffer
	cfg, _ := pacedReports(h.config())
	cfg.Log = slog.New(slog.NewTextHandler(&logged, nil))
	service := h.run(cfg)
	h.lastReport(carries("a"))
	sent := len(h.reportsSent())
	require.Never(t, func() bool { return len(h.reportsSent()) > sent }, 1500*time.Millisecond, tick,
		"a refused view is not sent again until it changes")
	assert.Contains(t, logged.String(), "garam refused the report as too large", "REP-6: the error is reported")

	h.declare("pool-b", pool(watched, "b", 1))

	h.settled(service, hasOutcome("pool-b", declaration.Applied))
	_, ok := h.objects.get(ref(watched, "b"))
	assert.True(t, ok, "REP-6: declarations go on being applied")
	h.lastReport(carries("a", "b"))
	mu.Lock()
	defer mu.Unlock()
	assert.GreaterOrEqual(t, refusals, 2, "the next change is reported again")
}

func TestRun_SendsAReportNotTakenAgainAfterABackOff(t *testing.T) {
	h := newHarness(t)
	failed := false
	h.setIntercept(func(w http.ResponseWriter, r *http.Request, _ http.Handler) bool {
		if r.URL.Path != reportPath || failed {
			return false
		}
		failed = true
		w.WriteHeader(http.StatusServiceUnavailable)
		return true
	})
	var mu sync.Mutex
	var waits []time.Duration
	cfg := h.config()
	cfg.After = func(d time.Duration) <-chan time.Time {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		if d <= time.Minute {
			return time.After(time.Millisecond)
		}
		return nil
	}
	h.declare(poolA, pool(watched, "a", 1))

	h.run(cfg)

	require.Eventually(t, func() bool {
		got := h.fix("latest-report", reference.FixtureRequest{Cluster: reference.GRN(h.record)})
		return got["report"] != nil
	}, eventually, tick, "the report was taken at its second send")
	mu.Lock()
	defer mu.Unlock()
	assert.Contains(t, waits, time.Second, "T-6: at least 1 second before the same request")
}

func TestRun_ReportsItsMode(t *testing.T) {
	h := newHarness(t)
	h.declare(poolA, pool(watched, "a", 1))
	cfg := h.config()
	cfg.ReportOnly = true

	h.run(cfg)

	report := h.lastReport(func(r map[string]any) bool { return r["connector"] != nil })
	connector, _ := report["connector"].(map[string]any)
	assert.Equal(t, "reportOnly", connector["mode"])
}

// syncBuffer is a log destination many goroutines write to.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func keys[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
