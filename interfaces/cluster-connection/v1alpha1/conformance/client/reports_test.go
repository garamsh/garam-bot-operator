//go:build conformance

package client

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	botv1 "github.com/garamsh/garam-bot/api/declaration/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

func wholeView(t *testing.T) {
	h := newHarness(t)
	ns := h.watched[0]
	made := h.put(ns, "made-here", "kept", nil)
	const message = "status-written-through-the-subresource"
	require.NoError(t, unstructured.SetNestedField(made.Object, map[string]any{"observedGeneration": int64(1),
		"conditions": []any{map[string]any{"type": "Ready", "status": "True", "reason": "Tested", "message": message,
			"lastTransitionTime": metav1.Now().UTC().Format(time.RFC3339)}}}, "status"))
	require.NoError(t, kube.Status().Update(context.Background(), made))
	id := h.declare("", bot(ns, "a", "reviewer"))
	h.start()

	r := h.report(func(r map[string]any) bool {
		return carries("a", "made-here")(r) && strings.Contains(mustJSON(r), message)
	}, "REP-1: the whole view, the status included")

	assert.NotEmpty(t, r["observedAt"], "REP-1")
	connector, _ := r["connector"].(map[string]any)
	assert.NotEmpty(t, connector["version"])
	assert.Equal(t, "apply", connector["mode"])
	assert.Equal(t, []any{ns}, connector["namespaces"])
	assert.Equal(t, map[string]string{id: "applied"}, outcomes(r))
	objects := reportedObjects(r)
	applied := objects["a"]
	for _, member := range []string{"apiVersion", "kind", "namespace", "name", "uid", "owner", "generation", "status"} {
		assert.Contains(t, applied, member, "REP-2")
	}
	assert.Equal(t, "garam", applied["owner"])
	assert.Equal(t, id, applied["declarationId"], "REP-2")
	assert.Equal(t, false, applied["orphaned"], "REP-2")
	assert.NotContains(t, applied, "spec", "REP-2: a garam-owned object carries no spec")
	cluster := objects["made-here"]
	assert.Equal(t, "cluster", cluster["owner"])
	assert.Equal(t, map[string]any{"role": "kept"}, cluster["spec"].(map[string]any)["persona"], "REP-2: its spec")
	conditions, _ := cluster["status"].(map[string]any)["conditions"].([]any)
	require.Len(t, conditions, 1, "REP-2: its status")
	assert.Equal(t, message, conditions[0].(map[string]any)["message"])
	assert.NotContains(t, cluster, "declarationId")
	for name, o := range objects {
		for _, member := range []string{"metadata", "labels", "annotations", "managedFields"} {
			assert.NotContains(t, o, member, "REP-2 names the members of %s", name)
		}
	}
}

func mustJSON(v any) string {
	encoded, _ := json.Marshal(v)
	return string(encoded)
}

func leavesOutOtherRecordsObjects(t *testing.T) {
	h := newHarness(t)
	ns := h.watched[0]
	h.put(ns, "theirs", "kept", markedBy(anotherRecord))
	h.declare("", bot(ns, "own", "reviewer"))
	h.start()

	r := h.report(carries("own"), "control: its own object is reported")

	assert.NotContains(t, reportedObjects(r), "theirs", "REP-1, CLAIM-3: another record's object is not its own")
}

func noSecretOrConfigMap(t *testing.T) {
	h := newHarness(t)
	ns := h.watched[0]
	const secretValue, configValue = "s3cr3t-value-never-reported", "config-value-never-reported"
	ctx := context.Background()
	require.NoError(t, kube.Create(ctx, &corev1.Secret{Namespace: ns, Name: "creds",
		StringData: map[string]string{"token": secretValue}}))
	require.NoError(t, kube.Create(ctx, &corev1.ConfigMap{Namespace: ns, Name: "settings",
		Data: map[string]string{"setting": configValue}}))
	h.put(ns, "made-here", "kept", func(o *unstructured.Unstructured) {
		_ = unstructured.SetNestedStringSlice(o.Object, []string{"creds/token"}, "spec", "permissions", "secrets")
	})
	declared := bot(ns, "a", "reviewer")
	declared["spec"].(map[string]any)["permissions"] = map[string]any{"secrets": []any{"creds/token"}}
	h.declare("", declared)
	h.start()

	h.report(carries("a", "made-here"), "control: the namespace's own objects are reported")

	for _, r := range h.reports() {
		encoded := mustJSON(r)
		assert.NotContains(t, encoded, secretValue, "REP-3: no Secret's value")
		assert.NotContains(t, encoded, configValue, "REP-3: no ConfigMap's value")
		for _, o := range reportedObjects(r) {
			assert.Equal(t, botv1.GroupVersion.String(), o["apiVersion"], "REP-3: only declared kinds")
			assert.NotContains(t, []any{"Secret", "ConfigMap"}, o["kind"])
		}
		assert.NotContains(t, reportedObjects(r), "settings")
	}
	assert.Contains(t, mustJSON(h.reports()), `"creds/token"`, "REP-3: a secret named only by reference")
}

func within10sOfChange(t *testing.T) {
	h := newHarness(t)
	ns := h.watched[0]
	h.start()
	h.report(func(map[string]any) bool { return true }, "a first report")

	changed := time.Now()
	h.put(ns, "new", "reviewer", nil)

	h.report(carries("new"), "the change is reported")
	for i, s := range h.sent(reportsPath) {
		if carries("new")(h.reports()[i]) {
			assert.Less(t, s.at.Sub(changed), 10*time.Second, "REP-4: within 10 seconds of the change")
			return
		}
	}
}

func atLeastEvery5m(t *testing.T) {
	h := newHarness(t)
	h.start()
	h.report(func(map[string]any) bool { return true }, "a first report")
	time.Sleep(15 * time.Second)
	settled := len(h.sent(reportsPath))

	require.Eventually(t, func() bool { return len(h.sent(reportsPath)) > settled }, 5*time.Minute+30*time.Second,
		time.Second, "REP-4: an unchanged view is reported again")

	sent := h.sent(reportsPath)
	assert.LessOrEqual(t, sent[len(sent)-1].at.Sub(sent[len(sent)-2].at), 5*time.Minute+5*time.Second,
		"REP-4: at least every 5 minutes")
}

// answeringReports answers every report with status, and Retry-After where it is set.
func answeringReports(status int, code, retryAfter string) intercept {
	return func(w http.ResponseWriter, r *http.Request, _ []byte, _ http.Handler) bool {
		if r.URL.Path != reportsPath {
			return false
		}
		if retryAfter != "" {
			w.Header().Set("Retry-After", retryAfter)
		}
		answer(w, status, code)
		return true
	}
}

func goesOnAfter413(t *testing.T) {
	h := newHarness(t)
	ns := h.watched[0]
	h.setIntercept(answeringReports(http.StatusRequestEntityTooLarge, "report_too_large", ""))
	h.declare("", bot(ns, "a", "reviewer"))
	h.start()
	h.applied(ns, "a")
	require.Eventually(t, func() bool { return len(h.sent(reportsPath)) > 0 }, eventually, tick)
	sent := len(h.sent(reportsPath))

	require.Never(t, func() bool { return len(h.sent(reportsPath)) > sent }, 5*time.Second, tick,
		"the same view is not sent again before it changes")
	h.declare("", bot(ns, "b", "reviewer"))

	h.applied(ns, "b")
	assert.Eventually(t, func() bool { return len(h.sent(reportsPath)) > sent }, eventually, tick,
		"REP-6: the next change is reported again, and declarations go on")
}

func reportsBackOffOn5xx(t *testing.T) {
	h := newHarness(t)
	h.setIntercept(answeringReports(http.StatusServiceUnavailable, "unavailable", ""))
	h.start()

	require.Eventually(t, func() bool { return len(h.sent(reportsPath)) >= 4 }, time.Minute, tick)

	waited := gaps(h.sent(reportsPath)[:4])
	for i, want := range []time.Duration{time.Second, 2 * time.Second, 4 * time.Second} {
		assert.GreaterOrEqual(t, waited[i], want*95/100, "T-6: at least 1 s, doubling: %v", waited)
	}
}

func reportsWaitRetryAfter(t *testing.T) {
	h := newHarness(t)
	ns := h.watched[0]
	h.setIntercept(answeringReports(http.StatusTooManyRequests, "throttled", "6"))
	h.start()
	require.Eventually(t, func() bool { return len(h.sent(reportsPath)) > 0 }, eventually, tick)
	first := h.sent(reportsPath)[0]

	h.put(ns, "during-the-wait", "reviewer", nil)

	require.Eventually(t, func() bool { return len(h.sent(reportsPath)) >= 2 }, eventually, tick)
	assert.GreaterOrEqual(t, h.sent(reportsPath)[1].at.Sub(first.at), 6*time.Second,
		"T-6: a change during the wait sends nothing before Retry-After")
}

func reportsAfterRestart(t *testing.T) {
	h := newHarness(t)
	ns := h.watched[0]
	h.declare("", bot(ns, "a", "reviewer"))
	client := h.start()
	before := h.report(carries("a"), "a first report")
	client.stop()
	stopped := len(h.sent(reportsPath))

	client.restart()

	require.Eventually(t, func() bool { return len(h.sent(reportsPath)) > stopped }, eventually, tick,
		"the restarted client reports")
	after := h.reports()[stopped]
	assert.True(t, carries("a")(after), "REP-1: the whole view again")
	was, _ := time.Parse(time.RFC3339Nano, before["observedAt"].(string))
	is, _ := time.Parse(time.RFC3339Nano, after["observedAt"].(string))
	assert.True(t, is.After(was), "REP-5: observedAt is later than the last report's")
}
