package declaration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"time"

	botv1 "github.com/garamsh/garam-bot/api/declaration/v1alpha1"
)

const (
	reportsPath = "/cluster-connection/v1alpha1/clusters/self/reports"
	// reportEvery is REP-4's longest time between two reports.
	reportEvery = 5 * time.Minute
	// reportTimeout bounds one report's request.
	reportTimeout = 30 * time.Second
	// modeApply and modeReportOnly are a report's connector.mode.
	modeApply      = "apply"
	modeReportOnly = "reportOnly"
)

// report and its members are Report of openapi.yaml (REP-1, REP-2).
type report struct {
	ObservedAt   time.Time        `json:"observedAt"`
	Connector    reportConnector  `json:"connector"`
	Declarations []reportOutcome  `json:"declarations"`
	Objects      []reportedObject `json:"objects"`
}

type reportConnector struct {
	Version          string   `json:"version"`
	Mode             string   `json:"mode"`
	Namespaces       []string `json:"namespaces"`
	ClaimedElsewhere []string `json:"claimedElsewhere"`
}

type reportOutcome struct {
	ID         string  `json:"id"`
	Generation int64   `json:"generation"`
	Outcome    Outcome `json:"outcome"`
	Message    string  `json:"message,omitempty"`
}

type reportedObject struct {
	APIVersion    string         `json:"apiVersion"`
	Kind          string         `json:"kind"`
	Namespace     string         `json:"namespace"`
	Name          string         `json:"name"`
	UID           string         `json:"uid"`
	Owner         botv1.Owner    `json:"owner"`
	Generation    int64          `json:"generation"`
	Cluster       string         `json:"cluster,omitempty"`
	DeclarationID string         `json:"declarationId,omitempty"`
	Orphaned      *bool          `json:"orphaned,omitempty"`
	Spec          map[string]any `json:"spec,omitempty"`
	Status        map[string]any `json:"status"`
}

// reports sends the last pass's whole view within moments of each pass that found it changed, and at
// least every 5 minutes (REP-1, REP-4). A report garam did not take is sent again after T-6's wait.
func (s *service) reports(ctx context.Context) error {
	var sent *Status
	var due time.Time
	wait := firstBackoff
	var retry <-chan time.Time
	for {
		periodic := s.cfg.After(reportEvery)
		if !due.IsZero() {
			periodic = s.cfg.After(max(due.Sub(s.cfg.Now()), 0))
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-s.reported:
		case <-periodic:
			sent = nil
		case <-retry:
		}
		retry = nil
		status, ok := s.Status()
		if !ok || (sent != nil && sameView(*sent, status)) {
			continue
		}
		err := s.send(ctx, status)
		var refused *unanswered
		switch {
		case err == nil, errors.As(err, &refused) && refused.status == http.StatusRequestEntityTooLarge:
			if err != nil {
				// REP-6: reported, and declarations go on being applied. The next change is reported again.
				s.cfg.Log.Error("garam refused the report as too large", "objects", len(status.Objects))
			}
			sent, due, wait = &status, s.cfg.Now().Add(reportEvery), firstBackoff
		case ctx.Err() != nil:
			return ctx.Err()
		case errors.As(err, &refused) && refused.status == http.StatusTooManyRequests:
			retry = s.cfg.After(refused.retryAfter)
		default:
			s.cfg.Log.Warn("the report was not taken; sending it again", "error", err, "wait", wait)
			retry = s.cfg.After(wait)
			wait = min(2*wait, lastBackoff)
		}
	}
}

// sameView is whether two passes found the same view, whenever each observed it.
func sameView(a, b Status) bool {
	a.ObservedAt, b.ObservedAt = time.Time{}, time.Time{}
	return reflect.DeepEqual(a, b)
}

// send posts status as one report.
func (s *service) send(ctx context.Context, status Status) error {
	ctx, cancel := context.WithTimeout(ctx, reportTimeout)
	defer cancel()
	body, err := json.Marshal(s.reportOf(status))
	if err != nil {
		return fmt.Errorf("encode the report: %w", err)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, s.cfg.BaseURL+reportsPath, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("build the report request: %w", err)
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := s.cfg.HTTP.Do(request)
	if err != nil {
		return &unanswered{err: err}
	}
	defer func() { _ = response.Body.Close() }()
	switch response.StatusCode {
	case http.StatusNoContent:
		return nil
	case http.StatusTooManyRequests:
		return &unanswered{status: response.StatusCode, retryAfter: retryAfter(response.Header.Get("Retry-After"))}
	}
	return &unanswered{status: response.StatusCode}
}

// reportOf is status as a report carries it.
func (s *service) reportOf(status Status) report {
	mode := modeApply
	if status.ReportOnly {
		mode = modeReportOnly
	}
	out := report{
		ObservedAt: status.ObservedAt.UTC(),
		Connector: reportConnector{Version: s.cfg.Version, Mode: mode, Namespaces: nonNil(status.Namespaces),
			ClaimedElsewhere: nonNil(status.ClaimedElsewhere)},
		Declarations: make([]reportOutcome, 0, len(status.Declarations)),
		Objects:      make([]reportedObject, 0, len(status.Objects)),
	}
	for _, r := range status.Declarations {
		out.Declarations = append(out.Declarations, reportOutcome(r))
	}
	for _, o := range status.Objects {
		reported := reportedObject{APIVersion: APIVersion, Kind: o.Ref.Kind, Namespace: o.Ref.Namespace,
			Name: o.Ref.Name, UID: o.UID, Owner: o.Owner, Generation: o.Generation, Spec: o.Spec, Status: o.Status}
		if reported.Status == nil {
			reported.Status = map[string]any{}
		}
		if o.Owner == botv1.OwnerGaram {
			orphaned := o.Orphaned
			reported.Cluster, reported.DeclarationID, reported.Orphaned = s.cfg.Record, o.DeclarationID, &orphaned
		}
		out.Objects = append(out.Objects, reported)
	}
	return out
}

func nonNil(values []string) []string {
	if values == nil {
		return []string{}
	}
	return values
}
