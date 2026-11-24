package declaration

import (
	"encoding/json"
	"errors"
	"fmt"

	botv1 "github.com/garamsh/garam-bot/api/declaration/v1alpha1"
	"github.com/garamsh/garam-bot/api/declaration/v1alpha1/manifest"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	utiljson "k8s.io/apimachinery/pkg/util/json"
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

// declaredObject is what DECL-7 lets a declaration's object carry.
type declaredObject struct {
	APIVersion string `json:"apiVersion"`
	Kind       string `json:"kind"`
	Metadata   struct {
		Namespace   string            `json:"namespace"`
		Name        string            `json:"name"`
		Labels      map[string]string `json:"labels,omitempty"`
		Annotations map[string]string `json:"annotations,omitempty"`
	} `json:"metadata"`
	Spec json.RawMessage `json:"spec"`
}

// declare reads an apply change for record. A declaration whose object breaks DECL-7, DECL-8 or DECL-9
// is held as invalid, so its outcome is reported and nothing of it is applied (DECL-12).
func declare(record, id string, generation int64, raw json.RawMessage) declared {
	d := declared{id: id, generation: generation}
	var object declaredObject
	if err := json.Unmarshal(raw, &object); err != nil {
		d.invalid = "the object is not a JSON object of DeclaredObject's shape"
		return d
	}
	d.ref = Ref{Kind: object.Kind, Namespace: object.Metadata.Namespace, Name: object.Metadata.Name}
	if object.Metadata.Namespace == "" || len(object.Spec) == 0 {
		d.invalid = "the object carries no metadata.namespace or no spec (DECL-7)"
		return d
	}
	// interface 1's own parser admits what the API server admits under strict field validation: a
	// declared kind (DECL-7), secrets by reference alone (DECL-8), and no image, since no field names
	// one (DECL-9, IMG-1).
	if _, err := manifest.Parse(raw); err != nil {
		d.invalid = bounded(err.Error())
		return d
	}
	applied, err := applyObject(record, d, object)
	if err != nil {
		d.invalid = bounded(err.Error())
		return d
	}
	d.object = applied
	return d
}

// applyObject is the object applied for d: the declaration's own members, and the ownership marker
// naming record, d's identifier and its generation (DECL-11). Nothing else of the object is applied.
func applyObject(record string, d declared, object declaredObject) (*unstructured.Unstructured, error) {
	var spec map[string]any
	// apimachinery's decoder keeps an integer an int64, as an object read from the API server holds it.
	if err := utiljson.Unmarshal(object.Spec, &spec); err != nil || spec == nil {
		return nil, errors.New("spec is not a JSON object")
	}
	applied := &unstructured.Unstructured{Object: map[string]any{"spec": spec}}
	applied.SetAPIVersion(object.APIVersion)
	applied.SetKind(object.Kind)
	applied.SetNamespace(object.Metadata.Namespace)
	applied.SetName(object.Metadata.Name)
	applied.SetLabels(object.Metadata.Labels)
	applied.SetAnnotations(object.Metadata.Annotations)
	err := botv1.MarkGaramOwned(applied, botv1.GaramDeclaration{Cluster: record, ID: d.id, Generation: d.generation})
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
