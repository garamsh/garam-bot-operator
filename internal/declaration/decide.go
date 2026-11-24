package declaration

import (
	"cmp"
	"fmt"
	"maps"
	"slices"

	botv1 "github.com/garamsh/garam-bot/api/declaration/v1alpha1"
)

// view is the cluster as one pass read it.
type view struct {
	// claimed are the watched namespaces whose claim names this release, and elsewhere the others.
	claimed   map[string]bool
	elsewhere []string
	// objects are every object of a declared kind in the claimed namespaces.
	objects []Held
}

// writeOp is what a write does.
type writeOp int

const (
	create writeOp = iota + 1
	apply
	remove
)

// write is one write a pass carries out.
type write struct {
	op          writeOp
	declaration declared
	// held is the object written over or deleted; it is zero for a create.
	held Held
}

// decision is what one pass decided: the writes, each held declaration's outcome, the orphans, and the
// tombstones carried out, which are no longer held.
type decision struct {
	writes    []write
	results   map[string]Result
	orphans   []Ref
	buried    []string
	claimed   []string
	elsewhere []string
}

// decide is the pipeline's one decision, from the declarations and tombstones held and the cluster as
// read: what to write, and what came of each declaration. It writes nothing itself.
func decide(record string, reportOnly bool, watched map[string]bool, held map[string]declared,
	tombstones map[string]bool, seen view,
) decision {
	byRef := make(map[Ref]Held, len(seen.objects))
	own := map[string][]Held{}
	for _, object := range seen.objects {
		byRef[object.Ref] = object
		if id, ok := ownID(record, object); ok {
			own[id] = append(own[id], object)
		}
	}
	d := decision{results: map[string]Result{}, claimed: sortedKeys(seen.claimed), elsewhere: seen.elsewhere}
	for _, id := range sortedKeys(held) {
		declaration := held[id]
		result, w := settle(record, reportOnly, watched, seen.claimed, declaration, byRef)
		if result != nil {
			d.results[id] = *result
		}
		if w != nil {
			d.writes = append(d.writes, *w)
		}
	}
	for _, id := range sortedKeys(tombstones) {
		objects := own[id]
		if len(objects) == 0 {
			// Nothing of it is left in a claimed namespace, so the tombstone has been carried out.
			d.buried = append(d.buried, id)
			continue
		}
		if reportOnly {
			continue
		}
		for _, object := range objects {
			d.writes = append(d.writes, write{op: remove, held: object})
		}
	}
	for _, id := range sortedKeys(own) {
		declaration, named := held[id]
		for _, object := range own[id] {
			// DECL-13: an object no declaration held names is reported, and only a tombstone deletes it.
			if !named || declaration.ref != object.Ref {
				d.orphans = append(d.orphans, object.Ref)
			}
		}
	}
	slices.SortFunc(d.orphans, compareRefs)
	return d
}

// settle decides one held declaration: its outcome, if it has one, and the write it needs, if any.
func settle(record string, reportOnly bool, watched, claimed map[string]bool, d declared,
	byRef map[Ref]Held,
) (*Result, *write) {
	result := func(outcome Outcome, generation int64, message string) *Result {
		return &Result{ID: d.id, Generation: generation, Outcome: outcome, Message: message}
	}
	switch {
	case d.object == nil:
		return result(Invalid, d.generation, d.invalid), nil
	case !watched[d.ref.Namespace]:
		return result(NamespaceNotWatched, d.generation, "the connector does not watch namespace "+d.ref.Namespace), nil
	case !claimed[d.ref.Namespace]:
		// CLAIM-2: nothing is applied, deleted or reported in a namespace claimed elsewhere.
		return nil, nil
	}
	object, present := byRef[d.ref]
	if !present {
		if reportOnly {
			return nil, nil
		}
		return result(Applied, d.generation, ""), &write{op: create, declaration: d}
	}
	if why := heldOtherwise(record, d.id, object); why != "" {
		return result(Conflict, d.generation, fmt.Sprintf("%s is held by %s", d.ref, why)), nil
	}
	applied, _ := object.Marker.Garam()
	switch {
	case applied.Generation > d.generation:
		// DECL-10: a lower generation than the one applied is never applied.
		return result(Applied, applied.Generation, ""), nil
	case reportOnly && applied.Generation < d.generation:
		return nil, nil
	case reportOnly:
		return result(Applied, d.generation, ""), nil
	}
	// The same generation is applied again, so a hand edit of the object is reverted (ADR 0089).
	return result(Applied, d.generation, ""), &write{op: apply, declaration: d, held: object}
}

// heldOtherwise says what holds the object's name where it is not this record's garam-owned object for
// id, and answers "" where it is (DECL-11, DECL-14).
func heldOtherwise(record, id string, object Held) string {
	if object.MarkerErr != nil {
		return "an object whose ownership marker is malformed"
	}
	applied, garamOwned := object.Marker.Garam()
	switch {
	case !garamOwned:
		return "a cluster-owned object"
	case applied.Cluster != record:
		return "a garam-owned object of another cluster record"
	case applied.ID != id:
		return "this record's object for declaration " + applied.ID
	}
	return ""
}

// ownID answers the declaration identifier of record's own garam-owned object.
func ownID(record string, object Held) (string, bool) {
	if object.MarkerErr != nil || object.Marker.Owner() != botv1.OwnerGaram {
		return "", false
	}
	applied, _ := object.Marker.Garam()
	return applied.ID, applied.Cluster == record
}

func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

func compareRefs(a, b Ref) int {
	return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Kind, b.Kind), cmp.Compare(a.Name, b.Name))
}
