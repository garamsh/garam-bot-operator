package declaration

import (
	"context"
	"errors"

	botv1 "github.com/garamsh/garam-bot/api/declaration/v1alpha1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
)

// Held is an object of a declared kind as the cluster holds it.
type Held struct {
	Ref             Ref
	UID             string
	ResourceVersion string
	// Generation is metadata.generation, and Spec and Status the object's own, as a report carries them
	// (REP-2). Each is a copy the pipeline may keep.
	Generation int64
	Spec       map[string]any
	Status     map[string]any
	// Marker is the object's ownership marker. MarkerErr is set in its place where the marker is
	// malformed, and such an object is acted on as neither owner's (OWN-4).
	Marker    botv1.Management
	MarkerErr error
}

// Objects is the cluster, as the pipeline reads and writes it: the watched namespaces alone.
type Objects interface {
	// Claim answers the holderIdentity of namespace's claim, and false where it has none.
	Claim(ctx context.Context, namespace string) (string, bool, error)
	// List answers every object of a declared kind in namespace.
	List(ctx context.Context, namespace string) ([]Held, error)
	// Create creates object where nothing holds its name, and answers ErrNameTaken otherwise.
	Create(ctx context.Context, object *unstructured.Unstructured) error
	// Apply applies object over held by server-side apply, and answers ErrChanged where the object is
	// no longer the one held.
	Apply(ctx context.Context, object *unstructured.Unstructured, held Held) error
	// Delete deletes held, and answers ErrChanged where the object is no longer the one held.
	Delete(ctx context.Context, held Held) error
	// Changed is signalled when an object of a declared kind or a claim changes.
	Changed() <-chan struct{}
}

var (
	// ErrNameTaken is a Create whose name an object took after the pass read the cluster.
	ErrNameTaken = errors.New("another object took the name")
	// ErrChanged is a write to an object changed after the pass read it.
	ErrChanged = errors.New("the object changed after it was read")
)
