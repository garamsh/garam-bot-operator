//go:build conformance

package client

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
)

func (h *harness) lease(namespace string) (*coordinationv1.Lease, bool) {
	h.t.Helper()
	lease := &coordinationv1.Lease{}
	err := kube.Get(context.Background(), types.NamespacedName{Namespace: namespace, Name: claimName}, lease)
	if apierrors.IsNotFound(err) {
		return nil, false
	}
	require.NoError(h.t, err)
	return lease, true
}

func actsOnlyUnderOwnClaim(t *testing.T) {
	h := newHarness(t, namespaces(0, 1))
	own, elsewhere := h.watched[0], h.watched[1]
	h.declare("", bot(own, "a", "reviewer"))
	h.declare("", bot(elsewhere, "a", "reviewer"))
	h.start()

	h.applied(own, "a")
	h.report(func(r map[string]any) bool {
		connector, _ := r["connector"].(map[string]any)
		namespaces, _ := connector["namespaces"].([]any)
		return len(namespaces) == 1 && namespaces[0] == own
	}, "CLAIM-2: namespaces are those whose claim names its release")
	_, applied := h.object(elsewhere, "a")
	assert.False(t, applied, "CLAIM-2: nothing is applied under another release's claim")

	lease, _ := h.lease(own)
	require.NoError(t, kube.Delete(context.Background(), lease), "the installer removes the claim (CLAIM-1)")
	h.declare("", bot(own, "b", "reviewer"))
	require.Never(t, func() bool {
		_, ok := h.object(own, "b")
		return ok
	}, 10*time.Second, tick, "CLAIM-2: it acts only while the claim exists")
}

func neverWritesAClaim(t *testing.T) {
	h := newHarness(t, namespaces(1, 1))
	own, unclaimed, elsewhere := h.watched[0], h.watched[1], h.watched[2]
	before := map[string]string{}
	for _, ns := range []string{own, elsewhere} {
		lease, _ := h.lease(ns)
		before[ns] = lease.ResourceVersion
	}
	h.declare("", bot(own, "a", "reviewer"))
	client := h.start()
	h.applied(own, "a")
	client.stop()

	for ns, version := range before {
		lease, ok := h.lease(ns)
		require.True(t, ok, "CLAIM-2: the claim in %s is not deleted", ns)
		assert.Equal(t, version, lease.ResourceVersion, "CLAIM-2: the claim in %s is not updated", ns)
	}
	_, created := h.lease(unclaimed)
	assert.False(t, created, "CLAIM-2: no claim is created")
}

// staysOut requires the watched namespace ns to be listed claimedElsewhere, and nothing in it applied
// or reported.
func staysOut(t *testing.T, h *harness, ns string) {
	t.Helper()
	h.put(ns, "made-here", "kept", nil)
	id := h.declare("", bot(ns, "a", "reviewer"))
	own := h.declare("", bot(h.watched[0], "a", "reviewer"))
	h.start()

	h.applied(h.watched[0], "a")
	r := h.report(func(r map[string]any) bool {
		connector, _ := r["connector"].(map[string]any)
		elsewhere, _ := connector["claimedElsewhere"].([]any)
		return len(elsewhere) == 1 && elsewhere[0] == ns && hasOutcome(own, "applied")(r)
	}, "CLAIM-2: listed claimedElsewhere")
	assert.NotContains(t, outcomes(r), id, "CLAIM-2: nothing is reported for it")
	for _, o := range reportedObjects(r) {
		assert.NotEqual(t, ns, o["namespace"], "CLAIM-2: none of its objects is reported")
	}
	_, applied := h.object(ns, "a")
	assert.False(t, applied, "CLAIM-2: nothing is applied in it")
}

func staysOutOfNamespaceClaimedElsewhere(t *testing.T) {
	h := newHarness(t, namespaces(0, 1))
	staysOut(t, h, h.watched[1])
}

func staysOutOfUnclaimedNamespace(t *testing.T) {
	h := newHarness(t, namespaces(1, 0))
	staysOut(t, h, h.watched[1])
}
