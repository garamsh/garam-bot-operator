package declaration

import (
	"encoding/json"
	"fmt"

	botv1 "github.com/garamsh/garam-bot/api/declaration/v1alpha1"
	"github.com/garamsh/garam-bot/api/declaration/v1alpha1/manifest"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
)

// maxMessage bounds a Result's message, as REP-1's DeclarationOutcome does.
const maxMessage = 1024

// declared is one declaration the connector holds: the latest generation received for its identifier.
type declared struct {
	id         string
	generation int64
	ref        Ref
	// object is what is applied: the declaration's object, marked as this record's. It is nil where
	// the declaration is invalid, and invalid says why.
	object  *unstructured.Unstructured
	invalid string
}

// declare reads an apply change for record. A declaration whose object breaks DECL-7, DECL-8 or DECL-9
// is held as invalid, so its outcome is reported and nothing of it is applied (DECL-12).
func declare(record, id string, generation int64, raw json.RawMessage) declared {
	d := declared{id: id, generation: generation}
	// interface 1's own parser admits what the API server admits under strict field validation: a
	// declared kind (DECL-7), secrets by reference alone (DECL-8), and no image, since no field names
	// one (DECL-9, IMG-1). What it answers is all that is applied, so what is checked is what is applied.
	parsed, err := manifest.Parse(raw)
	if err != nil {
		d.invalid = bounded(err.Error())
		return d
	}
	kind := parsed.GetObjectKind().GroupVersionKind().Kind
	d.ref = Ref{Kind: kind, Namespace: parsed.GetNamespace(), Name: parsed.GetName()}
	if d.ref.Namespace == "" {
		d.invalid = "the object carries no metadata.namespace (DECL-7)"
		return d
	}
	applied, err := applyObject(record, d, parsed)
	if err != nil {
		d.invalid = bounded(err.Error())
		return d
	}
	d.object = applied
	return d
}

// applyObject is the object applied for d: the parsed object's kind, namespace, name, labels,
// annotations and spec, with its defaults, and the ownership marker naming record, d's identifier and
// its generation (DECL-11). Nothing else of the object is applied.
func applyObject(record string, d declared, parsed manifest.Object) (*unstructured.Unstructured, error) {
	content, err := runtime.DefaultUnstructuredConverter.ToUnstructured(parsed)
	if err != nil {
		return nil, fmt.Errorf("the parsed object cannot be read: %w", err)
	}
	spec, ok := content["spec"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("the parsed object carries no spec")
	}
	applied := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	applied.SetAPIVersion(APIVersion)
	applied.SetKind(d.ref.Kind)
	applied.SetNamespace(d.ref.Namespace)
	applied.SetName(d.ref.Name)
	applied.SetLabels(parsed.GetLabels())
	applied.SetAnnotations(parsed.GetAnnotations())
	err = botv1.MarkGaramOwned(applied, botv1.GaramDeclaration{Cluster: record, ID: d.id, Generation: d.generation})
	if err != nil {
		return nil, fmt.Errorf("the declaration cannot be marked: %w", err)
	}
	return applied, nil
}

func bounded(message string) string {
	if len(message) <= maxMessage {
		return message
	}
	return message[:maxMessage-3] + "..."
}
