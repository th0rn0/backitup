package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/th0rn0/backitup/internal/alert"
	"github.com/th0rn0/backitup/internal/auth"
	"github.com/th0rn0/backitup/internal/model"
)

type ctxKey int

const clientCtxKey ctxKey = 0

// requireClient authenticates a client by bearer token and puts the matched
// client in the request context. Failures return a generic 401 (no enumeration).
func (s *Server) requireClient(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		tok := bearerToken(r)
		if tok == "" {
			unauthorized(w)
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
		defer cancel()
		cl, err := s.clientByToken(ctx, tok)
		if err != nil || cl == nil {
			unauthorized(w)
			return
		}
		next(w, r.WithContext(context.WithValue(r.Context(), clientCtxKey, cl)))
	}
}

func clientFrom(ctx context.Context) *model.Client {
	c, _ := ctx.Value(clientCtxKey).(*model.Client)
	return c
}

func bearerToken(r *http.Request) string {
	if after, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer "); ok {
		return strings.TrimSpace(after)
	}
	return ""
}

// clientByToken finds the enabled client whose token hash verifies against the
// presented token. TokenPrefix (first 8 chars, non-secret) is compared first so
// argon2 verification only runs for the (usually 1) matching client.
func (s *Server) clientByToken(ctx context.Context, token string) (*model.Client, error) {
	clients, err := s.st.ListClients(ctx)
	if err != nil {
		return nil, err
	}
	prefix := ""
	if len(token) >= 8 {
		prefix = token[:8]
	}
	for i := range clients {
		c := clients[i]
		if !c.Enabled || c.TokenHash == "" {
			continue
		}
		if c.TokenPrefix != "" && c.TokenPrefix != prefix {
			continue
		}
		if ok, err := auth.VerifyPassword(token, c.TokenHash); err == nil && ok {
			return &c, nil
		}
	}
	return nil, nil
}

// getConfig returns the calling client's behaviour (D1: server owns WHAT).
// It also clears any run stuck in "running" for this client — a new getConfig
// call means a new backup session is starting, so the previous session is gone.
func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	cl := clientFrom(r.Context())
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := s.st.MarkClientRunningRunFailed(ctx, cl.ID); err != nil {
		log.Printf("getConfig: clear stale run for client=%q: %v", cl.Name, err)
	}
	excludes := cl.Excludes
	if excludes == nil {
		excludes = []string{}
	}
	latestPath := filepath.Join(s.backupBaseDir, model.Slug(cl.Name), "snapshots", "latest")
	_, hasPrev := os.Lstat(latestPath)
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":                  cl.Mode,
		"excludes":              excludes,
		"retention_days":        cl.RetentionDays,
		"skip_symlinks":         cl.SkipSymlinks,
		"has_previous_snapshot": hasPrev == nil,
	})
}

type statusReq struct {
	Status     string    `json:"status"`
	RunID      int64     `json:"run_id"`
	Bytes      int64     `json:"bytes"`
	Files      int64     `json:"files"`
	SnapshotID string    `json:"snapshot_id"`
	StartedAt  time.Time `json:"started_at"`
	FinishedAt time.Time `json:"finished_at"`
	LogTail    string    `json:"log_tail"`
}

// postStatus records a client's run result (drives dashboard truthfulness).
// When status=running a new run row is inserted and its ID is returned so the
// client can update it with the final status. When run_id is non-zero the
// existing row is updated instead of inserting a new one.
func (s *Server) postStatus(w http.ResponseWriter, r *http.Request) {
	cl := clientFrom(r.Context())
	var req statusReq
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, int64(model.MaxLogTail)+4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		http.Error(w, "invalid JSON body", http.StatusBadRequest)
		return
	}
	st := model.RunStatus(req.Status)
	if st != model.StatusOK && st != model.StatusFailed && st != model.StatusOverlap && st != model.StatusRunning {
		http.Error(w, "invalid status", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	// Update path: client is finalising a run it previously started.
	if req.RunID > 0 && st != model.StatusRunning {
		finished := req.FinishedAt
		if finished.IsZero() {
			finished = time.Now().UTC()
		}
		if err := s.st.UpdateRun(ctx, req.RunID, cl.ID, model.Run{
			FinishedAt: finished, Status: st,
			Bytes: req.Bytes, Files: req.Files, SnapshotID: req.SnapshotID, LogTail: req.LogTail,
		}); err != nil {
			http.Error(w, "failed to update run", http.StatusInternalServerError)
			return
		}
		if s.verbose {
			log.Printf("status: client=%q run=%d status=%s files=%d bytes=%d", cl.Name, req.RunID, st, req.Files, req.Bytes)
		}
		go s.notifyStatus(cl, st, req.Files, req.Bytes, finished)
		writeJSON(w, http.StatusCreated, map[string]any{"run_id": req.RunID})
		return
	}

	// Insert path: new run (or "running" sentinel).
	finished := req.FinishedAt
	if finished.IsZero() && st != model.StatusRunning {
		finished = time.Now().UTC()
	}
	started := req.StartedAt
	if started.IsZero() {
		started = time.Now().UTC()
	}
	id, err := s.st.RecordRun(ctx, model.Run{
		ClientID: cl.ID, StartedAt: started, FinishedAt: finished, Status: st,
		Bytes: req.Bytes, Files: req.Files, SnapshotID: req.SnapshotID, LogTail: req.LogTail,
	})
	if err != nil {
		http.Error(w, "failed to record status", http.StatusInternalServerError)
		return
	}
	if s.verbose {
		log.Printf("status: client=%q run=%d status=%s files=%d bytes=%d", cl.Name, id, st, req.Files, req.Bytes)
	}
	go s.notifyStatus(cl, st, req.Files, req.Bytes, started)
	writeJSON(w, http.StatusCreated, map[string]any{"run_id": id})
}

// notifyStatus sends a Discord message for a status change. Failures always
// fire; all other statuses only fire when verbose mode is enabled.
// Call as a goroutine — never blocks the response path.
func (s *Server) notifyStatus(cl *model.Client, st model.RunStatus, files, bytes int64, ts time.Time) {
	fmtTime := func(t time.Time) string {
		return t.In(s.loc).Format("2006-01-02 15:04:05 MST")
	}
	switch st {
	case model.StatusFailed:
		alert.Discord(s.discordWebhook, fmt.Sprintf(
			"⚠️ **backitup** — `%s` backup **FAILED**\nSource: %s\nAt: %s",
			cl.Name, cl.SourceLabel, fmtTime(ts),
		))
	case model.StatusOK:
		if s.verbose {
			alert.Discord(s.discordWebhook, fmt.Sprintf(
				"✅ **backitup** — `%s` backup **OK**\nSource: %s\nFinished: %s | files=%d size=%s",
				cl.Name, cl.SourceLabel, fmtTime(ts), files, alert.FormatBytes(bytes),
			))
		}
	case model.StatusRunning:
		if s.verbose {
			alert.Discord(s.discordWebhook, fmt.Sprintf(
				"▶️ **backitup** — `%s` backup **STARTED**\nSource: %s\nStarted: %s",
				cl.Name, cl.SourceLabel, fmtTime(ts),
			))
		}
	case model.StatusOverlap:
		if s.verbose {
			alert.Discord(s.discordWebhook, fmt.Sprintf(
				"⏭️ **backitup** — `%s` backup **SKIPPED** (overlap — previous run still in progress)\nSource: %s",
				cl.Name, cl.SourceLabel,
			))
		}
	}
}


func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func unauthorized(w http.ResponseWriter) {
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}
