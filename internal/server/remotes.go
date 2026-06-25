package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"os/exec"
	"sort"
	"strings"
	"time"
)

type rcloneRemote struct {
	Name string
	Type string
}

// listRcloneRemotes returns all remotes in the rclone config, sorted by name.
func (s *Server) listRcloneRemotes(ctx context.Context) ([]rcloneRemote, error) {
	if s.rcloneConfig == "" {
		return nil, nil
	}
	out, err := exec.CommandContext(ctx, "rclone", "--config", s.rcloneConfig, "config", "dump").Output()
	if err != nil {
		return nil, fmt.Errorf("rclone config dump: %w", err)
	}
	if len(strings.TrimSpace(string(out))) == 0 || strings.TrimSpace(string(out)) == "null" {
		return nil, nil
	}
	var dump map[string]map[string]string
	if err := json.Unmarshal(out, &dump); err != nil {
		return nil, fmt.Errorf("parse rclone config: %w", err)
	}
	remotes := make([]rcloneRemote, 0, len(dump))
	for name, cfg := range dump {
		remotes = append(remotes, rcloneRemote{Name: name, Type: cfg["type"]})
	}
	sort.Slice(remotes, func(i, j int) bool { return remotes[i].Name < remotes[j].Name })
	return remotes, nil
}

// getRemotes renders the remote storage management page.
func (s *Server) getRemotes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	remotes, listErr := s.listRcloneRemotes(ctx)
	errMsg := r.URL.Query().Get("err")
	flash := r.URL.Query().Get("msg")
	if listErr != nil && errMsg == "" {
		errMsg = "Could not list remotes: " + listErr.Error()
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.ExecuteTemplate(w, "remotes.html", map[string]any{
		"Username": usernameFromContext(r.Context()),
		"Remotes":  remotes,
		"Flash":    flash,
		"Error":    errMsg,
		"NoRclone": s.rcloneConfig == "",
	})
}

// postCreateS3Remote creates an S3-compatible rclone remote non-interactively.
func (s *Server) postCreateS3Remote(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/settings/remotes?err=invalid+form", http.StatusSeeOther)
		return
	}

	name := strings.TrimSpace(r.PostFormValue("name"))
	accessKeyID := strings.TrimSpace(r.PostFormValue("access_key_id"))
	secretKey := strings.TrimSpace(r.PostFormValue("secret_access_key"))
	region := strings.TrimSpace(r.PostFormValue("region"))
	endpoint := strings.TrimSpace(r.PostFormValue("endpoint"))

	if name == "" || accessKeyID == "" || secretKey == "" {
		http.Redirect(w, r, "/settings/remotes?err=Name%2C+access+key%2C+and+secret+are+required", http.StatusSeeOther)
		return
	}
	if !validRemoteName(name) {
		http.Redirect(w, r, "/settings/remotes?err=Remote+name+may+only+contain+letters%2C+digits%2C+hyphens%2C+underscores%2C+and+dots", http.StatusSeeOther)
		return
	}

	// Use provider=Other when a custom endpoint is given so rclone doesn't
	// apply AWS-specific defaults that would conflict with other providers.
	provider := "AWS"
	if endpoint != "" {
		provider = "Other"
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	args := []string{"--config", s.rcloneConfig, "config", "create", name, "s3",
		"provider=" + provider,
		"access_key_id=" + accessKeyID,
		"secret_access_key=" + secretKey,
	}
	if region != "" {
		args = append(args, "region="+region)
	}
	if endpoint != "" {
		args = append(args, "endpoint="+endpoint)
	}

	if out, err := exec.CommandContext(ctx, "rclone", args...).CombinedOutput(); err != nil {
		log.Printf("rclone config create s3 %q: %v: %s", name, err, out)
		http.Redirect(w, r, "/settings/remotes?err="+url.QueryEscape("rclone error: "+strings.TrimSpace(string(out))), http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/settings/remotes?msg=Remote+%22"+url.QueryEscape(name)+"%22+created", http.StatusSeeOther)
}

// postCreateGDriveRemote creates a Google Drive rclone remote using a service
// account credentials JSON. Service accounts need no OAuth browser flow and no
// token refresh — rclone handles JWT auth internally.
func (s *Server) postCreateGDriveRemote(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/settings/remotes?err=invalid+form", http.StatusSeeOther)
		return
	}

	name := strings.TrimSpace(r.PostFormValue("name"))
	credJSON := strings.TrimSpace(r.PostFormValue("service_account_credentials"))
	teamDriveID := strings.TrimSpace(r.PostFormValue("team_drive_id"))

	if name == "" || credJSON == "" {
		http.Redirect(w, r, "/settings/remotes?err=Name+and+service+account+JSON+are+required", http.StatusSeeOther)
		return
	}
	if !validRemoteName(name) {
		http.Redirect(w, r, "/settings/remotes?err=Remote+name+may+only+contain+letters%2C+digits%2C+hyphens%2C+underscores%2C+and+dots", http.StatusSeeOther)
		return
	}

	// Validate and compact the JSON so rclone receives a clean single-line value.
	var raw json.RawMessage
	if err := json.Unmarshal([]byte(credJSON), &raw); err != nil {
		http.Redirect(w, r, "/settings/remotes?err="+url.QueryEscape("Invalid service account JSON: "+err.Error()), http.StatusSeeOther)
		return
	}
	compact, _ := json.Marshal(raw)

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	args := []string{"--config", s.rcloneConfig, "config", "create", name, "drive",
		"scope=drive",
		"service_account_credentials=" + string(compact),
	}
	if teamDriveID != "" {
		args = append(args, "team_drive="+teamDriveID)
	}

	if out, err := exec.CommandContext(ctx, "rclone", args...).CombinedOutput(); err != nil {
		log.Printf("rclone config create drive %q: %v: %s", name, err, out)
		http.Redirect(w, r, "/settings/remotes?err="+url.QueryEscape("rclone error: "+strings.TrimSpace(string(out))), http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/settings/remotes?msg=Google+Drive+remote+%22"+url.QueryEscape(name)+"%22+created", http.StatusSeeOther)
}

// postDeleteRemote removes a remote from the rclone config by name.
func (s *Server) postDeleteRemote(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validRemoteName(name) {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if out, err := exec.CommandContext(ctx, "rclone", "--config", s.rcloneConfig, "config", "delete", name).CombinedOutput(); err != nil {
		log.Printf("rclone config delete %q: %v: %s", name, err, out)
		http.Redirect(w, r, "/settings/remotes?err="+url.QueryEscape("delete failed: "+strings.TrimSpace(string(out))), http.StatusSeeOther)
		return
	}

	http.Redirect(w, r, "/settings/remotes?msg=Remote+%22"+url.QueryEscape(name)+"%22+deleted", http.StatusSeeOther)
}

// validRemoteName accepts the character set rclone allows for remote names.
func validRemoteName(name string) bool {
	if name == "" {
		return false
	}
	for _, c := range name {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') &&
			(c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			return false
		}
	}
	return true
}
