package server

import (
	"context"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

// writeRcloneConfig regenerates the rclone config file from all remotes stored
// in the database. Called after every create/delete and once on server startup.
func (s *Server) writeRcloneConfig(ctx context.Context) error {
	if s.rcloneConfig == "" {
		return nil
	}
	remotes, err := s.st.ListRemotes(ctx)
	if err != nil {
		return err
	}
	var sb strings.Builder
	for _, r := range remotes {
		sb.WriteString(r.RcloneSection())
		sb.WriteString("\n")
	}
	return os.WriteFile(s.rcloneConfig, []byte(sb.String()), 0600)
}

// RegenerateRcloneConfig is the exported entry point called from main after
// all server options are wired up.
func (s *Server) RegenerateRcloneConfig(ctx context.Context) error {
	return s.writeRcloneConfig(ctx)
}

// getRemotes renders the remote storage management page.
func (s *Server) getRemotes(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	remotes, err := s.st.ListRemotes(ctx)
	errMsg := r.URL.Query().Get("err")
	flash := r.URL.Query().Get("msg")
	if err != nil && errMsg == "" {
		errMsg = "Could not list remotes: " + err.Error()
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.ExecuteTemplate(w, "remotes.html", map[string]any{
		"Username":   usernameFromContext(r.Context()),
		"ActivePage": "remotes",
		"Remotes":    remotes,
		"Backends":   model.Backends,
		"Flash":      flash,
		"Error":      errMsg,
		"NoRclone":   s.rcloneConfig == "",
	})
}

// postCreateRemote creates a new remote, obscuring passwords where rclone
// requires it, then regenerates rclone.conf.
func (s *Server) postCreateRemote(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/settings/remotes?err=invalid+form", http.StatusSeeOther)
		return
	}

	name := strings.TrimSpace(r.PostFormValue("name"))
	backend := model.RemoteBackend(r.PostFormValue("backend"))

	if name == "" {
		http.Redirect(w, r, "/settings/remotes?err=Remote+name+is+required", http.StatusSeeOther)
		return
	}
	if !validRemoteName(name) {
		http.Redirect(w, r, "/settings/remotes?err=Remote+name+may+only+contain+letters%2C+digits%2C+hyphens%2C+underscores%2C+and+dots", http.StatusSeeOther)
		return
	}

	def := model.FindBackend(backend)
	if def == nil {
		http.Redirect(w, r, "/settings/remotes?err=Unknown+backend", http.StatusSeeOther)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	cfg := map[string]string{}
	for _, f := range def.Fields {
		val := strings.TrimSpace(r.PostFormValue(f.Key))
		if val == "" {
			if f.Required {
				http.Redirect(w, r, "/settings/remotes?err="+url.QueryEscape(f.Label+" is required"), http.StatusSeeOther)
				return
			}
			continue
		}
		if f.Obscure {
			obscured, err := rcloneObscure(ctx, s.rcloneConfig, val)
			if err != nil {
				log.Printf("rclone obscure for %s/%s: %v", name, f.Key, err)
				http.Redirect(w, r, "/settings/remotes?err="+url.QueryEscape("Could not obscure password: "+err.Error()), http.StatusSeeOther)
				return
			}
			val = obscured
		}
		cfg[f.Key] = val
	}

	remote := model.Remote{
		Name:      name,
		Backend:   backend,
		Config:    cfg,
		CreatedAt: time.Now(),
	}
	if err := s.st.CreateRemote(ctx, remote); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			http.Redirect(w, r, "/settings/remotes?err="+url.QueryEscape("A remote named \""+name+"\" already exists"), http.StatusSeeOther)
			return
		}
		log.Printf("create remote %q: %v", name, err)
		http.Redirect(w, r, "/settings/remotes?err="+url.QueryEscape("Could not save remote: "+err.Error()), http.StatusSeeOther)
		return
	}

	if err := s.writeRcloneConfig(ctx); err != nil {
		log.Printf("write rclone config after create %q: %v", name, err)
	}

	http.Redirect(w, r, "/settings/remotes?msg="+url.QueryEscape("Remote \""+name+"\" created"), http.StatusSeeOther)
}

// postTestRemote verifies a remote is reachable with rclone lsd.
func (s *Server) postTestRemote(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validRemoteName(name) {
		http.NotFound(w, r)
		return
	}
	if s.rcloneConfig == "" {
		http.Redirect(w, r, "/settings/remotes?err=rclone+not+configured", http.StatusSeeOther)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	out, err := exec.CommandContext(ctx, "rclone", "--config", s.rcloneConfig, "lsd", name+":").CombinedOutput()
	if err != nil {
		log.Printf("rclone test %q: %v: %s", name, err, out)
		http.Redirect(w, r, "/settings/remotes?err="+url.QueryEscape("Connection test failed for \""+name+"\" — check server logs for details"), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/settings/remotes?msg="+url.QueryEscape("\""+name+"\" connected successfully"), http.StatusSeeOther)
}

// postDeleteRemote removes a remote from the database and regenerates rclone.conf.
func (s *Server) postDeleteRemote(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validRemoteName(name) {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	if err := s.st.DeleteRemote(ctx, name); err != nil {
		log.Printf("delete remote %q: %v", name, err)
		http.Redirect(w, r, "/settings/remotes?err="+url.QueryEscape("Delete failed: "+err.Error()), http.StatusSeeOther)
		return
	}

	if err := s.writeRcloneConfig(ctx); err != nil {
		log.Printf("write rclone config after delete %q: %v", name, err)
	}

	http.Redirect(w, r, "/settings/remotes?msg=Remote+%22"+url.QueryEscape(name)+"%22+deleted", http.StatusSeeOther)
}

// rcloneObscure runs `rclone obscure -` (reading from stdin) and returns the
// obscured value. Piping via stdin keeps the plaintext password out of
// /proc/<pid>/cmdline and ps output.
func rcloneObscure(ctx context.Context, cfgPath, pass string) (string, error) {
	args := []string{"obscure", "-"}
	if cfgPath != "" {
		args = append([]string{"--config", cfgPath}, args...)
	}
	cmd := exec.CommandContext(ctx, "rclone", args...)
	cmd.Stdin = strings.NewReader(pass)
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
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
