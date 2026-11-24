//go:build conformance

package server

import (
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/garam-agent-operator/interfaces/cluster-connection/v1alpha1/conformance/internal/wire"
)

const reportsPath = wire.ReportsPath

// report is a Report observed at, carrying outcomes and objects (REP-1).
func report(observedAt time.Time, outcomes, objects []map[string]any) map[string]any {
	if outcomes == nil {
		outcomes = []map[string]any{}
	}
	if objects == nil {
		objects = []map[string]any{}
	}
	return map[string]any{
		"observedAt": observedAt.UTC().Format(time.RFC3339Nano),
		"connector": map[string]any{"version": "conformance", "mode": "apply", "namespaces": []string{"bots"},
			"claimedElsewhere": []string{}},
		"declarations": outcomes,
		"objects":      objects,
	}
}

func emptyReport() map[string]any { return report(time.Now(), nil, nil) }

func outcome(id string) map[string]any {
	return map[string]any{"id": id, "generation": 1, "outcome": "applied"}
}

func garamObject(cluster, name, id string) map[string]any {
	return map[string]any{
		"apiVersion": "bot.garam.sh/v1alpha1", "kind": "Bot", "namespace": "bots", "name": name, "uid": "uid-" + name,
		"owner": "garam", "generation": 1, "cluster": cluster, "declarationId": id, "orphaned": false,
		"status": map[string]any{},
	}
}

func clusterObject(apiVersion, kind, name string) map[string]any {
	return map[string]any{
		"apiVersion": apiVersion, "kind": kind, "namespace": "bots", "name": name, "uid": "uid-" + name,
		"owner": "cluster", "generation": 1, "spec": bot(name)["spec"], "status": map[string]any{},
	}
}

// latest is the fixture's latest report of cluster, nil where none is kept.
func latest(t *testing.T, cluster string) map[string]any {
	t.Helper()
	got := target.Fix(t, "latest-report", cluster)
	kept, _ := got["report"].(map[string]any)
	return kept
}

func names(t *testing.T, kept map[string]any, member, key string) []string {
	t.Helper()
	require.NotNil(t, kept, "no report is kept")
	items, _ := kept[member].([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		object, _ := item.(map[string]any)
		name, _ := object[key].(string)
		out = append(out, name)
	}
	return out
}

func acceptsReport(t *testing.T) {
	record, bearer := target.Connect(t)
	id := target.MustDeclare(t, record.Cluster, "", bot("a"))

	got := target.Post(t, reportsPath, bearer, report(time.Now(),
		[]map[string]any{outcome(id)},
		[]map[string]any{garamObject(record.Cluster, "a", id), clusterObject("bot.garam.sh/v1alpha1", "Bot", "made-here")}))

	assert.Equal(t, http.StatusNoContent, got.Status, string(got.Body))
	kept := latest(t, record.Cluster)
	assert.Equal(t, []string{id}, names(t, kept, "declarations", "id"))
	assert.ElementsMatch(t, []string{"a", "made-here"}, names(t, kept, "objects", "name"))
}

func keepsLatestObserved(t *testing.T) {
	record, bearer := target.Connect(t)
	now := time.Now()
	post := func(at time.Time, name string) int {
		return target.Post(t, reportsPath, bearer,
			report(at, nil, []map[string]any{clusterObject("bot.garam.sh/v1alpha1", "Bot", name)})).Status
	}
	require.Equal(t, http.StatusNoContent, post(now, "newer"))

	older := post(now.Add(-time.Second), "older")

	assert.Equal(t, http.StatusNoContent, older, "REP-5: an older report is answered 204 and ignored")
	assert.Equal(t, []string{"newer"}, names(t, latest(t, record.Cluster), "objects", "name"))
	require.Equal(t, http.StatusNoContent, post(now.Add(time.Second), "latest"))
	assert.Equal(t, []string{"latest"}, names(t, latest(t, record.Cluster), "objects", "name"),
		"control: a later one replaces it")
}

// recordsLastContact allows the server's clock to stand up to a minute from the suite's.
func recordsLastContact(t *testing.T) {
	record, bearer := target.Connect(t)
	before := time.Now()

	require.Equal(t, http.StatusOK, target.Poll(t, bearer, "", 0).Status)

	got := target.Fix(t, "latest-report", record.Cluster)
	contact, err := time.Parse(time.RFC3339Nano, stringOf(got["lastContactAt"]))
	require.NoError(t, err, "REP-5: lastContactAt after an authenticated request: %v", got)
	assert.WithinRange(t, contact, before.Add(-time.Minute), time.Now().Add(time.Minute))
}

// refusedReports requires each report object to be refused as REP-7 says, and, as a control, the same
// report carrying a declared kind to be taken.
func refusedReports(t *testing.T, objects map[string]map[string]any) {
	t.Helper()
	record, bearer := target.Connect(t)
	for name, object := range objects {
		t.Run(name, func(t *testing.T) {
			got := target.Post(t, reportsPath, bearer, report(time.Now(), nil, []map[string]any{object}))

			assert.Equal(t, http.StatusBadRequest, got.Status, string(got.Body))
			assert.Equal(t, "invalid_request", got.ErrorCode(t))
		})
	}
	assert.Nil(t, latest(t, record.Cluster), "no refused report is kept")
	assert.Equal(t, http.StatusNoContent, target.Post(t, reportsPath, bearer, report(time.Now(), nil,
		[]map[string]any{clusterObject("bot.garam.sh/v1alpha1", "Bot", "declared")})).Status, "control: a declared kind")
}

func refusesSecretKind(t *testing.T) {
	refusedReports(t, map[string]map[string]any{
		"Secret":    clusterObject("v1", "Secret", "creds"),
		"ConfigMap": clusterObject("v1", "ConfigMap", "settings"),
	})
}

func refusesUndeclaredReportKind(t *testing.T) {
	refusedReports(t, map[string]map[string]any{
		"Deployment":                       clusterObject("apps/v1", "Deployment", "app"),
		"a declared kind in another group": clusterObject("example.com/v1alpha1", "Bot", "lookalike"),
	})
}

func ignoresForeignDeclarationID(t *testing.T) {
	record, bearer := target.Connect(t)
	other, _ := target.Connect(t)
	own := target.MustDeclare(t, record.Cluster, "", bot("own"))
	foreign := target.MustDeclare(t, other.Cluster, "", bot("foreign"))

	got := target.Post(t, reportsPath, bearer, report(time.Now(),
		[]map[string]any{outcome(own), outcome(foreign), outcome("never-declared")},
		[]map[string]any{garamObject(record.Cluster, "own", own), garamObject(record.Cluster, "foreign", foreign),
			garamObject(record.Cluster, "none", "never-declared")}))

	require.Equal(t, http.StatusNoContent, got.Status, string(got.Body))
	kept := latest(t, record.Cluster)
	assert.Equal(t, []string{own}, names(t, kept, "declarations", "id"), "REP-8")
	assert.Equal(t, []string{"own"}, names(t, kept, "objects", "name"), "REP-8")
	assert.Nil(t, latest(t, other.Cluster), "REP-8: nothing is recorded on the record the foreign id is placed on")
}

func specIsNeverADeclaration(t *testing.T) {
	record, bearer := target.Connect(t)
	object := clusterObject("bot.garam.sh/v1alpha1", "Bot", "by-kubectl")
	object["spec"] = bot("injected")

	require.Equal(t, http.StatusNoContent,
		target.Post(t, reportsPath, bearer, report(time.Now(), nil, []map[string]any{object})).Status)

	for _, page := range target.FullSet(t, bearer) {
		assert.Empty(t, page.Changes, "REP-9: a reported spec places no declaration")
	}
	assert.Equal(t, []string{"by-kubectl"}, names(t, latest(t, record.Cluster), "objects", "name"),
		"control: the report was kept")
}
