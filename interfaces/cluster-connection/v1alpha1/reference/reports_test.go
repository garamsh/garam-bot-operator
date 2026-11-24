package reference_test

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/garamsh/garam-agent-operator/interfaces/cluster-connection/v1alpha1/reference"
)

// report is a report observed at, carrying outcomes and objects.
func report(observedAt time.Time, outcomes, objects []map[string]any) map[string]any {
	if outcomes == nil {
		outcomes = []map[string]any{}
	}
	if objects == nil {
		objects = []map[string]any{}
	}
	return map[string]any{
		"observedAt":   observedAt.Format(time.RFC3339Nano),
		"connector":    map[string]any{"version": "test", "mode": "apply", "namespaces": []string{"bots"}},
		"declarations": outcomes,
		"objects":      objects,
	}
}

func outcome(id string) map[string]any {
	return map[string]any{"id": id, "generation": 1, "outcome": "applied"}
}

func garamObject(name, id string) map[string]any {
	return map[string]any{
		"apiVersion": "bot.garam.sh/v1alpha1", "kind": "Bot", "namespace": "bots", "name": name, "uid": name,
		"owner": "garam", "generation": 1, "declarationId": id, "orphaned": false, "status": map[string]any{},
	}
}

func clusterObject(kind, name string) map[string]any {
	return map[string]any{
		"apiVersion": "bot.garam.sh/v1alpha1", "kind": kind, "namespace": "bots", "name": name, "uid": name,
		"owner": "cluster", "generation": 1, "spec": map[string]any{"replicas": 1}, "status": map[string]any{},
	}
}

// latest is the fixture's latest report of grn.
func (h *harness) latest(grn reference.GRN) map[string]any {
	h.t.Helper()
	got := h.fix("latest-report", reference.FixtureRequest{Cluster: grn})
	rep, _ := got["report"].(map[string]any)
	return rep
}

func names(t *testing.T, rep map[string]any, member, key string) []string {
	t.Helper()
	require.NotNil(t, rep, "no report is kept")
	items := rep[member].([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, item.(map[string]any)[key].(string))
	}
	return out
}

func TestReport_AcceptsReport(t *testing.T) {
	h := newHarness(t)
	grn, bearer := h.connected()
	id := h.declare(grn, "", bot("a"))

	got := h.post(repPath, bearer, report(h.clock.Now(), []map[string]any{outcome(id)},
		[]map[string]any{garamObject("a", id), clusterObject("Bot", "mine")}))

	assert.Equal(t, http.StatusNoContent, got.status, string(got.body))
	rep := h.latest(grn)
	assert.Equal(t, []string{id}, names(t, rep, "declarations", "id"))
	assert.Equal(t, []string{"a", "mine"}, names(t, rep, "objects", "name"))
}

func TestReport_KeepsLatestObserved(t *testing.T) {
	h := newHarness(t)
	grn, bearer := h.connected()
	now := h.clock.Now()
	require.Equal(t, http.StatusNoContent,
		h.post(repPath, bearer, report(now, nil, []map[string]any{clusterObject("Bot", "newer")})).status)

	older := h.post(repPath, bearer, report(now.Add(-time.Second), nil, []map[string]any{clusterObject("Bot", "older")}))
	same := h.post(repPath, bearer, report(now, nil, []map[string]any{clusterObject("Bot", "same")}))

	assert.Equal(t, http.StatusNoContent, older.status, "an older report is answered 204 and ignored")
	assert.Equal(t, http.StatusNoContent, same.status)
	assert.Equal(t, []string{"newer"}, names(t, h.latest(grn), "objects", "name"))
	require.Equal(t, http.StatusNoContent,
		h.post(repPath, bearer, report(now.Add(time.Second), nil, []map[string]any{clusterObject("Bot", "latest")})).status)
	assert.Equal(t, []string{"latest"}, names(t, h.latest(grn), "objects", "name"), "control: a newer one replaces it")
}

func TestReport_RecordsLastContact(t *testing.T) {
	h := newHarness(t)
	grn, bearer := h.connected()
	assert.Nil(t, h.fix("latest-report", reference.FixtureRequest{Cluster: grn})["lastContactAt"],
		"an exchange alone is no contact on a protocol route")

	h.clock.advance(time.Minute)
	h.poll(bearer, "", 0)
	h.clock.advance(time.Minute)
	h.poll("not-a-token", "", 0)

	got := h.fix("latest-report", reference.FixtureRequest{Cluster: grn})
	assert.Equal(t, h.clock.Now().Add(-time.Minute).Format(time.RFC3339Nano), got["lastContactAt"],
		"the authenticated poll is recorded and the refused one is not")
}

func TestReport_RefusesObjectsOfKindsNotDeclared(t *testing.T) {
	h := newHarness(t)
	grn, bearer := h.connected()
	for _, kind := range []string{"Secret", "ConfigMap", "Deployment"} {
		t.Run(kind, func(t *testing.T) {
			got := h.post(repPath, bearer, report(h.clock.Now(), nil,
				[]map[string]any{clusterObject("Bot", "fine"), clusterObject(kind, "refused")}))

			assert.Equal(t, http.StatusBadRequest, got.status)
			assert.Equal(t, "invalid_request", got.errorCode(t))
			assert.Nil(t, h.latest(grn), "nothing of the refused report is kept")
		})
	}
	assert.Equal(t, http.StatusNoContent,
		h.post(repPath, bearer, report(h.clock.Now(), nil, []map[string]any{clusterObject("Bot", "fine")})).status,
		"control: the same report without the refused object is accepted")
}

func TestReport_IgnoresForeignDeclarationID(t *testing.T) {
	h := newHarness(t)
	grn, bearer := h.connected()
	_, otherRecord := h.mint()
	own := h.declare(grn, "", bot("own"))
	foreign := h.declare(otherRecord, "", bot("foreign"))

	got := h.post(repPath, bearer, report(h.clock.Now(),
		[]map[string]any{outcome(own), outcome(foreign), outcome("never-declared")},
		[]map[string]any{garamObject("own", own), garamObject("foreign", foreign), garamObject("none", "never-declared")}))

	require.Equal(t, http.StatusNoContent, got.status, string(got.body))
	rep := h.latest(grn)
	assert.Equal(t, []string{own}, names(t, rep, "declarations", "id"))
	assert.Equal(t, []string{"own"}, names(t, rep, "objects", "name"))
	assert.Nil(t, h.latest(otherRecord), "nothing is recorded on the record the foreign id is placed on")
}

func TestReport_SpecIsNeverADeclaration(t *testing.T) {
	h := newHarness(t)
	grn, bearer := h.connected()
	object := clusterObject("Bot", "by-kubectl")
	object["spec"] = bot("injected")

	require.Equal(t, http.StatusNoContent,
		h.post(repPath, bearer, report(h.clock.Now(), nil, []map[string]any{object})).status)

	assert.Empty(t, h.poll(bearer, "", 0).page(t).Changes)
	assert.Equal(t, []string{"by-kubectl"}, names(t, h.latest(grn), "objects", "name"))
}

func TestReport_RefusesBodyOver4MiB(t *testing.T) {
	h := newHarness(t)
	_, bearer := h.connected()
	object := clusterObject("Bot", "big")
	object["status"] = map[string]any{"message": strings.Repeat("x", 4<<20)}

	got := h.post(repPath, bearer, report(h.clock.Now(), nil, []map[string]any{object}))

	assert.Equal(t, http.StatusRequestEntityTooLarge, got.status)
	assert.Equal(t, "report_too_large", got.errorCode(t))
}
