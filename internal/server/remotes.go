package server

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/th0rn0/backitup/internal/atomicfile"
	"github.com/th0rn0/backitup/internal/model"
)

// writeRcloneConfig regenerates the sections of the rclone config file that
// backitup owns. Called after every create/edit/delete and once on startup.
//
// It must not rewrite the file wholesale: sections an operator added by hand
// (an rclone [crypt] section, a provider backitup has no fields for) are
// preserved verbatim. See rcloneconf.go for who owns what.
func (s *Server) writeRcloneConfig(ctx context.Context) error {
	if s.rcloneConfig == "" {
		return nil
	}
	remotes, err := s.st.ListRemotes(ctx)
	if err != nil {
		return err
	}
	sections := make([]string, 0, len(remotes))
	for _, r := range remotes {
		sections = append(sections, r.RcloneSection())
	}
	existing, err := readFileIfExists(s.rcloneConfig)
	if err != nil {
		// Without the current contents there is no way to preserve what
		// backitup does not own, and overwriting would be exactly the data loss
		// this file is careful about. Report it instead.
		return err
	}
	return atomicfile.Write(s.rcloneConfig, []byte(mergeRcloneConfig(existing, sections)), 0o600)
}

// readFileIfExists returns the file's contents, or "" if it does not exist.
// Any other error (a permission problem, a directory in the way) is returned:
// the caller must not overwrite a file it could not read.
func readFileIfExists(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", nil
		}
		return "", fmt.Errorf("read %s: %w", path, err)
	}
	return string(b), nil
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

	// A section backitup did not create is not backitup's to replace. The
	// writer owns sections by NAME, so a new row with this name would rewrite
	// that section on the next write — for a hand-written [crypt] that means
	// its key is gone and everything it already wrote becomes unreadable. The
	// name is also referenced by every client, cron job and script that uses
	// it, so quietly renaming the operator's section would break those just as
	// quietly. Refuse, and let the operator choose a name.
	owned, err := s.st.GetRemoteByName(ctx, name)
	if err != nil {
		log.Printf("create remote %q: look up for the config ownership check: %v", name, err)
		http.Redirect(w, r, "/settings/remotes?err="+url.QueryEscape("Could not save remote: "+err.Error()), http.StatusSeeOther)
		return
	}
	if owned == nil {
		handWritten, hErr := s.rcloneSectionExists(name)
		if hErr != nil {
			log.Printf("create remote %q: read rclone config for the ownership check: %v", name, hErr)
			http.Redirect(w, r, "/settings/remotes?err="+url.QueryEscape("Could not save remote: "+hErr.Error()), http.StatusSeeOther)
			return
		}
		if handWritten {
			http.Redirect(w, r, "/settings/remotes?err="+url.QueryEscape(
				"rclone.conf already has a section named \""+name+"\" that backitup did not create — rename or remove it there first, backitup would otherwise overwrite it"),
				http.StatusSeeOther)
			return
		}
	}

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

	if msg, vErr := s.validateRemoteConfig(ctx, name, def, cfg); vErr != nil {
		log.Printf("create remote %q: validate: %v", name, vErr)
		http.Redirect(w, r, "/settings/remotes?err="+url.QueryEscape("Could not save remote: "+vErr.Error()), http.StatusSeeOther)
		return
	} else if msg != "" {
		http.Redirect(w, r, "/settings/remotes?err="+url.QueryEscape(msg), http.StatusSeeOther)
		return
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

// getEditRemote renders the edit form for an existing remote.
func (s *Server) getEditRemote(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validRemoteName(name) {
		http.NotFound(w, r)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	remote, err := s.st.GetRemoteByName(ctx, name)
	if err != nil || remote == nil {
		http.NotFound(w, r)
		return
	}
	def := model.FindBackend(remote.Backend)
	if def == nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = s.tmpl.ExecuteTemplate(w, "remote_edit.html", map[string]any{
		"Username":   usernameFromContext(r.Context()),
		"ActivePage": "remotes",
		"Remote":     remote,
		"Def":        def,
		"Flash":      r.URL.Query().Get("msg"),
		"Error":      r.URL.Query().Get("err"),
	})
}

// postEditRemote updates the config of an existing remote.
func (s *Server) postEditRemote(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	if !validRemoteName(name) {
		http.NotFound(w, r)
		return
	}
	if err := r.ParseForm(); err != nil {
		http.Redirect(w, r, "/settings/remotes/"+name+"/edit?err=invalid+form", http.StatusSeeOther)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	existing, err := s.st.GetRemoteByName(ctx, name)
	if err != nil || existing == nil {
		http.NotFound(w, r)
		return
	}
	def := model.FindBackend(existing.Backend)
	if def == nil {
		http.NotFound(w, r)
		return
	}

	cfg := map[string]string{}
	for _, f := range def.Fields {
		val := strings.TrimSpace(r.PostFormValue(f.Key))
		if val == "" {
			// Keep existing value (critical for obscured password fields).
			if v, ok := existing.Config[f.Key]; ok {
				cfg[f.Key] = v
			}
			continue
		}
		if f.Obscure {
			obscured, oErr := rcloneObscure(ctx, s.rcloneConfig, val)
			if oErr != nil {
				log.Printf("rclone obscure for %s/%s: %v", name, f.Key, oErr)
				http.Redirect(w, r, "/settings/remotes/"+name+"/edit?err="+url.QueryEscape("Could not obscure password: "+oErr.Error()), http.StatusSeeOther)
				return
			}
			val = obscured
		}
		cfg[f.Key] = val
	}

	if msg, vErr := s.validateRemoteConfig(ctx, name, def, cfg); vErr != nil {
		log.Printf("update remote %q: validate: %v", name, vErr)
		http.Redirect(w, r, "/settings/remotes/"+name+"/edit?err="+url.QueryEscape("Could not save remote: "+vErr.Error()), http.StatusSeeOther)
		return
	} else if msg != "" {
		http.Redirect(w, r, "/settings/remotes/"+name+"/edit?err="+url.QueryEscape(msg), http.StatusSeeOther)
		return
	}

	if err := s.st.UpdateRemote(ctx, name, cfg); err != nil {
		log.Printf("update remote %q: %v", name, err)
		http.Redirect(w, r, "/settings/remotes/"+name+"/edit?err="+url.QueryEscape("Could not save remote: "+err.Error()), http.StatusSeeOther)
		return
	}

	if err := s.writeRcloneConfig(ctx); err != nil {
		log.Printf("write rclone config after edit %q: %v", name, err)
	}

	http.Redirect(w, r, "/settings/remotes?msg="+url.QueryEscape("Remote \""+name+"\" updated"), http.StatusSeeOther)
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

	// A crypt remote is a section that points at this one; deleting the parent
	// would leave a config rclone cannot use, and the encrypted remote's own
	// data would be unreachable even if it is recreated. Refuse, and say what
	// to delete first.
	remotes, err := s.st.ListRemotes(ctx)
	if err != nil {
		// Not a reason to refuse the operator's explicit delete: a store blip
		// must not block it. The rewrite below reports the same failure.
		log.Printf("delete remote %q: list remotes for the encryption guard: %v", name, err)
	}
	for _, other := range remotes {
		if other.CryptParent() == name {
			http.Redirect(w, r, "/settings/remotes?err="+url.QueryEscape(
				"Remote \""+name+"\" is encrypted by remote \""+other.Name+"\" — delete that one first"),
				http.StatusSeeOther)
			return
		}
	}

	// The loop above only sees rows, and rclone.conf can hold sections the
	// table does not know: an operator's hand-written [crypt] with
	// `remote = <name>:` does not become unresolvable when this remote is
	// deleted — their offsite tier just stops working. Sections backitup does
	// own are the rows the loop above already handled, so those are skipped
	// here: a stale copy of a managed section in the file must not refuse a
	// delete that is actually allowed.
	if s.rcloneConfig != "" {
		managed := make(map[string]bool, len(remotes))
		for _, r := range remotes {
			managed[r.Name] = true
		}
		existing, rErr := readFileIfExists(s.rcloneConfig)
		if rErr != nil {
			// As above: a config blip is not a reason to refuse an explicit
			// delete. The rewrite after it reports the same failure.
			log.Printf("delete remote %q: read rclone config for the encryption guard: %v", name, rErr)
		} else {
			for _, ref := range sectionsReferencing(existing, name) {
				if managed[ref] {
					continue
				}
				http.Redirect(w, r, "/settings/remotes?err="+url.QueryEscape(
					"Remote \""+name+"\" is encrypted by remote \""+ref+"\" — delete that one first"),
					http.StatusSeeOther)
				return
			}
		}
	}

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

// validateRemoteConfig applies the checks that the field list alone cannot
// make: values that must come from a fixed vocabulary, and a crypt remote's
// reference to the plaintext remote it wraps. It returns an operator-facing
// message when the config is refused, or an error when the check itself could
// not be made (a store failure — not the operator's fault, so it is not
// reported as a bad value).
//
// cfg is normalised in place: the crypt parent is stored as the bare remote
// name, since the trailing colon is rclone.conf syntax, not data.
func (s *Server) validateRemoteConfig(ctx context.Context, name string, def *model.BackendDef, cfg map[string]string) (string, error) {
	for _, f := range def.Fields {
		v, ok := cfg[f.Key]
		if !ok || v == "" {
			continue
		}
		if !f.AllowsValue(v) {
			return f.Label + " is not one of the allowed choices", nil
		}
	}
	if def.ID != model.BackendCrypt {
		return "", nil
	}

	parent := strings.TrimSuffix(strings.TrimSpace(cfg["remote"]), ":")
	if parent == "" {
		return "A plaintext remote to encrypt is required", nil
	}
	if !validRemoteName(parent) {
		return "Plaintext remote may only contain letters, digits, hyphens, underscores, and dots", nil
	}
	if parent == name {
		return fmt.Sprintf("Remote %q cannot encrypt itself — name the plaintext remote it wraps", name), nil
	}
	cfg["remote"] = parent

	p, err := s.st.GetRemoteByName(ctx, parent)
	if err != nil {
		return "", err
	}
	if p == nil {
		// An operator's own hand-written section is a legitimate parent — the
		// whole point of preserving those sections is that they keep working.
		handWritten, err := s.rcloneSectionExists(parent)
		if err != nil {
			return "", err
		}
		if !handWritten {
			return fmt.Sprintf("Remote %q does not exist yet — create the plaintext remote first, then the encrypted one", parent), nil
		}
		return "", nil
	}
	if p.IsCrypt() {
		return fmt.Sprintf("Remote %q is itself encrypted — an encrypted remote wraps a plaintext remote", parent), nil
	}
	return "", nil
}

// rcloneSectionExists reports whether the rclone config file already defines a
// section called name — a remote the operator configured by hand rather than in
// the UI, which therefore has no row in the database.
func (s *Server) rcloneSectionExists(name string) (bool, error) {
	if s.rcloneConfig == "" {
		return false, nil
	}
	existing, err := readFileIfExists(s.rcloneConfig)
	if err != nil {
		return false, err
	}
	for _, line := range strings.Split(existing, "\n") {
		if n, ok := sectionName(line); ok && n == name {
			return true, nil
		}
	}
	return false, nil
}
