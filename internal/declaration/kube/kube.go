// Package kube is the declaration domain's Objects over a Kubernetes API server: reads from a cache of
// the watched namespaces alone, and writes under one field manager.
package kube

import (
	"context"
	"fmt"

	botv1 "github.com/garamsh/garam-bot/api/declaration/v1alpha1"
	coordinationv1 "k8s.io/api/coordination/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/csaupgrade"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/garamsh/garam-agent-operator/internal/declaration"
)

// FieldManager is the one field manager every write is made under (ADR 0089 decision 2).
const FieldManager = "garam-connector"

// Objects is declaration.Objects over the API server cfg reaches.
type Objects struct {
	cache   cache.Cache
	client  client.Client
	changed chan struct{}
}

var _ declaration.Objects = (*Objects)(nil)

// New returns Objects whose cache holds the declared kinds and the claims of namespaces alone. Start
// runs the cache.
func New(cfg *rest.Config, namespaces []string) (*Objects, error) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register the Kubernetes kinds: %w", err)
	}
	if err := botv1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register the declared kinds: %w", err)
	}
	restricted := make(map[string]cache.Config, len(namespaces))
	for _, namespace := range namespaces {
		restricted[namespace] = cache.Config{}
	}
	objects, err := cache.New(cfg, cache.Options{
		Scheme:            scheme,
		DefaultNamespaces: restricted,
		ByObject: map[client.Object]cache.ByObject{
			// Only the claim of each namespace, never its other Leases.
			&coordinationv1.Lease{}: {Field: fields.OneTermEqualSelector("metadata.name", declaration.ClaimName)},
		},
	})
	if err != nil {
		return nil, fmt.Errorf("build the cache of the watched namespaces: %w", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme, Cache: &client.CacheOptions{Reader: objects, Unstructured: true}})
	if err != nil {
		return nil, fmt.Errorf("build the Kubernetes client: %w", err)
	}
	return &Objects{cache: objects, client: c, changed: make(chan struct{}, 1)}, nil
}

// Start watches the declared kinds and the claims, signals Changed on each change, and runs the cache
// until ctx ends.
func (o *Objects) Start(ctx context.Context) error {
	handler := toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(any) { o.signal() },
		UpdateFunc: func(any, any) { o.signal() },
		DeleteFunc: func(any) { o.signal() },
	}
	watched := make([]client.Object, 0, 1+len(declaration.Kinds))
	watched = append(watched, &coordinationv1.Lease{})
	for _, kind := range declaration.Kinds {
		watched = append(watched, newObject(kind))
	}
	for _, object := range watched {
		informer, err := o.cache.GetInformer(ctx, object)
		if err != nil {
			return fmt.Errorf("watch %T: %w", object, err)
		}
		if _, err := informer.AddEventHandler(handler); err != nil {
			return fmt.Errorf("watch %T: %w", object, err)
		}
	}
	return o.cache.Start(ctx)
}

// WaitForCacheSync waits until the cache Start runs holds the watched namespaces, and answers false
// where ctx ended first. Nothing is read before it.
func (o *Objects) WaitForCacheSync(ctx context.Context) bool {
	return o.cache.WaitForCacheSync(ctx)
}

func (o *Objects) signal() {
	select {
	case o.changed <- struct{}{}:
	default:
	}
}

func (o *Objects) Changed() <-chan struct{} { return o.changed }

func (o *Objects) Claim(ctx context.Context, namespace string) (string, bool, error) {
	var claim coordinationv1.Lease
	err := o.client.Get(ctx, client.ObjectKey{Namespace: namespace, Name: declaration.ClaimName}, &claim)
	switch {
	case apierrors.IsNotFound(err):
		return "", false, nil
	case err != nil:
		return "", false, fmt.Errorf("read the claim of %s: %w", namespace, err)
	case claim.Spec.HolderIdentity == nil:
		return "", true, nil
	}
	return *claim.Spec.HolderIdentity, true, nil
}

func (o *Objects) List(ctx context.Context, namespace string) ([]declaration.Held, error) {
	var out []declaration.Held
	for _, kind := range declaration.Kinds {
		list := &unstructured.UnstructuredList{}
		list.SetAPIVersion(declaration.APIVersion)
		list.SetKind(kind + "List")
		if err := o.client.List(ctx, list, client.InNamespace(namespace)); err != nil {
			return nil, fmt.Errorf("list %s in %s: %w", kind, namespace, err)
		}
		for i := range list.Items {
			out = append(out, held(&list.Items[i]))
		}
	}
	return out, nil
}

// Create creates object. The fields it sets are under the field manager's update entry until the
// first Apply over the object hands them to its apply entry.
func (o *Objects) Create(ctx context.Context, object *unstructured.Unstructured) error {
	err := o.client.Create(ctx, object, client.FieldOwner(FieldManager), client.FieldValidation("Strict"))
	if apierrors.IsAlreadyExists(err) {
		return declaration.ErrNameTaken
	}
	if err != nil {
		return fmt.Errorf("create %s %s/%s: %w", object.GetKind(), object.GetNamespace(), object.GetName(), err)
	}
	return nil
}

// Apply applies object by server-side apply, forcing the fields a hand edit took (ADR 0089 decision 2),
// on the condition that the object is still the one held.
func (o *Objects) Apply(ctx context.Context, object *unstructured.Unstructured, over declaration.Held) error {
	live, err := o.live(ctx, over)
	if err != nil {
		return err
	}
	// A Create left the fields it set under an update entry, where an apply that drops one would leave
	// it in place.
	if err := o.upgrade(ctx, live); err != nil {
		return err
	}
	object.SetResourceVersion(live.GetResourceVersion())
	err = o.client.Apply(ctx, client.ApplyConfigurationFromUnstructured(object),
		client.FieldOwner(FieldManager), client.ForceOwnership)
	if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
		return declaration.ErrChanged
	}
	if err != nil {
		return fmt.Errorf("apply %s: %w", over.Ref, err)
	}
	return nil
}

func (o *Objects) Delete(ctx context.Context, held declaration.Held) error {
	object := newObject(held.Ref.Kind)
	object.SetNamespace(held.Ref.Namespace)
	object.SetName(held.Ref.Name)
	uid, version := types.UID(held.UID), held.ResourceVersion
	err := o.client.Delete(ctx, object, client.Preconditions{UID: &uid, ResourceVersion: &version})
	switch {
	case apierrors.IsNotFound(err):
		return nil
	case apierrors.IsConflict(err):
		return declaration.ErrChanged
	case err != nil:
		return fmt.Errorf("delete %s: %w", held.Ref, err)
	}
	return nil
}

// live is held as the cache has it now, and ErrChanged where that is another object or version.
func (o *Objects) live(ctx context.Context, held declaration.Held) (*unstructured.Unstructured, error) {
	live := newObject(held.Ref.Kind)
	err := o.client.Get(ctx, client.ObjectKey{Namespace: held.Ref.Namespace, Name: held.Ref.Name}, live)
	switch {
	case apierrors.IsNotFound(err):
		return nil, declaration.ErrChanged
	case err != nil:
		return nil, fmt.Errorf("read %s: %w", held.Ref, err)
	case string(live.GetUID()) != held.UID || live.GetResourceVersion() != held.ResourceVersion:
		return nil, declaration.ErrChanged
	}
	return live, nil
}

// upgrade moves the fields the field manager set by create or update to its apply entry. It changes
// nothing where there are none, and is refused where the object changed since it was read.
func (o *Objects) upgrade(ctx context.Context, object *unstructured.Unstructured) error {
	patch, err := csaupgrade.UpgradeManagedFieldsPatch(object, sets.New(FieldManager), FieldManager)
	if err != nil {
		return fmt.Errorf("prepare the managed fields of %s/%s: %w", object.GetNamespace(), object.GetName(), err)
	}
	if patch == nil {
		return nil
	}
	err = o.client.Patch(ctx, object, client.RawPatch(types.JSONPatchType, patch))
	if apierrors.IsConflict(err) || apierrors.IsNotFound(err) {
		// The patch carries the resourceVersion read, so another write came first.
		return declaration.ErrChanged
	}
	if err != nil {
		return fmt.Errorf("hand the managed fields of %s/%s to %s's apply: %w",
			object.GetNamespace(), object.GetName(), FieldManager, err)
	}
	return nil
}

func held(object *unstructured.Unstructured) declaration.Held {
	h := declaration.Held{
		Ref:             declaration.Ref{Kind: object.GetKind(), Namespace: object.GetNamespace(), Name: object.GetName()},
		UID:             string(object.GetUID()),
		ResourceVersion: object.GetResourceVersion(),
	}
	h.Marker, h.MarkerErr = botv1.ManagementOf(object)
	return h
}

func newObject(kind string) *unstructured.Unstructured {
	object := &unstructured.Unstructured{}
	object.SetAPIVersion(declaration.APIVersion)
	object.SetKind(kind)
	return object
}
