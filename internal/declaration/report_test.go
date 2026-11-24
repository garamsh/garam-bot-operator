package declaration_test

import (
	"bytes"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
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
	made := object(watched, "cluster-made", map[string]string{team: "a"})
	made.Object["status"] = map[string]any{"observedGeneration": int64(1), "phase": "Running"}
	h.objects.put(made)
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
	assert.Equal(t, map[string]any{}, applied["status"], "an object with no status reports an empty one")
	for _, member := range []string{"apiVersion", "kind", "namespace", "uid", "generation"} {
		assert.NotEmpty(t, applied[member], member)
	}
	assert.Equal(t, true, objects["gone"]["orphaned"], "DECL-13")
	reportedMade := objects["cluster-made"]
	assert.Equal(t, "cluster", reportedMade["owner"])
	assert.Equal(t, map[string]any{maxField: float64(1)}, reportedMade["spec"], "REP-2: a cluster-owned object carries its spec")
	assert.Equal(t, map[string]any{"observedGeneration": float64(1), "phase": "Running"}, reportedMade["status"],
		"REP-2: every object carries its status")
	assert.NotContains(t, reportedMade, "declarationId")
	assert.NotContains(t, reportedMade, "orphaned")
	for _, object := range objects {
		for _, member := range []string{"metadata", "labels", "annotations"} {
			assert.NotContains(t, object, member, "REP-2 names the members a report carries")
		}
	}
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
	clock := &shiftedClock{}
	cfg, fire := pacedReports(h.config())
	cfg.Now = clock.now
	service := h.run(cfg)
	h.lastReport(carries("a"))
	sent := len(h.reportsSent())
	before, _ := service.Status()
	h.objects.signal()
	h.settled(service, func(s declaration.Status) bool { return s.ObservedAt.After(before.ObservedAt) })
	require.Never(t, func() bool { return len(h.reportsSent()) > sent }, 300*time.Millisecond, tick,
		"control: a pass that finds the same view does not report it again")

	clock.shift(5 * time.Minute)
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
	assert.Contains(t, logged.String(), "the report is too large for garam", "REP-6: the error is reported")

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
	var failOnce atomic.Bool
	h.setIntercept(func(w http.ResponseWriter, r *http.Request, _ http.Handler) bool {
		if r.URL.Path != reportPath || !failOnce.CompareAndSwap(true, false) {
			return false
		}
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
	service := h.run(cfg)
	h.lastReport(carries("a"))
	failOnce.Store(true)

	h.objects.put(object(watched, "new", nil))

	h.settled(service, func(s declaration.Status) bool { return hasObject(s, "new") })
	require.Eventually(t, func() bool { return !failOnce.Load() }, eventually, tick, "control: the report was refused")
	require.Eventually(t, func() bool {
		got := h.fix("latest-report", reference.FixtureRequest{Cluster: reference.GRN(h.record)})
		report, _ := got["report"].(map[string]any)
		return report != nil && carries("new")(report)
	}, eventually, tick, "the refused report was taken at its next send, with no pass in between")
	mu.Lock()
	defer mu.Unlock()
	assert.True(t, slices.ContainsFunc(waits, func(d time.Duration) bool { return d > 900*time.Millisecond && d <= time.Second }),
		"T-6: 1 second from the refused send to the next: %v", waits)
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

// shiftedClock is the wall clock moved on by what the test shifts it by.
type shiftedClock struct{ by atomic.Int64 }

func (c *shiftedClock) now() time.Time        { return time.Now().Add(time.Duration(c.by.Load())) }
func (c *shiftedClock) shift(d time.Duration) { c.by.Add(int64(d)) }

// answering answers every report status, with Retry-After where it is not empty, once on is set.
func answering(on *atomic.Bool, status int, after string) func(http.ResponseWriter, *http.Request, http.Handler) bool {
	return func(w http.ResponseWriter, r *http.Request, _ http.Handler) bool {
		if r.URL.Path != reportPath || !on.Load() {
			return false
		}
		if after != "" {
			w.Header().Set("Retry-After", after)
		}
		w.WriteHeader(status)
		return true
	}
}

// recordedWaits wraps cfg's waits with real ones it records.
func recordedWaits(cfg declaration.Config) (declaration.Config, func() []time.Duration) {
	var mu sync.Mutex
	var waits []time.Duration
	cfg.After = func(d time.Duration) <-chan time.Time {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		return time.After(d)
	}
	return cfg, func() []time.Duration {
		mu.Lock()
		defer mu.Unlock()
		return append([]time.Duration{}, waits...)
	}
}

func TestRun_BacksOffAReportPastItsDeadline(t *testing.T) {
	h := newHarness(t)
	var failing atomic.Bool
	h.setIntercept(answering(&failing, http.StatusServiceUnavailable, ""))
	clock := &shiftedClock{}
	cfg := h.config()
	cfg.Now = clock.now
	h.declare(poolA, pool(watched, "a", 1))
	h.run(cfg)
	h.lastReport(carries("a"))
	failing.Store(true)
	clock.shift(6 * time.Minute)
	sent := len(h.reportsSent())

	h.objects.put(object(watched, "new", nil))

	require.Eventually(t, func() bool { return len(h.reportsSent()) > sent }, eventually, tick,
		"control: the change past the deadline is sent")
	require.Never(t, func() bool { return len(h.reportsSent()) > sent+2 }, 1500*time.Millisecond, tick,
		"T-6: past REP-4's deadline a failed report still waits 1 s, then 2 s, before it is sent again")
}

func TestRun_HoldsAReportUntilRetryAfterWhateverPassesEnd(t *testing.T) {
	for _, tc := range []struct {
		name, retryAfter string
		wait             time.Duration
	}{
		{"waits Retry-After", "30", 30 * time.Second},
		{"waits at most 60 s", "120", time.Minute},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newHarness(t)
			var limited atomic.Bool
			h.setIntercept(answering(&limited, http.StatusTooManyRequests, tc.retryAfter))
			cfg, waits := recordedWaits(h.config())
			h.declare(poolA, pool(watched, "a", 1))
			service := h.run(cfg)
			h.lastReport(carries("a"))
			limited.Store(true)
			h.objects.put(object(watched, "limited", nil))
			h.lastReport(carries("a", "limited"))
			sent := len(h.reportsSent())

			h.objects.put(object(watched, "during", nil))
			h.settled(service, func(s declaration.Status) bool { return hasObject(s, "during") })

			require.Never(t, func() bool { return len(h.reportsSent()) > sent }, 1500*time.Millisecond, tick,
				"T-6: a pass during the wait sends nothing before it")
			assert.True(t, slices.ContainsFunc(waits(), func(d time.Duration) bool {
				return d > tc.wait-time.Second && d <= tc.wait
			}), "the wait follows Retry-After: %v", waits())
		})
	}
}

func TestRun_WaitsRetryAfterFromTheAnswer(t *testing.T) {
	h := newHarness(t)
	clock := &shiftedClock{}
	var limited atomic.Bool
	h.setIntercept(func(w http.ResponseWriter, r *http.Request, _ http.Handler) bool {
		if r.URL.Path != reportPath || !limited.Load() {
			return false
		}
		clock.shift(10 * time.Second)
		w.Header().Set("Retry-After", "30")
		w.WriteHeader(http.StatusTooManyRequests)
		return true
	})
	cfg, waits := recordedWaits(h.config())
	cfg.Now = clock.now
	h.declare(poolA, pool(watched, "a", 1))
	h.run(cfg)
	h.lastReport(carries("a"))
	limited.Store(true)

	h.objects.put(object(watched, "slow", nil))

	require.Eventually(t, func() bool {
		return slices.ContainsFunc(waits(), func(d time.Duration) bool { return d > 29*time.Second && d <= 30*time.Second })
	}, eventually, tick, "T-6: Retry-After runs from the answer, which arrived 10 s after the send")
}

func TestRun_LeavesUnsentAReportOver4MiB(t *testing.T) {
	h := newHarness(t)
	var logged syncBuffer
	cfg := h.config()
	cfg.Log = slog.New(slog.NewTextHandler(&logged, nil))
	h.declare(poolA, pool(watched, "a", 1))
	service := h.run(cfg)
	h.lastReport(carries("a"))
	sent := len(h.reportsSent())
	huge := object(watched, "huge", nil)
	huge.Object["spec"] = map[string]any{maxField: int64(1), "padding": strings.Repeat("x", 5<<20)}

	h.objects.put(huge)

	require.Eventually(t, func() bool { return strings.Contains(logged.String(), "the report is too large for garam") },
		eventually, tick, "REP-6: the error is reported")
	assert.Len(t, h.reportsSent(), sent, "a report garam need not take is not uploaded")
	h.declare("pool-b", pool(watched, "b", 1))
	h.settled(service, hasOutcome("pool-b", declaration.Applied))
}

func TestRun_LogsAReportGaramRefusedAtError(t *testing.T) {
	for _, tc := range []struct {
		status int
		level  string
	}{
		{http.StatusBadRequest, "level=ERROR"},
		{http.StatusServiceUnavailable, "level=WARN"},
	} {
		t.Run(http.StatusText(tc.status), func(t *testing.T) {
			h := newHarness(t)
			on := atomic.Bool{}
			on.Store(true)
			h.setIntercept(answering(&on, tc.status, ""))
			var logged syncBuffer
			cfg := h.config()
			cfg.Log = slog.New(slog.NewTextHandler(&logged, nil))
			h.declare(poolA, pool(watched, "a", 1))
			h.run(cfg)

			require.Eventually(t, func() bool {
				for line := range strings.SplitSeq(logged.String(), "\n") {
					if strings.Contains(line, "sending it again") {
						return strings.Contains(line, tc.level)
					}
				}
				return false
			}, eventually, tick, "a report refused as wrong is an error, one not taken yet a warning")
		})
	}
}

func hasObject(s declaration.Status, name string) bool {
	return slices.ContainsFunc(s.Objects, func(o declaration.Object) bool { return o.Ref.Name == name })
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
