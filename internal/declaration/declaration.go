package declaration

import (
	"time"

	botv1 "github.com/garamsh/garam-bot/api/declaration/v1alpha1"
)

// ClaimName is the name of a namespace's claim, the Lease naming the release that acts in it (CLAIM-1).
const ClaimName = "garam-connector-claim"

// Kinds are the declared kinds, interface 1's at v1alpha1 (DECL-7).
var Kinds = []string{"Bot", "Workspace", "WorkspaceClass", "BotPool"}

// APIVersion is the apiVersion of every declared kind.
var APIVersion = botv1.GroupVersion.String()

// Outcome is what came of a declaration, as a report carries it (REP-1).
type Outcome string

const (
	// Applied is a declaration whose object is the cluster's garam-owned object for it.
	Applied Outcome = "applied"
	// Conflict is a declaration whose namespace and name another object holds (DECL-11).
	Conflict Outcome = "conflict"
	// Invalid is a declaration that breaks DECL-7, DECL-8 or DECL-9, or that the API server refused.
	Invalid Outcome = "invalid"
	// NamespaceNotWatched is a declaration for a namespace the connector does not watch (DECL-12).
	NamespaceNotWatched Outcome = "namespaceNotWatched"
)

// Result is one declaration's outcome.
type Result struct {
	ID         string
	Generation int64
	Outcome    Outcome
	Message    string
}

// Ref names one object of a declared kind.
type Ref struct {
	Kind      string
	Namespace string
	Name      string
}

func (r Ref) String() string { return r.Kind + " " + r.Namespace + "/" + r.Name }

// Object is an own object as a report carries it (REP-1, REP-2): this record's garam-owned object, or a
// cluster-owned one, in a claimed namespace. An object whose marker is malformed is neither (OWN-4).
type Object struct {
	Ref        Ref
	UID        string
	Generation int64
	Owner      botv1.Owner
	// DeclarationID and Orphaned are a garam-owned object's, and Spec a cluster-owned object's.
	DeclarationID string
	Orphaned      bool
	Spec          map[string]any
	Status        map[string]any
}

// Status is what the last pass found: what a report is built from.
type Status struct {
	// ObservedAt is when the pass read the cluster.
	ObservedAt time.Time
	ReportOnly bool
	// Namespaces are the watched namespaces whose claim names this release.
	Namespaces []string
	// ClaimedElsewhere are the watched namespaces with no claim, or a claim naming another release.
	ClaimedElsewhere []string
	// Declarations are the outcomes of the declarations held, by identifier. A declaration for a
	// namespace claimed elsewhere has none.
	Declarations []Result
	// Orphans are this record's garam-owned objects that no declaration held names (DECL-13).
	Orphans []Ref
	// Objects are the own objects of the claimed namespaces, by namespace, kind and name.
	Objects []Object
}
