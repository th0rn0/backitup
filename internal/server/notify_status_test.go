package server

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

// webhookRecorder stands in for a Discord webhook and records what was posted.
type webhookRecorder struct {
	mu   sync.Mutex
	msgs []string
	srv  *httptest.Server
}

func newWebhookRecorder(t *testing.T) *webhookRecorder {
	t.Helper()
	rec := &webhookRecorder{}
	rec.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Errorf("read webhook body: %v", err)
		}
		var payload struct {
			Content string `json:"content"`
		}
		if err := json.Unmarshal(body, &payload); err != nil {
			t.Errorf("webhook body is not JSON: %v", err)
		}
		rec.mu.Lock()
		rec.msgs = append(rec.msgs, payload.Content)
		rec.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(rec.srv.Close)
	return rec
}

func (r *webhookRecorder) messages() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.msgs))
	copy(out, r.msgs)
	return out
}

// notifyOne drives the notifier for a single status and returns what was posted.
func notifyOne(t *testing.T, verbose bool, st model.RunStatus) []string {
	t.Helper()
	rec := newWebhookRecorder(t)
	s := &Server{}
	s.ConfigureDiscord(rec.srv.URL)
	s.ConfigureVerbose(verbose)
	s.ConfigureTimezone(time.UTC)
	s.notifyStatus(&model.Client{Name: "laptop", SourceLabel: "/home/th0rn0/docs"}, st, 12, 4096,
		time.Date(2026, 11, 25, 20, 0, 0, 0, time.UTC))
	return rec.messages()
}

// The outcome of a run always reaches the channel, whatever verbose says. These
// are the two messages an operator acts on, and before this they were behind a
// flag that also gated the start notice, so turning the noise off turned the
// success report off with it.
func TestNotifyOutcomeAlwaysFires(t *testing.T) {
	for _, tc := range []struct {
		name string
		st   model.RunStatus
		want string
	}{
		{"ok", model.StatusOK, "backup **OK**"},
		{"failed", model.StatusFailed, "backup **FAILED**"},
	} {
		for _, verbose := range []bool{false, true} {
			t.Run(tc.name, func(t *testing.T) {
				msgs := notifyOne(t, verbose, tc.st)
				if len(msgs) != 1 {
					t.Fatalf("verbose=%v: got %d messages, want 1: %q", verbose, len(msgs), msgs)
				}
				if !strings.Contains(msgs[0], tc.want) {
					t.Fatalf("verbose=%v: message %q does not carry %q", verbose, msgs[0], tc.want)
				}
			})
		}
	}
}

// A run that has only started is not news: the schedule already said it would
// run. Sending it doubles the traffic for every client and buries the outcome.
func TestNotifyStartIsNeverSent(t *testing.T) {
	for _, verbose := range []bool{false, true} {
		msgs := notifyOne(t, verbose, model.StatusRunning)
		if len(msgs) != 0 {
			t.Fatalf("verbose=%v: got %d messages for a started run, want none: %q", verbose, len(msgs), msgs)
		}
	}
}

// An overlap is a diagnostic about cadence rather than an outcome, so it stays
// behind verbose mode.
func TestNotifyOverlapOnlyWithVerbose(t *testing.T) {
	if msgs := notifyOne(t, false, model.StatusOverlap); len(msgs) != 0 {
		t.Fatalf("verbose=false: got %d overlap messages, want none: %q", len(msgs), msgs)
	}
	msgs := notifyOne(t, true, model.StatusOverlap)
	if len(msgs) != 1 || !strings.Contains(msgs[0], "SKIPPED") {
		t.Fatalf("verbose=true: got %q, want one message carrying SKIPPED", msgs)
	}
}
