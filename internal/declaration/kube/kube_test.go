package kube_test

import (
	"context"
	"testing"
	"time"

	botv1 "github.com/garamsh/garam-bot/api/declaration/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/garamsh/garam-agent-operator/internal/declaration"
	"github.com/garamsh/garam-agent-operator/internal/declaration/kube"
)

const (
	record = "grn:acme/iam/cluster/c1"
	// holder is the release every claim in these tests names.
	holder = "garam/connector"
	// account names the ServiceAccount, its Role and its binding; handEdit is what a hand edit writes.
	account  = "connector"
	handEdit = "edited"
	specKey  = "spec"
)

// declared marks o as this record's object for declaration id at generation 1.
func declared(t *testing.T, o *unstructured.Unstructured, id string) *unstructured.Unstructured {
	t.Helper()
	o.SetAPIVersion(declaration.APIVersion)
	require.NoError(t, botv1.MarkGaramOwned(o, botv1.GaramDeclaration{Cluster: record, ID: id, Generation: 1}))
	return o
}

// workspaceClass is a WorkspaceClass as a declaration applies it, with an idle sleep or without.
func workspaceClass(t *testing.T, ns, name string, idle bool) *unstructured.Unstructured {
	t.Helper()
	spec := map[string]any{"provider": "mvmd", "resources": map[string]any{"cpu": "1", "memory": "1Gi", "disk": "1Gi"}}
	if idle {
		spec["idle"] = map[string]any{"sleepAfter": "1h0m0s"}
	}
	o := &unstructured.Unstructured{Object: map[string]any{specKey: spec}}
	o.SetKind("WorkspaceClass")
	o.SetNamespace(ns)
	o.SetName(name)
	return declared(t, o, "class-"+name)
}

// bot is a Bot as a declaration applies it, allowed egress to hosts.
func bot(t *testing.T, ns, name string, hosts ...string) *unstructured.Unstructured {
	t.Helper()
	egress := make([]any, 0, len(hosts))
	for _, host := range hosts {
		egress = append(egress, map[string]any{"host": host})
	}
	o := &unstructured.Unstructured{Object: map[string]any{specKey: map[string]any{
		"persona": map[string]any{"role": "reviewer"}, "agent": map[string]any{"harness": "claude-code"},
		"permissions": map[string]any{"egress": egress},
	}}}
	o.SetKind("Bot")
	o.SetNamespace(ns)
	o.SetName(name)
	return declared(t, o, "bot-"+name)
}

func liveClass(t *testing.T, ns string) *botv1.WorkspaceClass {
	t.Helper()
	got := &botv1.WorkspaceClass{}
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "c"}, got))
	return got
}

func TestApply_RemovesWhatALaterGenerationDropsFromAnObjectItCreated(t *testing.T) {
	t.Run("a field of the spec", func(t *testing.T) {
		ns := namespace(t)
		objects := started(t, cfg, ns)
		require.NoError(t, objects.Create(context.Background(), workspaceClass(t, ns, "c", true)))
		held := heldAs(t, objects, ns, "c", present)

		require.NoError(t, objects.Apply(context.Background(), workspaceClass(t, ns, "c", false), held))

		assert.Nil(t, liveClass(t, ns).Spec.Idle)
	})
	t.Run("a label", func(t *testing.T) {
		ns := namespace(t)
		objects := started(t, cfg, ns)
		first := workspaceClass(t, ns, "c", false)
		first.SetLabels(map[string]string{"team": "a", botv1.LabelManagedBy: "garam"})
		require.NoError(t, objects.Create(context.Background(), first))
		held := heldAs(t, objects, ns, "c", present)

		require.NoError(t, objects.Apply(context.Background(), workspaceClass(t, ns, "c", false), held))

		assert.NotContains(t, liveClass(t, ns).Labels, "team",
			"the create's fields were handed to the apply's entry, so the apply removes the label it dropped")
	})
}

func TestApply_RevertsAHandEdit(t *testing.T) {
	t.Run("a field another manager changed", func(t *testing.T) {
		ns := namespace(t)
		objects := started(t, cfg, ns)
		require.NoError(t, objects.Create(context.Background(), workspaceClass(t, ns, "c", false)))
		edited := liveClass(t, ns)
		edited.Spec.Provider = handEdit
		require.NoError(t, k8sClient.Update(context.Background(), edited, client.FieldOwner("kubectl-edit")))
		held := heldAs(t, objects, ns, "c", atVersion(edited.ResourceVersion))

		require.NoError(t, objects.Apply(context.Background(), workspaceClass(t, ns, "c", false), held))

		assert.Equal(t, "mvmd", liveClass(t, ns).Spec.Provider)
	})
	t.Run("a field another manager added", func(t *testing.T) {
		ns := namespace(t)
		objects := started(t, cfg, ns)
		require.NoError(t, objects.Create(context.Background(), workspaceClass(t, ns, "c", false)))
		require.NoError(t, objects.Apply(context.Background(), workspaceClass(t, ns, "c", false),
			heldAs(t, objects, ns, "c", present)))
		edited := liveClass(t, ns)
		edited.Spec.Idle = &botv1.Idle{SleepAfter: metav1.Duration{Duration: time.Hour}}
		require.NoError(t, k8sClient.Update(context.Background(), edited, client.FieldOwner("kubectl-edit")))
		held := heldAs(t, objects, ns, "c", atVersion(edited.ResourceVersion))

		require.NoError(t, objects.Apply(context.Background(), workspaceClass(t, ns, "c", false), held))

		assert.Nil(t, liveClass(t, ns).Spec.Idle, "the hand-added idle sleep is removed")
	})
	t.Run("an egress host another manager added to a Bot", func(t *testing.T) {
		ns := namespace(t)
		objects := started(t, cfg, ns)
		require.NoError(t, objects.Create(context.Background(), bot(t, ns, "b", "api.example.com")))
		require.NoError(t, objects.Apply(context.Background(), bot(t, ns, "b", "api.example.com"),
			heldAs(t, objects, ns, "b", present)))
		edited := &botv1.Bot{}
		require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "b"}, edited))
		edited.Spec.Permissions.Egress = append(edited.Spec.Permissions.Egress, botv1.EgressPermission{Host: "exfil.example.net"})
		require.NoError(t, k8sClient.Update(context.Background(), edited, client.FieldOwner("kubectl-edit")))
		held := heldAs(t, objects, ns, "b", atVersion(edited.ResourceVersion))

		require.NoError(t, objects.Apply(context.Background(), bot(t, ns, "b", "api.example.com"), held))

		got := &botv1.Bot{}
		require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "b"}, got))
		assert.Equal(t, []botv1.EgressPermission{{Host: "api.example.com"}}, got.Spec.Permissions.Egress,
			"the bot is allowed only what was declared")
	})
	t.Run("the declared marker another manager changed", func(t *testing.T) {
		ns := namespace(t)
		objects := started(t, cfg, ns)
		require.NoError(t, objects.Create(context.Background(), workspaceClass(t, ns, "c", false)))
		edited := liveClass(t, ns)
		edited.Labels[botv1.LabelManagedBy] = handEdit
		edited.Annotations[botv1.AnnotationDeclaration] = handEdit
		require.NoError(t, k8sClient.Update(context.Background(), edited, client.FieldOwner("kubectl-edit")))
		held := heldAs(t, objects, ns, "c", atVersion(edited.ResourceVersion))

		require.NoError(t, objects.Apply(context.Background(), workspaceClass(t, ns, "c", false), held))

		got := liveClass(t, ns)
		assert.Equal(t, "garam", got.Labels[botv1.LabelManagedBy])
		assert.Equal(t, "class-c", got.Annotations[botv1.AnnotationDeclaration])
	})
}

func TestApply_RefusesAnObjectChangedAfterItWasRead(t *testing.T) {
	ns := namespace(t)
	objects := started(t, cfg, ns)
	require.NoError(t, objects.Create(context.Background(), workspaceClass(t, ns, "c", false)))
	held := heldAs(t, objects, ns, "c", present)
	edited := liveClass(t, ns)
	edited.Spec.Resources.CPU = resource.MustParse("2")
	require.NoError(t, k8sClient.Update(context.Background(), edited, client.FieldOwner("kubectl-edit")))
	// The cache has the edit, so the object held is refused before anything is sent.
	heldAs(t, objects, ns, "c", atVersion(edited.ResourceVersion))

	err := objects.Apply(context.Background(), workspaceClass(t, ns, "c", false), held)

	require.ErrorIs(t, err, declaration.ErrChanged)
	assert.Equal(t, "2", liveClass(t, ns).Spec.Resources.CPU.String())
}

func TestCreate_RefusesANameAnotherObjectHolds(t *testing.T) {
	ns := namespace(t)
	objects := started(t, cfg, ns)
	require.NoError(t, k8sClient.Create(context.Background(), &botv1.BotPool{
		Namespace: ns, Name: "p", Spec: botv1.BotPoolSpec{MaxBotsPerPod: 2},
	}))
	pool := &unstructured.Unstructured{Object: map[string]any{specKey: map[string]any{"maxBotsPerPod": int64(5)}}}
	pool.SetKind("BotPool")
	pool.SetNamespace(ns)
	pool.SetName("p")

	require.ErrorIs(t, objects.Create(context.Background(), declared(t, pool, "pool-p")), declaration.ErrNameTaken)

	got := &botv1.BotPool{}
	require.NoError(t, k8sClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "p"}, got))
	assert.Equal(t, int32(2), got.Spec.MaxBotsPerPod)
	assert.NotContains(t, got.Labels, botv1.LabelManagedBy)
}

func TestDelete_DeletesTheObjectHeldAndRefusesOneChangedAfterItWasRead(t *testing.T) {
	ns := namespace(t)
	objects := started(t, cfg, ns)
	require.NoError(t, objects.Create(context.Background(), workspaceClass(t, ns, "c", false)))
	held := heldAs(t, objects, ns, "c", present)
	edited := liveClass(t, ns)
	edited.Spec.Provider = handEdit
	require.NoError(t, k8sClient.Update(context.Background(), edited))

	require.ErrorIs(t, objects.Delete(context.Background(), held), declaration.ErrChanged)
	require.NoError(t, objects.Delete(context.Background(), heldAs(t, objects, ns, "c", atVersion(edited.ResourceVersion))))

	err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: ns, Name: "c"}, &botv1.WorkspaceClass{})
	assert.True(t, apierrors.IsNotFound(err))
}

func TestWrites_RefuseANamespaceTheObjectsDoNotWatch(t *testing.T) {
	ns, unwatched := namespace(t), namespace(t)
	objects := started(t, cfg, ns)
	elsewhere := declaration.Held{Ref: declaration.Ref{Kind: "WorkspaceClass", Namespace: unwatched, Name: "c"}}

	require.ErrorIs(t, objects.Create(context.Background(), workspaceClass(t, unwatched, "c", false)), kube.ErrNotWatched)
	require.ErrorIs(t, objects.Apply(context.Background(), workspaceClass(t, unwatched, "c", false), elsewhere),
		kube.ErrNotWatched)
	require.ErrorIs(t, objects.Delete(context.Background(), elsewhere), kube.ErrNotWatched)

	err := k8sClient.Get(context.Background(), client.ObjectKey{Namespace: unwatched, Name: "c"}, &botv1.WorkspaceClass{})
	assert.True(t, apierrors.IsNotFound(err), "nothing was written there")
	require.NoError(t, objects.Create(context.Background(), workspaceClass(t, ns, "c", false)),
		"control: the watched namespace is written")
}

func TestList_ReadsEachObjectsMarkerAMalformedOneIncluded(t *testing.T) {
	ns := namespace(t)
	objects := started(t, cfg, ns)
	require.NoError(t, objects.Create(context.Background(), workspaceClass(t, ns, "mine", false)))
	require.NoError(t, k8sClient.Create(context.Background(), &botv1.BotPool{
		Namespace: ns, Name: "malformed", Labels: map[string]string{botv1.LabelManagedBy: "garam"},
		Spec: botv1.BotPoolSpec{MaxBotsPerPod: 1},
	}))

	mine := heldAs(t, objects, ns, "mine", present)
	malformed := heldAs(t, objects, ns, "malformed", present)

	applied, garamOwned := mine.Marker.Garam()
	require.True(t, garamOwned)
	assert.Equal(t, botv1.GaramDeclaration{Cluster: record, ID: "class-mine", Generation: 1}, applied)
	assert.Equal(t, declaration.Ref{Kind: "WorkspaceClass", Namespace: ns, Name: "mine"}, mine.Ref)
	assert.NotEmpty(t, mine.UID)
	assert.ErrorIs(t, malformed.MarkerErr, botv1.ErrInvalidMarker)
}

func TestClaim_ReadsTheClaimAndFindsNoneWhereThereIsNone(t *testing.T) {
	ns := namespace(t)
	objects := started(t, cfg, ns)
	_, found, err := objects.Claim(context.Background(), ns)
	require.NoError(t, err)
	assert.False(t, found)

	require.NoError(t, k8sClient.Create(context.Background(), &coordinationv1.Lease{
		Namespace: ns, Name: declaration.ClaimName, Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr.To(holder)},
	}))

	require.Eventually(t, func() bool {
		claimed, found, err := objects.Claim(context.Background(), ns)
		return err == nil && found && claimed == holder
	}, eventually, tick)
}

func TestCache_HoldsNoNamespaceButTheWatchedOnes(t *testing.T) {
	ns, unwatched := namespace(t), namespace(t)
	objects := started(t, cfg, ns)

	_, err := objects.List(context.Background(), unwatched)
	require.Error(t, err)
	_, _, err = objects.Claim(context.Background(), unwatched)
	require.Error(t, err)
	_, err = objects.List(context.Background(), ns)
	require.NoError(t, err, "control: the watched namespace is read")
}

func TestChanged_IsSignalledOnAChangeToAnObjectOfADeclaredKind(t *testing.T) {
	ns := namespace(t)
	objects := started(t, cfg, ns)
	select {
	case <-objects.Changed():
	default:
	}
	require.Never(t, func() bool {
		select {
		case <-objects.Changed():
			return true
		default:
			return false
		}
	}, 200*time.Millisecond, 10*time.Millisecond, "control: nothing changed yet")

	require.NoError(t, k8sClient.Create(context.Background(), &botv1.BotPool{
		Namespace: ns, Name: "p", Spec: botv1.BotPoolSpec{MaxBotsPerPod: 1},
	}))

	select {
	case <-objects.Changed():
	case <-time.After(eventually):
		t.Fatal("no change was signalled")
	}
}

// TestObjects_RunUnderTheRoleConnectorMdStates runs every read and write as a ServiceAccount bound to
// exactly the Role connector.md §Rights states for a watched namespace, and nothing more.
func TestObjects_RunUnderTheRoleConnectorMdStates(t *testing.T) {
	ns := namespace(t)
	ctx := context.Background()
	require.NoError(t, k8sClient.Create(ctx, &corev1.ServiceAccount{Namespace: ns, Name: account}))
	require.NoError(t, k8sClient.Create(ctx, &rbacv1.Role{Namespace: ns, Name: account, Rules: []rbacv1.PolicyRule{
		{
			APIGroups: []string{"bot.garam.sh"},
			Resources: []string{"bots", "workspaces", "workspaceclasses", "botpools"},
			Verbs:     []string{"list", "watch", "create", "patch", "delete"},
		},
		{
			APIGroups:     []string{"coordination.k8s.io"},
			Resources:     []string{"leases"},
			ResourceNames: []string{declaration.ClaimName},
			Verbs:         []string{"get", "list", "watch"},
		},
	}}))
	require.NoError(t, k8sClient.Create(ctx, &rbacv1.RoleBinding{
		Namespace: ns, Name: account,
		RoleRef:  rbacv1.RoleRef{APIGroup: rbacv1.GroupName, Kind: "Role", Name: account},
		Subjects: []rbacv1.Subject{{Kind: rbacv1.ServiceAccountKind, Namespace: ns, Name: account}},
	}))
	require.NoError(t, k8sClient.Create(ctx, &coordinationv1.Lease{
		Namespace: ns, Name: declaration.ClaimName, Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr.To(holder)},
	}))
	impersonated := rest.CopyConfig(cfg)
	impersonated.Impersonate = rest.ImpersonationConfig{UserName: "system:serviceaccount:" + ns + ":" + account}

	objects := started(t, impersonated, ns)

	require.Eventually(t, func() bool {
		claimed, found, err := objects.Claim(ctx, ns)
		return err == nil && found && claimed == holder
	}, eventually, tick, "the claim is read by name")
	require.NoError(t, objects.Create(ctx, workspaceClass(t, ns, "c", true)))
	held := heldAs(t, objects, ns, "c", present)
	require.NoError(t, objects.Apply(ctx, workspaceClass(t, ns, "c", false), held),
		"the managed fields' handover, the replace and the apply")
	applied := liveClass(t, ns)
	assert.Nil(t, applied.Spec.Idle)
	require.NoError(t, objects.Delete(ctx, heldAs(t, objects, ns, "c", atVersion(applied.ResourceVersion))))
	err := k8sClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: "c"}, &botv1.WorkspaceClass{})
	assert.True(t, apierrors.IsNotFound(err))

	as, err := client.New(impersonated, client.Options{Scheme: k8sClient.Scheme()})
	require.NoError(t, err)
	err = as.Create(ctx, &coordinationv1.Lease{Namespace: ns, Name: "another"})
	assert.True(t, apierrors.IsForbidden(err), "control: the identity holds no right beyond the Role: %v", err)
}
