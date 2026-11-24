package kube_test

import (
	"context"
	"fmt"
	"sync/atomic"
	"time"

	botv1 "github.com/garamsh/garam-bot/api/declaration/v1alpha1"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	coordinationv1 "k8s.io/api/coordination/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/garamsh/garam-agent-operator/internal/declaration"
	"github.com/garamsh/garam-agent-operator/internal/declaration/kube"
)

const record = "grn:acme/iam/cluster/c1"

var spaces atomic.Int32

// namespace creates a namespace of its own for one spec.
func namespace() string {
	name := fmt.Sprintf("watched-%d", spaces.Add(1))
	Expect(k8sClient.Create(ctx, &corev1.Namespace{Name: name})).To(Succeed())
	return name
}

// workspaceClass is a WorkspaceClass as a declaration applies it, with an idle sleep or without.
func workspaceClass(ns, name string, idle bool) *unstructured.Unstructured {
	spec := map[string]any{"provider": "mvmd", "resources": map[string]any{"cpu": "1", "memory": "1Gi", "disk": "1Gi"}}
	if idle {
		spec["idle"] = map[string]any{"sleepAfter": "1h0m0s"}
	}
	o := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	o.SetAPIVersion(declaration.APIVersion)
	o.SetKind("WorkspaceClass")
	o.SetNamespace(ns)
	o.SetName(name)
	Expect(botv1.MarkGaramOwned(o, botv1.GaramDeclaration{Cluster: record, ID: "class-" + name, Generation: 1})).To(Succeed())
	return o
}

var _ = Describe("Objects", func() {
	var (
		watched string
		objects *kube.Objects
	)

	// heldAs waits until the cache lists the object at name in watched, at least as recent as version.
	heldAs := func(name string, version func(declaration.Held) bool) declaration.Held {
		var found declaration.Held
		Eventually(func(g Gomega) {
			listed, err := objects.List(ctx, watched)
			g.Expect(err).NotTo(HaveOccurred())
			for _, held := range listed {
				if held.Ref.Name == name && version(held) {
					found = held
					return
				}
			}
			g.Expect(false).To(BeTrue(), "%s is not listed yet", name)
		}, 10*time.Second, 50*time.Millisecond).Should(Succeed())
		return found
	}
	present := func(declaration.Held) bool { return true }
	live := func(name string) *botv1.WorkspaceClass {
		got := &botv1.WorkspaceClass{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: watched, Name: name}, got)).To(Succeed())
		return got
	}

	BeforeEach(func() {
		watched = namespace()
		var err error
		objects, err = kube.New(cfg, []string{watched})
		Expect(err).NotTo(HaveOccurred())
		started, stop := context.WithCancel(ctx)
		DeferCleanup(stop)
		go func() { _ = objects.Start(started) }()
		Expect(objects.WaitForCacheSync(started)).To(BeTrue())
	})

	It("removes a field a later generation drops from an object it created", func() {
		Expect(objects.Create(ctx, workspaceClass(watched, "c", true))).To(Succeed())
		held := heldAs("c", present)

		Expect(objects.Apply(ctx, workspaceClass(watched, "c", false), held)).To(Succeed())

		Expect(live("c").Spec.Idle).To(BeNil(), "the create's fields were handed to the apply's entry")
	})

	It("reverts a hand edit by another field manager", func() {
		Expect(objects.Create(ctx, workspaceClass(watched, "c", false))).To(Succeed())
		edited := live("c")
		edited.Spec.Provider = "edited"
		Expect(k8sClient.Update(ctx, edited, client.FieldOwner("kubectl-edit"))).To(Succeed())
		held := heldAs("c", func(h declaration.Held) bool { return h.ResourceVersion == edited.ResourceVersion })

		Expect(objects.Apply(ctx, workspaceClass(watched, "c", false), held)).To(Succeed())

		Expect(live("c").Spec.Provider).To(Equal("mvmd"))
	})

	It("refuses to apply over an object changed after it was read", func() {
		Expect(objects.Create(ctx, workspaceClass(watched, "c", false))).To(Succeed())
		held := heldAs("c", present)
		edited := live("c")
		edited.Spec.Resources.CPU = resource.MustParse("2")
		Expect(k8sClient.Update(ctx, edited, client.FieldOwner("kubectl-edit"))).To(Succeed())
		// Whether the cache has the edit yet or not, the object held is refused.
		heldAs("c", func(h declaration.Held) bool { return h.ResourceVersion == edited.ResourceVersion })

		err := objects.Apply(ctx, workspaceClass(watched, "c", false), held)

		Expect(err).To(MatchError(declaration.ErrChanged))
		Expect(live("c").Spec.Resources.CPU.String()).To(Equal("2"))
	})

	It("refuses to create over a name another object holds", func() {
		Expect(k8sClient.Create(ctx, &botv1.BotPool{
			Namespace: watched, Name: "p", Spec: botv1.BotPoolSpec{MaxBotsPerPod: 2},
		})).To(Succeed())
		declared := &unstructured.Unstructured{Object: map[string]any{"spec": map[string]any{"maxBotsPerPod": int64(5)}}}
		declared.SetAPIVersion(declaration.APIVersion)
		declared.SetKind("BotPool")
		declared.SetNamespace(watched)
		declared.SetName("p")

		Expect(objects.Create(ctx, declared)).To(MatchError(declaration.ErrNameTaken))

		got := &botv1.BotPool{}
		Expect(k8sClient.Get(ctx, client.ObjectKey{Namespace: watched, Name: "p"}, got)).To(Succeed())
		Expect(got.Spec.MaxBotsPerPod).To(Equal(int32(2)))
		Expect(got.Labels).NotTo(HaveKey(botv1.LabelManagedBy))
	})

	It("deletes the object held, and refuses one changed after it was read", func() {
		Expect(objects.Create(ctx, workspaceClass(watched, "c", false))).To(Succeed())
		held := heldAs("c", present)
		edited := live("c")
		edited.Spec.Provider = "edited"
		Expect(k8sClient.Update(ctx, edited)).To(Succeed())

		Expect(objects.Delete(ctx, held)).To(MatchError(declaration.ErrChanged))
		Expect(objects.Delete(ctx, heldAs("c", func(h declaration.Held) bool {
			return h.ResourceVersion == edited.ResourceVersion
		}))).To(Succeed())

		err := k8sClient.Get(ctx, client.ObjectKey{Namespace: watched, Name: "c"}, &botv1.WorkspaceClass{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("lists each object with its ownership marker, a malformed one included", func() {
		Expect(objects.Create(ctx, workspaceClass(watched, "mine", false))).To(Succeed())
		Expect(k8sClient.Create(ctx, &botv1.BotPool{
			Namespace: watched, Name: "malformed", Labels: map[string]string{botv1.LabelManagedBy: "garam"},
			Spec: botv1.BotPoolSpec{MaxBotsPerPod: 1},
		})).To(Succeed())

		mine := heldAs("mine", present)
		malformed := heldAs("malformed", present)

		applied, garamOwned := mine.Marker.Garam()
		Expect(garamOwned).To(BeTrue())
		Expect(applied).To(Equal(botv1.GaramDeclaration{Cluster: record, ID: "class-mine", Generation: 1}))
		Expect(mine.Ref).To(Equal(declaration.Ref{Kind: "WorkspaceClass", Namespace: watched, Name: "mine"}))
		Expect(mine.UID).NotTo(BeEmpty())
		Expect(malformed.MarkerErr).To(MatchError(botv1.ErrInvalidMarker))
	})

	It("reads a namespace's claim, and finds none where there is none", func() {
		_, found, err := objects.Claim(ctx, watched)
		Expect(err).NotTo(HaveOccurred())
		Expect(found).To(BeFalse())

		Expect(k8sClient.Create(ctx, &coordinationv1.Lease{
			Namespace: watched, Name: declaration.ClaimName,
			Spec: coordinationv1.LeaseSpec{HolderIdentity: ptr.To("garam/connector")},
		})).To(Succeed())

		Eventually(func(g Gomega) {
			holder, found, err := objects.Claim(ctx, watched)
			g.Expect(err).NotTo(HaveOccurred())
			g.Expect(found).To(BeTrue())
			g.Expect(holder).To(Equal("garam/connector"))
		}, 10*time.Second, 50*time.Millisecond).Should(Succeed())
	})

	It("holds no namespace but the watched ones in its cache", func() {
		unwatched := namespace()

		_, err := objects.List(ctx, unwatched)
		Expect(err).To(HaveOccurred())
		_, _, err = objects.Claim(ctx, unwatched)
		Expect(err).To(HaveOccurred())
		_, err = objects.List(ctx, watched)
		Expect(err).NotTo(HaveOccurred(), "control: the watched namespace is read")
	})

	It("signals a change to an object of a declared kind", func() {
		Eventually(func() error { _, err := objects.List(ctx, watched); return err }, 10*time.Second).Should(Succeed())
		select {
		case <-objects.Changed():
		default:
		}
		Consistently(objects.Changed(), 200*time.Millisecond).ShouldNot(Receive(), "control: nothing changed yet")

		Expect(k8sClient.Create(ctx, &botv1.BotPool{
			Namespace: watched, Name: "p", Spec: botv1.BotPoolSpec{MaxBotsPerPod: 1},
		})).To(Succeed())

		Eventually(objects.Changed(), 10*time.Second).Should(Receive())
	})
})
