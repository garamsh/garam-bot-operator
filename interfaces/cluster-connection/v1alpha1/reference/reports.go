package reference

import (
	"encoding/json"
	"net/http"
	"slices"
	"time"
)

// maxReportBytes is REP-6's bound.
const maxReportBytes = 4 << 20

// Report is Report of openapi.yaml, as this server keeps it.
type Report struct {
	ObservedAt   time.Time            `json:"observedAt"`
	Connector    ReportConnector      `json:"connector"`
	Declarations []DeclarationOutcome `json:"declarations"`
	Objects      []ReportedObject     `json:"objects"`
}

// ReportConnector is a report's connector member.
type ReportConnector struct {
	Version          string   `json:"version"`
	Mode             string   `json:"mode"`
	Namespaces       []string `json:"namespaces"`
	ClaimedElsewhere []string `json:"claimedElsewhere,omitempty"`
}

// DeclarationOutcome is DeclarationOutcome of openapi.yaml.
type DeclarationOutcome struct {
	ID         string `json:"id"`
	Generation int64  `json:"generation"`
	Outcome    string `json:"outcome"`
	Message    string `json:"message,omitempty"`
}

// ReportedObject is ReportedObject of openapi.yaml. spec and status are display data (REP-9), kept as
// they came and never read.
type ReportedObject struct {
	APIVersion    string          `json:"apiVersion"`
	Kind          string          `json:"kind"`
	Namespace     string          `json:"namespace"`
	Name          string          `json:"name"`
	UID           string          `json:"uid"`
	Owner         string          `json:"owner"`
	Generation    int64           `json:"generation"`
	Cluster       GRN             `json:"cluster,omitempty"`
	DeclarationID string          `json:"declarationId,omitempty"`
	Orphaned      *bool           `json:"orphaned,omitempty"`
	Spec          json.RawMessage `json:"spec,omitempty"`
	Status        json.RawMessage `json:"status"`
}

var (
	modes    = []string{"apply", "reportOnly"}
	outcomes = []string{"applied", "conflict", "invalid", "namespaceNotWatched"}
	owners   = []string{"garam", "cluster"}
)

func (s *Server) report(w http.ResponseWriter, r *http.Request) error {
	tooLarge := refuse(http.StatusRequestEntityTooLarge, codeReportTooLarge, "the report is over 4 MiB")
	if err := s.authenticateFirst(r); err != nil {
		return err
	}
	var report Report
	if err := decodeBody(w, r, maxReportBytes, tooLarge, &report); err != nil {
		return err
	}
	if err := checkReport(report); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()
	c, err := s.authenticate(r)
	if err != nil {
		return err
	}
	if c.report == nil || report.ObservedAt.After(c.report.ObservedAt) {
		kept := s.placedOn(c.grn, report)
		c.report = &kept
	}
	w.WriteHeader(http.StatusNoContent)
	return nil
}

// checkReport refuses a report missing a required member, or carrying an object of a kind that is not
// declared, a Secret and a ConfigMap among them (REP-7).
func checkReport(report Report) error {
	invalid := func(format string, args ...any) error {
		return refuse(http.StatusBadRequest, codeInvalidRequest, format, args...)
	}
	switch {
	case report.ObservedAt.IsZero():
		return invalid("observedAt is missing")
	case report.Connector.Version == "" || !slices.Contains(modes, report.Connector.Mode):
		return invalid("connector.version or connector.mode is missing or unknown")
	case report.Connector.Namespaces == nil || report.Declarations == nil || report.Objects == nil:
		return invalid("connector.namespaces, declarations or objects is missing")
	}
	for _, o := range report.Declarations {
		if o.ID == "" || o.Generation < 1 || !slices.Contains(outcomes, o.Outcome) {
			return invalid("declaration outcome %q is incomplete or unknown", o.ID)
		}
	}
	for _, o := range report.Objects {
		if o.APIVersion != declaredAPIVersion || !slices.Contains(declaredKinds, o.Kind) {
			return invalid("REP-7: an object of %s %s is not of a declared kind", o.APIVersion, o.Kind)
		}
		if !slices.Contains(owners, o.Owner) || o.Generation < 1 || o.Status == nil {
			return invalid("object %s/%s lacks owner, generation or status", o.Namespace, o.Name)
		}
	}
	return nil
}

// placedOn is report without the outcomes and objects that name a declaration not placed on grn (REP-8).
// The caller holds s.mu.
func (s *Server) placedOn(grn GRN, report Report) Report {
	placed := func(id string) bool {
		d, ok := s.declarations[id]
		return ok && d.cluster == grn
	}
	report.Declarations = slices.DeleteFunc(report.Declarations, func(o DeclarationOutcome) bool {
		return !placed(o.ID)
	})
	report.Objects = slices.DeleteFunc(report.Objects, func(o ReportedObject) bool {
		if o.Owner != "garam" {
			return o.DeclarationID != "" && !placed(o.DeclarationID)
		}
		return !placed(o.DeclarationID) || (o.Cluster != "" && o.Cluster != grn)
	})
	return report
}

// LatestReport is the fixture's latest-report answer.
type LatestReport struct {
	Cluster       GRN        `json:"cluster"`
	LastContactAt *time.Time `json:"lastContactAt"`
	Report        *Report    `json:"report"`
}

func (s *Server) latestReport(grn GRN) (LatestReport, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c, ok := s.clusters[grn]
	if !ok {
		return LatestReport{}, errNoCluster
	}
	answer := LatestReport{Cluster: grn, Report: c.report}
	if !c.lastContactAt.IsZero() {
		contact := c.lastContactAt
		answer.LastContactAt = &contact
	}
	return answer, nil
}
