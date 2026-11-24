package declaration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const (
	declarationsPath = "/cluster-connection/v1alpha1/clusters/self/declarations"
	// pollWait is DECL-3's longest hold, asked for on every poll.
	pollWait = 50
	// pollTimeout bounds one poll: the hold, and the time to answer it.
	pollTimeout = (pollWait + 30) * time.Second
	// firstBackoff and lastBackoff are T-6's wait after a 5xx or no answer: 1 second, doubling to 60.
	firstBackoff = time.Second
	lastBackoff  = 60 * time.Second
	// maxPageBytes bounds one page read.
	maxPageBytes = 32 << 20
)

// change and page are DeclarationPage's members (DECL-1, DECL-2).
type change struct {
	Op         string          `json:"op"`
	ID         string          `json:"id"`
	Generation int64           `json:"generation"`
	Object     json.RawMessage `json:"object"`
}

type page struct {
	Cursor  string   `json:"cursor"`
	Full    bool     `json:"full"`
	More    bool     `json:"more"`
	Changes []change `json:"changes"`
}

// errCursorExpired is DECL-6's 410: the full set is read again.
var errCursorExpired = errors.New("garam answered 410: the cursor expired")

// unanswered is a poll that came back without a page: its status, and the wait a 429 asked for.
type unanswered struct {
	status     int
	retryAfter time.Duration
	err        error
}

func (u *unanswered) Error() string {
	if u.err != nil {
		return u.err.Error()
	}
	return fmt.Sprintf("garam answered %d", u.status)
}

func (u *unanswered) Unwrap() error { return u.err }

// feed reads the declarations from garam into the service, from the full set on (DECL-1 to DECL-6).
func (s *service) feed(ctx context.Context) error {
	cursor := ""
	// staged collects a full set until its last page, since only then is it complete (DECL-5).
	var staged map[string]declared
	wait := firstBackoff
	for {
		got, err := s.poll(ctx, cursor)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var refused *unanswered
		switch {
		case errors.Is(err, errCursorExpired):
			s.cfg.Log.Warn("the declarations cursor expired; reading the full set again")
			cursor, staged = "", nil
			continue
		case errors.As(err, &refused) && refused.status == http.StatusTooManyRequests:
			if err := s.cfg.Sleep(ctx, refused.retryAfter); err != nil {
				return err
			}
			continue
		case err != nil:
			s.cfg.Log.Warn("declarations not read; asking again", "error", err, "wait", wait)
			if err := s.cfg.Sleep(ctx, wait); err != nil {
				return err
			}
			wait = min(2*wait, lastBackoff)
			continue
		}
		wait = firstBackoff
		if cursor == "" && staged == nil {
			staged = map[string]declared{}
		}
		if staged != nil {
			for _, c := range got.Changes {
				if c.Op == "apply" {
					staged[c.ID] = declare(s.cfg.Record, c.ID, c.Generation, c.Object)
				}
			}
			if !got.More {
				s.replace(staged)
				staged = nil
			}
		} else {
			s.take(got.Changes)
		}
		cursor = got.Cursor
	}
}

// poll asks for the changes after cursor, the full set where cursor is empty.
func (s *service) poll(ctx context.Context, cursor string) (page, error) {
	ctx, cancel := context.WithTimeout(ctx, pollTimeout)
	defer cancel()
	query := url.Values{"wait": {strconv.Itoa(pollWait)}}
	if cursor != "" {
		query.Set("after", cursor)
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, s.cfg.BaseURL+declarationsPath+"?"+query.Encode(), nil)
	if err != nil {
		return page{}, fmt.Errorf("build the declarations request: %w", err)
	}
	response, err := s.cfg.HTTP.Do(request)
	if err != nil {
		return page{}, &unanswered{err: err}
	}
	defer func() { _ = response.Body.Close() }()
	switch {
	case response.StatusCode == http.StatusGone:
		return page{}, errCursorExpired
	case response.StatusCode == http.StatusTooManyRequests:
		return page{}, &unanswered{status: response.StatusCode, retryAfter: retryAfter(response.Header.Get("Retry-After"))}
	case response.StatusCode != http.StatusOK:
		return page{}, &unanswered{status: response.StatusCode}
	}
	read, err := io.ReadAll(io.LimitReader(response.Body, maxPageBytes))
	if err != nil {
		return page{}, &unanswered{status: response.StatusCode, err: fmt.Errorf("read the declarations: %w", err)}
	}
	var got page
	if err := json.Unmarshal(read, &got); err != nil || got.Cursor == "" {
		return page{}, &unanswered{status: response.StatusCode, err: errors.New("the declarations answer is not a DeclarationPage")}
	}
	return got, nil
}

// retryAfter reads Retry-After as seconds, and as T-6's first wait where it is missing or unreadable.
func retryAfter(value string) time.Duration {
	seconds, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil || seconds < 1 {
		return firstBackoff
	}
	return time.Duration(seconds) * time.Second
}
