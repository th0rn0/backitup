package server

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"time"

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
// presented token. O(n) argon2 verifications per request; fine at homelab fleet
// size. TODO(scale): add a fast non-secret token id to narrow the lookup.
func (s *Server) clientByToken(ctx context.Context, token string) (*model.Client, error) {
	clients, err := s.st.ListClients(ctx)
	if err != nil {
		return nil, err
	}
	for i := range clients {
		c := clients[i]
		if !c.Enabled || c.TokenHash == "" {
			continue
		}
		if ok, err := auth.VerifyPassword(token, c.TokenHash); err == nil && ok {
			return &c, nil
		}
	}
	return nil, nil
}

// getConfig returns the calling client's behaviour (D1: server owns WHAT).
func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	cl := clientFrom(r.Context())
	excludes := cl.Excludes
	if excludes == nil {
		excludes = []string{}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"mode":           cl.Mode,
		"excludes":       excludes,
		"retention_days": cl.RetentionDays,
		"skip_symlinks":  cl.SkipSymlinks,
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
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10))
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
	writeJSON(w, http.StatusCreated, map[string]any{"run_id": id})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func unauthorized(w http.ResponseWriter) {
	http.Error(w, "unauthorized", http.StatusUnauthorized)
}
