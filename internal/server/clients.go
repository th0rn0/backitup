package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/th0rn0/backitup/internal/auth"
	"github.com/th0rn0/backitup/internal/authkeys"
	"github.com/th0rn0/backitup/internal/keys"
	"github.com/th0rn0/backitup/internal/mode"
	"github.com/th0rn0/backitup/internal/model"
	"github.com/th0rn0/backitup/internal/store"
)

// getNewClient renders the add-client form.
func (s *Server) getNewClient(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()
	remotes, _ := s.st.ListRemotes(ctx)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.render(w, "clients_new.html", map[string]any{
		"Username":    usernameFromContext(r.Context()),
		"ActivePage":  "clients/new",
		"Remotes":     remotes,
		"PublicHost":  s.publicHost,
		"PublicAPI":   s.publicAPI,
		"ClientImage": s.clientImage,
		// The preview must not advertise a known_hosts mount the created page
		// cannot hand over, so it follows the same rule the created page does.
		"HasHostKey": knownHostsLine(s.publicHost, s.sshHostKeyPath) != "",
	})
}

// postClients creates a client: generate an SSH keypair + bearer token, store
// the client (pubkey + token HASH only), regenerate authorized_keys, and show
// the private key + token + cron line ONCE (D4, DD5).
func (s *Server) postClients(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		// A form that will not parse is worth a log line of its own: it is the
		// one path here that really is about the request, so when an operator
		// reports "invalid form" the log must be able to confirm it.
		log.Printf("create client: parse form: %v", err)
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	name := r.PostFormValue("name")
	mode := model.Mode(r.PostFormValue("mode"))
	if name == "" || !mode.Valid() {
		s.renderNewClientError(w, r, "Name is required and mode must be targz or rsync.")
		return
	}
	// The name becomes the client's storage directory (via its slug) and part of
	// its sshd forced command. A name longer than a filesystem component can
	// never get a directory, and every authorized_keys rewrite would then fail —
	// so it is refused here, at the edge, rather than breaking SSH for the fleet.
	if len(name) > model.MaxSlugLen {
		s.renderNewClientError(w, r, fmt.Sprintf(
			"Client name must be at most %d characters (this one is %d). It becomes the client's "+
				"storage directory and part of its SSH forced command, so it has to stay short.",
			model.MaxSlugLen, len(name)))
		return
	}

	privPEM, pubLine, token, tokenHash, tokenPrefix, ok := generateClientCreds(w, name)
	if !ok {
		return
	}

	ctx, cancel := s.storeCtx(r)
	defer cancel()
	_, err := s.st.CreateClient(ctx, model.Client{
		Name:                 name,
		Mode:                 mode,
		SourceLabel:          r.PostFormValue("source_label"),
		RetentionDays:        atoiDefault(r.PostFormValue("retention_days"), 14),
		ExpectedIntervalSecs: atoiDefault(r.PostFormValue("expected_interval_secs"), 0),
		OffsiteRemote:        r.PostFormValue("offsite_remote"),
		OffsiteDir:           strings.TrimSpace(strings.Trim(r.PostFormValue("offsite_dir"), "/")),
		OffsiteCron:          strings.TrimSpace(r.PostFormValue("offsite_cron")),
		SkipSymlinks:         r.PostFormValue("skip_symlinks") == "1",
		PreserveSparse:       r.PostFormValue("preserve_sparse") == "1",
		PreserveHardlinks:    r.PostFormValue("preserve_hardlinks") == "1",
		PreserveACLs:         r.PostFormValue("preserve_acls") == "1",
		PreserveXattrs:       r.PostFormValue("preserve_xattrs") == "1",
		SSHPubKey:            pubLine,
		TokenHash:            tokenHash,
		TokenPrefix:          tokenPrefix,
		Enabled:              true,
	})
	if err != nil {
		// A name that maps onto an existing slug, or onto none at all, must be
		// refused: the slug IS the client's read-write backup directory.
		var conflict *store.SlugConflictError
		switch {
		case errors.As(err, &conflict):
			if conflict.Existing == name {
				s.renderNewClientError(w, r, fmt.Sprintf(
					"A client named %q already exists — the name is already taken.", name))
				return
			}
			s.renderNewClientError(w, r, fmt.Sprintf(
				"Storage slug %q is already used by client %q. Two names that differ only in "+
					"punctuation, spacing or case share one directory, so choose a name that "+
					"produces a different slug.", conflict.Slug, conflict.Existing))
			return
		case errors.Is(err, store.ErrEmptySlug):
			s.renderNewClientError(w, r, fmt.Sprintf(
				"Client name %q must contain at least one letter or digit: it becomes the "+
					"client's storage slug (directory name).", name))
			return
		}
		// Nothing about the name: either the store did not answer (503, and the
		// error is in the log) or it failed for a reason this handler cannot
		// attribute to the input. Before storeFailure existed every one of these
		// fell into the message below, so a spent deadline told the operator
		// their client name was probably taken — the 400 in build 165.
		if s.storeFailure(w, ctx, fmt.Sprintf("create client %q", name), err) {
			return
		}
		s.renderNewClientError(w, r, "Could not create client (is the name already taken?).")
		return
	}

	// Fresh context for the authorized_keys rewrite: the store write above may
	// have spent most of dbTimeout, and authorized_keys is what makes the client
	// we just created able to connect at all. Bounded by the same deadline so a
	// wedged filesystem cannot hang the request either.
	akCtx, akCancel := context.WithTimeout(context.Background(), s.dbTimeout)
	defer akCancel()
	if err := s.regenAuthorizedKeys(akCtx); err != nil {
		log.Printf("authkeys regenerate failed: %v — the new client's key is NOT in authorized_keys, so it cannot connect yet", err)
		// The client exists; surface the issue but still show the secrets.
	}
	// A name re-used after a delete inherits that client's directory, and with it
	// whatever mode the build that first made it used. Heal both directories now
	// rather than at some later pass, so the operator's group can read this client
	// from its first run.
	s.ReconcileClientDirModes(akCtx)

	apiBase := s.apiBase(r.PostFormValue("api_scheme"))
	// The page names ONE secrets directory (step 1) and the command mounts that
	// same path, so the operator never has to work out that a placeholder and a
	// real path are the same thing. The source mount is named for what it is:
	// the directory being backed up, read-only.
	secrets := secretsDir(name, r.PostFormValue("secrets_path"))
	source, sourcePrefilled := sourceMount(r.PostFormValue("backup_path"), r.PostFormValue("source_label"))
	kh := knownHostsLine(s.publicHost, s.sshHostKeyPath)
	dockerKnown, dockerInsecure := s.dockerCmds(token, apiBase, source, secrets, kh != "")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.render(w, "client_created.html", map[string]any{
		"Username":          usernameFromContext(r.Context()),
		"Name":              name,
		"Mode":              string(mode),
		"PrivateKey":        privPEM,
		"Token":             token,
		"Server":            s.publicHost,
		"APIBase":           apiBase,
		"SecretsDir":        secrets,
		"SourcePath":        source,
		"SourcePrefilled":   sourcePrefilled,
		"APIHint":           s.apiPlaceholderHint(),
		"KnownHostsLine":    kh,
		"DockerRunKnown":    dockerKnown,
		"DockerRunInsecure": dockerInsecure,
		"CronKnown":         cronEntry(defaultCronSchedule, dockerKnown),
		"CronInsecure":      cronEntry(defaultCronSchedule, dockerInsecure),
	})
}

// getClient renders the client detail page with run history.
func (s *Server) getClient(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.storeCtx(r)
	defer cancel()
	c, err := s.st.GetClientBySlug(ctx, r.PathValue("name"))
	if err != nil {
		if s.storeFailure(w, ctx, fmt.Sprintf("client detail: load client %q", r.PathValue("name")), err) {
			return
		}
		http.Error(w, "failed to load client", http.StatusInternalServerError)
		return
	}
	if c == nil {
		http.NotFound(w, r)
		return
	}
	runs, _ := s.st.ListRuns(ctx, c.ID, 20)
	offsiteRuns, _ := s.st.ListOffsiteRuns(ctx, c.ID, 20)
	allObjects, _ := s.st.ListOffsiteObjects(ctx, c.ID)
	// Dedup records are not offsite objects: they name no remote path of their
	// own, are never verified, and Download/Delete would address a file that was
	// never uploaded (rclone always errors). Keep them out of the table, the
	// missing-local list and the object count/size.
	offsiteObjects := make([]model.OffsiteObject, 0, len(allObjects))
	for _, o := range allObjects {
		if o.DedupOf != "" {
			continue
		}
		offsiteObjects = append(offsiteObjects, o)
	}
	allRemotes, _ := s.st.ListRemotes(ctx)
	var latest *model.Run
	if len(runs) > 0 {
		latest = &runs[0]
	}
	// Health ages from the last SUCCESSFUL run, not the newest row (S8).
	lastOK, err := s.st.LastSuccessfulRun(ctx, c.ID)
	if err != nil {
		log.Printf("getClient: last successful run for client=%q: %v", c.Name, err)
	}
	h := model.DeriveHealth(latest, lastOK, time.Duration(c.ExpectedIntervalSecs)*time.Second, time.Now())

	// Load local snapshots from the hot store (best-effort — the directory may
	// not exist yet, and a client without a usable slug has none to list).
	var localSnapshots []mode.Snapshot
	var clientDir string
	if sm, ok := mode.Server(c.Mode); ok {
		dir, dirErr := s.clientDir(*c)
		if dirErr != nil {
			log.Printf("getClient: %v; not listing local snapshots", dirErr)
		} else {
			clientDir = dir
			snaps, err := s.hotSnapshots(ctx, c, sm, clientDir)
			if err == nil {
				localSnapshots = snaps
			}
		}
	}

	// Build set of snapshot IDs present on local disk for cross-referencing.
	localIDs := make(map[string]bool, len(localSnapshots))
	for _, s := range localSnapshots {
		localIDs[s.ID] = true
	}

	// Snapshots the app holds no hot copy of. Since the hot pruner deletes a
	// snapshot once a strictly newer one is offsite, an offsite record whose
	// snapshot directory is gone is the designed steady state of a working
	// retention policy — the bucket holds the copy — not evidence of loss. Only a
	// row whose last verification reported the object missing is in doubt, and
	// only that row keeps the alarm colour and the "lost" wording.
	type offsiteOnlyRow struct {
		ID               string
		UploadedAt       time.Time
		Bytes            int64
		RemoteMissing    bool
		RemoteVerifiedAt time.Time
		// RequireConfirm is true when the app believes the object is still in the
		// bucket, which is the case where forgetting removes the only index entry
		// of a live backup: the operator has to confirm it a second time, by name.
		// Only a verification that ran and reported the object gone lifts the
		// requirement — "never checked" is not evidence that a copy is gone.
		RequireConfirm bool
	}
	var offsiteOnlySnaps []offsiteOnlyRow
	for _, o := range offsiteObjects {
		if localIDs[o.SnapshotID] {
			continue
		}
		offsiteOnlySnaps = append(offsiteOnlySnaps, offsiteOnlyRow{
			ID:               o.SnapshotID,
			UploadedAt:       o.UploadedAt,
			Bytes:            o.Bytes,
			RemoteMissing:    o.RemoteMissing,
			RemoteVerifiedAt: o.RemoteVerifiedAt,
			RequireConfirm:   !o.RemoteMissing,
		})
	}

	// Wrap each offsite object with the local-existence flag (remote status is
	// populated by the lifecycle verification pass, not on page load).
	type offsiteObjectView struct {
		model.OffsiteObject
		LocalExists bool
	}
	offsiteViews := make([]offsiteObjectView, len(offsiteObjects))
	for i, o := range offsiteObjects {
		offsiteViews[i] = offsiteObjectView{OffsiteObject: o, LocalExists: localIDs[o.SnapshotID]}
	}

	// On-disk figure: the lifecycle's measurement of this client's hot store,
	// read from the store. It is never derived from Snapshot.Bytes (rsync
	// reports 0 by design — its hardlinked tree has no per-snapshot size) and it
	// is never computed here: walking a 375 GB tree in a request handler is how
	// the page that reports a backup problem becomes the page that times out.
	onDiskValue, onDiskNote := "—", ""
	if storage, stErr := s.st.GetClientStorage(ctx, c.ID); stErr != nil {
		log.Printf("getClient: stored disk usage for client=%q: %v", c.Name, stErr)
		onDiskValue, onDiskNote = "unavailable", "the stored measurement could not be read"
	} else {
		onDiskValue, onDiskNote = diskFigure(storage, len(localSnapshots), time.Now())
	}
	var offsiteTotalBytes int64
	for _, o := range offsiteObjects {
		offsiteTotalBytes += o.Bytes
	}
	var lastBackupAge, lastBackupSize string
	switch {
	case latest == nil:
		lastBackupAge, lastBackupSize = "never", "—"
	case latest.Status == model.StatusRunning:
		lastBackupAge, lastBackupSize = "in progress", "…"
	case latest.Status == model.StatusFailed:
		lastBackupAge = "FAILED " + relTime(time.Since(latest.FinishedAt))
		lastBackupSize = "—"
	default:
		lastBackupAge = relTime(time.Since(latest.FinishedAt))
		lastBackupSize = humanBytes(latest.Bytes)
	}
	var expectedIntervalStr, nextExpected string
	if c.ExpectedIntervalSecs > 0 {
		d2 := time.Duration(c.ExpectedIntervalSecs) * time.Second
		switch {
		case d2 < time.Hour:
			expectedIntervalStr = fmt.Sprintf("%dm", int(d2.Minutes()))
		case d2 < 24*time.Hour:
			expectedIntervalStr = fmt.Sprintf("%dh", int(d2.Hours()))
		default:
			expectedIntervalStr = fmt.Sprintf("%dd", int(d2.Hours()/24))
		}
		if latest != nil && !latest.FinishedAt.IsZero() {
			next := latest.FinishedAt.Add(d2)
			remaining := time.Until(next)
			if remaining < 0 {
				nextExpected = "overdue"
			} else {
				switch {
				case remaining < time.Hour:
					nextExpected = fmt.Sprintf("in %dm", int(remaining.Minutes()))
				case remaining < 24*time.Hour:
					nextExpected = fmt.Sprintf("in %dh", int(remaining.Hours()))
				default:
					nextExpected = fmt.Sprintf("in %dd", int(remaining.Hours()/24))
				}
			}
		}
	}

	// Compute whether any snapshot has a non-zero size (for conditional column rendering).
	hasSnapshotSizes := false
	for _, s := range localSnapshots {
		if s.Bytes > 0 {
			hasSnapshotSizes = true
			break
		}
	}
	for _, s := range offsiteOnlySnaps {
		if s.Bytes > 0 {
			hasSnapshotSizes = true
			break
		}
	}

	// Name prefix, for the Settings field: the stored text (empty when nobody
	// chose one), the number a blank field falls back to, the "no prefix"
	// checkbox state, and an example of the resulting object name built by the
	// SAME function the uploader uses.
	namePrefixFallback := s.namePrefixDefault
	if namePrefixFallback == "" {
		namePrefixFallback = c.Slug
	}
	exampleSnap := "20260101T000000Z"
	if len(localSnapshots) > 0 {
		exampleSnap = localSnapshots[0].ID
	}
	exampleName := model.OffsiteObjectName(*c, s.namePrefixDefault, exampleSnap)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.render(w, "client_detail.html", map[string]any{
		"Username":            usernameFromContext(r.Context()),
		"ActivePage":          "",
		"Client":              c,
		"ClientDir":           clientDir,
		"Health":              string(h),
		"HealthLabel":         healthLabel(h),
		"Icon":                healthIcon(h),
		"Runs":                runs,
		"OffsiteRuns":         offsiteRuns,
		"OffsiteObjects":      offsiteViews,
		"LocalSnapshots":      localSnapshots,
		"OffsiteOnlySnaps":    offsiteOnlySnaps,
		"Remotes":             allRemotes,
		"ActiveTab":           clientTab(r.URL.Query().Get("tab")),
		"Flash":               r.URL.Query().Get("msg"),
		"Error":               r.URL.Query().Get("err"),
		"LastBackupAge":       lastBackupAge,
		"LastBackupSize":      lastBackupSize,
		"LocalCount":          len(localSnapshots),
		"OnDiskValue":         onDiskValue,
		"OnDiskNote":          onDiskNote,
		"OffsiteCount":        len(offsiteViews),
		"OffsiteTotalBytes":   offsiteTotalBytes,
		"ExpectedIntervalStr": expectedIntervalStr,
		"NextExpected":        nextExpected,
		"HasSnapshotSizes":    hasSnapshotSizes,
		// The restore command for a snapshot that records ownership is a pull
		// from this server, so the page needs the ingest address to state it.
		"PublicHost": s.publicHost,

		// Backup name prefix: what the field holds, what blank means, and a
		// live example of the object name it produces. NamePrefixExampleDir is
		// the directory the example sits in, so the page can show the whole
		// object path without guessing the remote's root.
		"NamePrefixInput":         c.NamePrefix,
		"NamePrefixNone":          c.NamePrefixSet && c.NamePrefix == "",
		"NamePrefixFallback":      namePrefixFallback,
		"NamePrefixExample":       exampleName,
		"NamePrefixExampleDir":    offsiteDirForPage(*c),
		"NamePrefixExampleLegacy": model.OffsiteObjectLegacyName(*c, exampleSnap),
		"NamePrefixSingle":        c.OffsiteUploadMode == model.OffsiteUploadSingle,
	})
}

// offsiteDirForPage is the directory component a client's objects live in, as
// the page must show it: the configured subdirectory, else the slug. It is the
// same rule lifecycle.offsiteDir applies, kept here so a template never has to
// know it.
func offsiteDirForPage(c model.Client) string {
	if c.OffsiteDir != "" {
		return c.OffsiteDir
	}
	return c.Slug
}

// getRunLog renders the log output for a single run.
func (s *Server) getRunLog(w http.ResponseWriter, r *http.Request) {
	runID, err := strconv.ParseInt(r.PathValue("runID"), 10, 64)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	ctx, cancel := s.storeCtx(r)
	defer cancel()
	c, err := s.st.GetClientBySlug(ctx, r.PathValue("name"))
	if err != nil {
		// A store that did not answer is not a missing run page: 404 would tell
		// the operator the run does not exist.
		if s.storeFailure(w, ctx, fmt.Sprintf("run log: load client %q", r.PathValue("name")), err) {
			return
		}
		http.NotFound(w, r)
		return
	}
	if c == nil {
		http.NotFound(w, r)
		return
	}
	run, err := s.st.GetRun(ctx, runID)
	if err != nil {
		if s.storeFailure(w, ctx, fmt.Sprintf("run log: load run %d", runID), err) {
			return
		}
		http.Error(w, "failed to load run", http.StatusInternalServerError)
		return
	}
	if run == nil || run.ClientID != c.ID {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.render(w, "run_log.html", map[string]any{
		"Username":   usernameFromContext(r.Context()),
		"ActivePage": "",
		"Client":     c,
		"Run":        run,
	})
}

// postRotateClient reissues the SSH key + bearer token for an existing client.
// Run history and all other settings are preserved. The old credentials are
// invalidated atomically; the operator must redeploy the new cron line.
func (s *Server) postRotateClient(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if r.PostFormValue("confirm") != "1" {
		http.Error(w, "confirmation required", http.StatusBadRequest)
		return
	}

	ctx, cancel := s.storeCtx(r)
	defer cancel()

	c, err := s.st.GetClientBySlug(ctx, r.PathValue("name"))
	if err != nil {
		if s.storeFailure(w, ctx, fmt.Sprintf("rotate: load client %q", r.PathValue("name")), err) {
			return
		}
		http.Error(w, "failed to load client", http.StatusInternalServerError)
		return
	}
	if c == nil {
		http.NotFound(w, r)
		return
	}

	privPEM, pubLine, token, tokenHash, tokenPrefix, ok := generateClientCreds(w, c.Name)
	if !ok {
		return
	}

	if err := s.st.RotateClientCreds(ctx, c.ID, pubLine, tokenHash, tokenPrefix, c.Version); err != nil {
		if errors.Is(err, store.ErrConflict) {
			http.Error(w, "concurrent rotation detected — reload the page and try again", http.StatusConflict)
			return
		}
		if s.storeFailure(w, ctx, fmt.Sprintf("rotate client %d (%q)", c.ID, c.Name), err) {
			// A store that did not answer is not a rotation that failed: build
			// 163 saw this path answer 500 and leave no log line at all.
			return
		}
		http.Error(w, "rotate failed", http.StatusInternalServerError)
		return
	}

	// Use a fresh context for the authkeys write: it is a local filesystem
	// operation that must not be bounded by the already-partially-spent HTTP
	// request context (the DB write above may have consumed most of the 5s).
	akCtx, akCancel := context.WithTimeout(context.Background(), s.dbTimeout)
	defer akCancel()
	authKeysFailed := false
	if err := s.regenAuthorizedKeys(akCtx); err != nil {
		log.Printf("authkeys regenerate failed after rotate client %d: %v", c.ID, err)
		authKeysFailed = true
	}

	apiBase := s.apiBase(r.PostFormValue("api_scheme"))
	secrets := secretsDir(c.Name, r.PostFormValue("secrets_path"))
	source, sourcePrefilled := sourceMount(r.PostFormValue("backup_path"), c.SourceLabel)
	kh := knownHostsLine(s.publicHost, s.sshHostKeyPath)
	dockerKnown, dockerInsecure := s.dockerCmds(token, apiBase, source, secrets, kh != "")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	s.render(w, "client_created.html", map[string]any{
		"Username":          usernameFromContext(r.Context()),
		"Name":              c.Name,
		"Mode":              string(c.Mode),
		"PrivateKey":        privPEM,
		"Token":             token,
		"Server":            s.publicHost,
		"APIBase":           apiBase,
		"SecretsDir":        secrets,
		"SourcePath":        source,
		"SourcePrefilled":   sourcePrefilled,
		"APIHint":           s.apiPlaceholderHint(),
		"KnownHostsLine":    kh,
		"DockerRunKnown":    dockerKnown,
		"DockerRunInsecure": dockerInsecure,
		"CronKnown":         cronEntry(defaultCronSchedule, dockerKnown),
		"CronInsecure":      cronEntry(defaultCronSchedule, dockerInsecure),
		"Rotated":           true,
		"AuthKeysFailed":    authKeysFailed,
	})
}

// postUpdateClientRetention updates the hot retention horizon and the expected
// backup interval. Offsite retention is not here: it belongs to the B2 bucket's
// lifecycle rule, which backitup neither configures nor overrides.
func (s *Server) postUpdateClientRetention(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	hotDays := atoiDefault(r.PostFormValue("retention_days"), 14)
	expectedInterval := atoiDefault(r.PostFormValue("expected_interval_secs"), 0)
	if hotDays < 1 {
		hotDays = 1
	}
	if expectedInterval < 0 {
		expectedInterval = 0
	}

	ctx, cancel := s.storeCtx(r)
	defer cancel()
	c, err := s.st.GetClientBySlug(ctx, r.PathValue("name"))
	if err != nil {
		if s.storeFailure(w, ctx, fmt.Sprintf("retention: load client %q", r.PathValue("name")), err) {
			return
		}
		http.Error(w, "failed to load client", http.StatusInternalServerError)
		return
	}
	if c == nil {
		http.NotFound(w, r)
		return
	}
	if err := s.st.UpdateClientRetention(ctx, c.ID, hotDays, expectedInterval); err != nil {
		if s.storeFailure(w, ctx, fmt.Sprintf("retention: update client %d (%q)", c.ID, c.Name), err) {
			return
		}
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/clients/"+r.PathValue("name"), http.StatusSeeOther)
}

// postUpdateClientFidelity records what this client's snapshots should capture
// beyond `-a`. Each box is "on" only when it was checked, so a form submitted
// with none of them clears every setting — the same shape as the create form,
// and no way to end up half-applied by an unchecked box that was never sent.
func (s *Server) postUpdateClientFidelity(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	ctx, cancel := s.storeCtx(r)
	defer cancel()
	c, err := s.st.GetClientBySlug(ctx, r.PathValue("name"))
	if err != nil {
		if s.storeFailure(w, ctx, fmt.Sprintf("fidelity: load client %q", r.PathValue("name")), err) {
			return
		}
		http.Error(w, "failed to load client", http.StatusInternalServerError)
		return
	}
	if c == nil {
		http.NotFound(w, r)
		return
	}
	err = s.st.UpdateClientFidelity(ctx, c.ID,
		r.PostFormValue("preserve_sparse") == "1",
		r.PostFormValue("preserve_hardlinks") == "1",
		r.PostFormValue("preserve_acls") == "1",
		r.PostFormValue("preserve_xattrs") == "1")
	if err != nil {
		if s.storeFailure(w, ctx, fmt.Sprintf("fidelity: update client %d (%q)", c.ID, c.Name), err) {
			return
		}
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/clients/"+r.PathValue("name"), http.StatusSeeOther)
}

// postUpdateClientGroupReadable flips the per-client switch that makes an rsync
// snapshot group-readable (--chmod=Dg+rx,Fg+r at the receiver).
//
// It is its own form, and its own store method, because it is the one setting
// here that trades fidelity for access: it is stated plainly on the page, and
// the operator who turns it on is told that earlier snapshots change too (the
// modes live on inodes shared by hardlinks). A targz client has no rsync
// receiver to widen anything, so the request is refused with a flash rather
// than stored and silently ignored.
func (s *Server) postUpdateClientGroupReadable(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	on := r.PostFormValue("group_readable") == "1"
	ctx, cancel := s.storeCtx(r)
	defer cancel()
	c, err := s.st.GetClientBySlug(ctx, r.PathValue("name"))
	if err != nil {
		if s.storeFailure(w, ctx, fmt.Sprintf("group-readable: load client %q", r.PathValue("name")), err) {
			return
		}
		http.Error(w, "failed to load client", http.StatusInternalServerError)
		return
	}
	if c == nil {
		http.NotFound(w, r)
		return
	}
	if on && c.Mode != model.ModeRsync {
		http.Redirect(w, r, clientFlashURL(c.Slug, "err",
			"Group-readable snapshots are an rsync setting: a tar.gz client writes one archive and has no snapshot directory to widen."),
			http.StatusSeeOther)
		return
	}
	if err := s.st.UpdateClientGroupReadable(ctx, c.ID, on); err != nil {
		if s.storeFailure(w, ctx, fmt.Sprintf("group-readable: update client %d (%q)", c.ID, c.Name), err) {
			return
		}
		http.Redirect(w, r, clientFlashURL(c.Slug, "err", "Could not save the setting: the database did not answer. Try again."),
			http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/clients/"+c.Slug, http.StatusSeeOther)
}

// postUpdateClientOwnership flips the per-client switch that makes the SERVER
// record the source's ownership in the snapshot (rsync --fake-super on the
// receiver, keeping the receiver unprivileged).
//
// It is its own form and its own store method for a different reason than the
// group-readable switch: this setting is not an rsync flag the client sends. It
// selects which forced command the client's key may run — `rrsync` or
// `backitup-rrsync` — so the store write alone changes nothing; authorized_keys
// has to be rewritten before the next run, or the page would claim a setting
// that has no effect. A targz client's receiver is not rsync at all, so the
// request is refused with a flash rather than stored and silently ignored.
func (s *Server) postUpdateClientOwnership(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	on := r.PostFormValue("record_ownership") == "1"
	ctx, cancel := s.storeCtx(r)
	defer cancel()
	c, err := s.st.GetClientBySlug(ctx, r.PathValue("name"))
	if err != nil {
		if s.storeFailure(w, ctx, fmt.Sprintf("ownership: load client %q", r.PathValue("name")), err) {
			return
		}
		http.Error(w, "failed to load client", http.StatusInternalServerError)
		return
	}
	if c == nil {
		http.NotFound(w, r)
		return
	}
	if on && c.Mode != model.ModeRsync {
		http.Redirect(w, r, clientFlashURL(c.Slug, "err",
			"Recording source ownership is an rsync setting: it changes the rsync the server runs on the other end of the transfer. A tar.gz client's own tar already records the numeric owner in the archive."),
			http.StatusSeeOther)
		return
	}
	if err := s.st.UpdateClientOwnership(ctx, c.ID, on); err != nil {
		if s.storeFailure(w, ctx, fmt.Sprintf("ownership: update client %d (%q)", c.ID, c.Name), err) {
			return
		}
		http.Redirect(w, r, clientFlashURL(c.Slug, "err", "Could not save the setting: the database did not answer. Try again."),
			http.StatusSeeOther)
		return
	}
	// authorized_keys is normally rewritten only on create/rotate/delete, but
	// this setting IS the forced command: without this the next run would use
	// the previous command and quietly store ownership the old way.
	akCtx, akCancel := s.storeCtx(r)
	defer akCancel()
	if err := s.regenAuthorizedKeys(akCtx); err != nil {
		log.Printf("ownership: client %d (%q): settings saved, but authorized_keys was not rewritten: %v", c.ID, c.Name, err)
		http.Redirect(w, r, clientFlashURL(c.Slug, "err",
			"The setting is saved, but the sshd authorized_keys file could not be rewritten, so the client's next run still uses the previous command. Check the server log and restart the server (it rewrites the file on startup)."),
			http.StatusSeeOther)
		return
	}
	stored := "the source's uid/gid"
	if !on {
		stored = "the backup account's uid/gid, as before"
	}
	http.Redirect(w, r, clientFlashURL(c.Slug, "msg",
		fmt.Sprintf("Ownership recording is %s for %s: the next run's snapshot will record %s. authorized_keys was rewritten; the client needs no change.",
			onOff(on), c.Name, stored)),
		http.StatusSeeOther)
}

// onOff spells a boolean the way the flash messages do.
func onOff(on bool) string {
	if on {
		return "on"
	}
	return "off"
}

// postForgetOffsiteObject stops tracking one offsite object without deleting the
// remote file.
//
// It is a secondary action now, not the only one: a snapshot with no hot copy is
// normally just a snapshot the pruner aged out once a newer one was offsite, and
// its row is the only way to list, download and verify the copy that actually
// exists. So the page demotes it, says what it costs, and — while the object is
// believed to still be in the bucket — requires a second confirmation that names
// the snapshot (a form field carrying its id), because a stray POST here removes
// the index entry of a live backup.
//
// Because the row goes, the dedupe records that claimed their content lives in
// that object go with it (store.ForgetOffsiteObject, one transaction): a chain
// pointing at a row nobody can read is the failure this card is about.
func (s *Server) postForgetOffsiteObject(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := s.storeCtx(r)
	defer cancel()
	c, err := s.st.GetClientBySlug(ctx, r.PathValue("name"))
	if err != nil {
		if s.storeFailure(w, ctx, fmt.Sprintf("forget offsite: load client %q", r.PathValue("name")), err) {
			return
		}
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	if c == nil {
		http.NotFound(w, r)
		return
	}
	snapshotID := r.PathValue("snapshotID")
	objs, err := s.st.ListOffsiteObjects(ctx, c.ID)
	if err != nil {
		if s.storeFailure(w, ctx, fmt.Sprintf("forget offsite: list objects for client %q", c.Name), err) {
			return
		}
		http.Error(w, "db error", http.StatusInternalServerError)
		return
	}
	var row *model.OffsiteObject
	for i := range objs {
		o := &objs[i]
		// A dedupe record names no object of its own: it is not what a row on the
		// page stands for and forgetting it would leave the owner untouched.
		if o.SnapshotID == snapshotID && o.Remote == c.OffsiteRemote && o.DedupOf == "" {
			row = o
			break
		}
	}
	if row == nil {
		http.Redirect(w, r, clientFlashURL(c.Slug, "err", fmt.Sprintf(
			"no offsite record of %s on %q: nothing to stop tracking", snapshotID, c.OffsiteRemote)),
			http.StatusSeeOther)
		return
	}
	// Second confirmation while the object is believed present. Only a check that
	// ran and reported the object gone lifts the requirement: "never verified" is
	// not evidence that the copy is gone.
	if !row.RemoteMissing {
		if err := r.ParseForm(); err != nil {
			http.Redirect(w, r, clientFlashURL(c.Slug, "err", "invalid form"), http.StatusSeeOther)
			return
		}
		if r.PostFormValue("confirm") != snapshotID {
			http.Redirect(w, r, clientFlashURL(c.Slug, "err", fmt.Sprintf(
				"stopping tracking of %s was not confirmed: the object is still in %q and this record is the only index entry for it. Nothing was removed.",
				snapshotID, c.OffsiteRemote)),
				http.StatusSeeOther)
			return
		}
	}
	dedupRows, err := s.st.ForgetOffsiteObject(ctx, c.ID, snapshotID, c.OffsiteRemote)
	if err != nil {
		if s.storeFailure(w, ctx, fmt.Sprintf("forget offsite: client=%q snap=%q", c.Name, snapshotID), err) {
			return
		}
		http.Redirect(w, r, clientFlashURL(c.Slug, "err", fmt.Sprintf("could not stop tracking %s: the database did not answer", snapshotID)),
			http.StatusSeeOther)
		return
	}
	msg := fmt.Sprintf("stopped tracking %s: the object stays in %q and backitup no longer lists, downloads or verifies it",
		snapshotID, c.OffsiteRemote)
	if dedupRows > 0 {
		msg += fmt.Sprintf("; %d deduplicated run(s) were dropped with it and will be uploaded again", dedupRows)
	}
	log.Printf("offsite forget: client=%q snap=%s remote=%s dedupe_rows=%d: tracking removed, object untouched",
		c.Name, snapshotID, c.OffsiteRemote, dedupRows)
	http.Redirect(w, r, clientFlashURL(c.Slug, "msg", msg), http.StatusSeeOther)
}

// postUpdateClientOffsite changes the offsite_remote for an existing client.
func (s *Server) postUpdateClientOffsite(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}

	remote := r.PostFormValue("offsite_remote")
	dir := strings.TrimSpace(strings.Trim(r.PostFormValue("offsite_dir"), "/"))
	cronExpr := strings.TrimSpace(r.PostFormValue("offsite_cron"))
	uploadMode := r.PostFormValue("offsite_upload_mode")
	switch uploadMode {
	case model.OffsiteUploadLatest, model.OffsiteUploadSingle:
		// keep as posted
	default:
		uploadMode = model.OffsiteUploadAll
	}

	if cronExpr != "" {
		if _, err := cron.ParseStandard(cronExpr); err != nil {
			http.Error(w, "invalid cron expression: "+err.Error(), http.StatusBadRequest)
			return
		}
	}

	// The name prefix is three-state. "No prefix" is a checkbox, so it survives
	// a browser that leaves a text input blank; a blank text input with the box
	// clear means "nobody chose one", written as NULL so the client follows the
	// fleet default and then its slug.
	namePrefix := resolveNamePrefixForm(r.PostFormValue("name_prefix"), r.PostFormValue("name_prefix_none") == "1")
	if namePrefix != nil {
		// Validated here as well as in the model because this is where the
		// operator typed it: a prefix with a separator would add a path
		// component, and the refusal has to name the offender on the page
		// rather than answer a bare 400.
		if reason := model.InvalidNamePrefix(*namePrefix); reason != "" {
			http.Redirect(w, r, clientFlashURL(r.PathValue("name"),
				"err", "backup name prefix refused: "+reason), http.StatusSeeOther)
			return
		}
	}

	ctx, cancel := s.storeCtx(r)
	defer cancel()

	// Validate that the remote exists in the DB (empty = disable offsite).
	if remote != "" {
		known, err := s.st.GetRemoteByName(ctx, remote)
		if err != nil {
			// A store blip is not an invalid remote name; do not tell the
			// operator to go and look at their remote list.
			if s.storeFailure(w, ctx, fmt.Sprintf("offsite: look up remote %q", remote), err) {
				return
			}
			http.Error(w, "invalid offsite_remote value", http.StatusBadRequest)
			return
		}
		if known == nil {
			http.Error(w, "invalid offsite_remote value", http.StatusBadRequest)
			return
		}
	}

	c, err := s.st.GetClientBySlug(ctx, r.PathValue("name"))
	if err != nil {
		if s.storeFailure(w, ctx, fmt.Sprintf("offsite: load client %q", r.PathValue("name")), err) {
			return
		}
		http.Error(w, "failed to load client", http.StatusInternalServerError)
		return
	}
	if c == nil {
		http.NotFound(w, r)
		return
	}

	if err := s.st.UpdateClientOffsite(ctx, c.ID, remote, dir, cronExpr, uploadMode, namePrefix); err != nil {
		if s.storeFailure(w, ctx, fmt.Sprintf("offsite: update client %d (%q)", c.ID, c.Name), err) {
			return
		}
		http.Error(w, "update failed", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/clients/"+r.PathValue("name"), http.StatusSeeOther)
}

// resolveNamePrefixForm turns the Settings form's two controls into the model's
// three-state prefix:
//
//	"no prefix" checked        -> "" (a deliberate "no prefix": the old names)
//	text non-empty             -> that text, trimmed (validated by the caller)
//	text empty, box clear      -> nil (nobody chose one: fleet default, then slug)
//
// The checkbox exists because a single text input cannot tell "blank, use the
// default" from "blank, write no prefix", and those two are different settings.
func resolveNamePrefixForm(text string, none bool) *string {
	if none {
		empty := ""
		return &empty
	}
	if v := strings.TrimSpace(text); v != "" {
		return &v
	}
	return nil
}

// postTestClientOffsite checks connectivity for a client's configured rclone
// remote + dir by running rclone lsd on the target path. Synchronous (fast).
func (s *Server) postTestClientOffsite(w http.ResponseWriter, r *http.Request) {
	if s.rcloneConfig == "" {
		http.Redirect(w, r, "/clients/"+r.PathValue("name")+"?err=rclone+not+configured", http.StatusSeeOther)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()

	c, err := s.st.GetClientBySlug(ctx, r.PathValue("name"))
	if err != nil {
		if s.storeFailure(w, ctx, fmt.Sprintf("offsite test: load client %q", r.PathValue("name")), err) {
			return
		}
		http.Error(w, "failed to load client", http.StatusInternalServerError)
		return
	}
	if c == nil {
		http.NotFound(w, r)
		return
	}
	if c.OffsiteRemote == "" {
		http.Redirect(w, r, "/clients/"+r.PathValue("name")+"?err=no+offsite+remote+configured", http.StatusSeeOther)
		return
	}

	// Test against the remote root only — the client-specific subdirectory is
	// created on first upload and won't exist yet for new clients.
	path := c.OffsiteRemote + ":"

	out, err := exec.CommandContext(ctx, "rclone", "--config", s.rcloneConfig, "lsd", path).CombinedOutput()
	slug := r.PathValue("name")
	if err != nil {
		log.Printf("offsite test: client=%q remote=%s: %v: %s", c.Name, path, err, out)
		http.Redirect(w, r, "/clients/"+slug+"?err="+url.QueryEscape("Connection test failed for "+path+" — check server logs for details"), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/clients/"+slug+"?msg="+url.QueryEscape("Connected to "+path+" successfully"), http.StatusSeeOther)
}

// postOffsiteRun triggers an immediate offsite upload for a client in the
// background and redirects immediately. The request context is NOT passed to
// the goroutine — it would be cancelled the moment the redirect is sent.
func (s *Server) postOffsiteRun(w http.ResponseWriter, r *http.Request) {
	if s.offsiteTrigger == nil {
		http.Error(w, "offsite not configured on this server", http.StatusServiceUnavailable)
		return
	}

	ctx, cancel := s.storeCtx(r)
	defer cancel()

	c, err := s.st.GetClientBySlug(ctx, r.PathValue("name"))
	if err != nil {
		if s.storeFailure(w, ctx, fmt.Sprintf("offsite run: load client %q", r.PathValue("name")), err) {
			return
		}
		http.Error(w, "failed to load client", http.StatusInternalServerError)
		return
	}
	if c == nil {
		http.NotFound(w, r)
		return
	}
	if c.OffsiteRemote == "" {
		http.Error(w, "client has no offsite remote configured", http.StatusBadRequest)
		return
	}

	clientID := c.ID
	clientName := c.Name
	go func() {
		if err := s.offsiteTrigger(context.Background(), clientID); err != nil {
			log.Printf("offsite run: client=%q: %v", clientName, err)
		}
	}()
	http.Redirect(w, r, "/clients/"+r.PathValue("name")+"?msg=Offsite+backup+started+in+the+background", http.StatusSeeOther)
}

// postDeleteClient removes a client and all its run history, regenerates
// authorized_keys, then redirects to the dashboard.
func (s *Server) postDeleteClient(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	if r.PostFormValue("confirm") != "1" {
		http.Error(w, "confirmation required", http.StatusBadRequest)
		return
	}

	ctx, cancel := s.storeCtx(r)
	defer cancel()

	c, err := s.st.GetClientBySlug(ctx, r.PathValue("name"))
	if err != nil {
		if s.storeFailure(w, ctx, fmt.Sprintf("delete client: load client %q", r.PathValue("name")), err) {
			return
		}
		http.Error(w, "failed to load client", http.StatusInternalServerError)
		return
	}
	if c == nil {
		http.NotFound(w, r)
		return
	}

	if err := s.st.DeleteClient(ctx, c.ID); err != nil {
		if s.storeFailure(w, ctx, fmt.Sprintf("delete client %d (%q)", c.ID, c.Name), err) {
			return
		}
		if strings.Contains(err.Error(), "not found") {
			http.NotFound(w, r)
			return
		}
		http.Error(w, "delete failed", http.StatusInternalServerError)
		return
	}

	akCtx, akCancel := context.WithTimeout(context.Background(), s.dbTimeout)
	defer akCancel()
	if err := s.regenAuthorizedKeys(akCtx); err != nil {
		log.Printf("authkeys regenerate failed after delete client %d: %v", c.ID, err)
		// The row is gone but the file may not be: if the rewrite did not land,
		// sshd still accepts the deleted client's key. That is a security
		// failure, so it goes to the operator as an error page, not a log line
		// followed by a "success" redirect.
		http.Error(w, "The client was deleted, but the SSH authorized_keys file could not be "+
			"regenerated: "+err.Error()+". Its SSH key may still be accepted until the file is "+
			"rewritten — fix the reported problem and restart the server.", http.StatusInternalServerError)
		return
	}

	http.Redirect(w, r, "/", http.StatusSeeOther)
}

// generateClientCreds generates a new SSH keypair and bearer token for the
// named client, writing errors directly to w. Returns ok=false on any error.
// tokenPrefix is the first 8 chars of the raw token stored in plaintext as a
// non-secret discriminator to short-circuit argon2 verification in clientByToken.
func generateClientCreds(w http.ResponseWriter, name string) (privPEM, pubLine, token, tokenHash, tokenPrefix string, ok bool) {
	var err error
	privPEM, pubLine, err = keys.GenerateKeypair("backitup:" + name)
	if err != nil {
		http.Error(w, "keygen failed", http.StatusInternalServerError)
		return
	}
	token, err = keys.GenerateToken()
	if err != nil {
		http.Error(w, "token gen failed", http.StatusInternalServerError)
		return
	}
	tokenHash, err = auth.HashPassword(token)
	if err != nil {
		http.Error(w, "token hash failed", http.StatusInternalServerError)
		return
	}
	if len(token) >= 8 {
		tokenPrefix = token[:8]
	}
	ok = true
	return
}

func (s *Server) renderNewClientError(w http.ResponseWriter, r *http.Request, msg string) {
	ctx, cancel := s.storeCtx(r)
	defer cancel()
	remotes, _ := s.st.ListRemotes(ctx)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusBadRequest)
	s.render(w, "clients_new.html", map[string]any{
		"Username":    usernameFromContext(r.Context()),
		"ActivePage":  "clients/new",
		"Remotes":     remotes,
		"Error":       msg,
		"PublicHost":  s.publicHost,
		"PublicAPI":   s.publicAPI,
		"ClientImage": s.clientImage,
	})
}

// regenAuthorizedKeys rewrites the sshd authorized_keys file from the current
// client list, atomically (D4). A no-op if no path is configured.
//
// Directory setup and file rewriting are deliberately not all-or-nothing: a
// client whose backup directory cannot be created (an over-long or otherwise
// unusable slug from an older row) is reported, but the file is still rewritten
// from the current rows. Aborting the rewrite on the first bad directory would
// freeze authorization for the whole fleet — and would keep a just-deleted
// client's key live, which is exactly the state deletion exists to end.
func (s *Server) regenAuthorizedKeys(ctx context.Context) error {
	if s.authKeysPath == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(s.authKeysPath), 0o700); err != nil {
		return err
	}
	clients, err := s.st.ListClients(ctx)
	if err != nil {
		return err
	}
	// Each enabled client needs its backup directory to exist before its first
	// run: rrsync (rsync mode) locks its jail root on connect, and rsync won't
	// create the missing snapshots/ parent. tar.gz's receiver mkdir's as needed,
	// but pre-creating is harmless and keeps the layout predictable.
	var dirErrs error
	for _, c := range clients {
		if !c.Enabled {
			continue
		}
		// No usable slug, no directory: MkdirAll on the base dir would silently
		// invent a shared jail. authkeys.Render withholds this client's key for
		// the same reason, so skipping the mkdir keeps the two consistent.
		dir, err := s.clientDir(c)
		if err != nil {
			log.Printf("authkeys: %v; skipping directory setup", err)
			continue
		}
		if c.Mode == model.ModeRsync {
			dir = filepath.Join(dir, "snapshots")
		}
		// 0750, not 0700: the operator reads backups as an account that is in the
		// server's group (gid 10001 in the shipped image), and these two
		// directories — <clientDir> and, for rsync, <clientDir>/snapshots — are
		// exactly the ones that would otherwise stop that account traversing into
		// the store. Everything below them is written by the rsync receiver, which
		// copies the source's modes (see model.Client.GroupReadable).
		//
		// MkdirAll does not touch the mode of a directory that already exists, so
		// a store written by an older build keeps its 0700 until
		// ReconcileClientDirModes heals it.
		if err := os.MkdirAll(dir, ClientDirMode); err != nil {
			log.Printf("authkeys: client %d (%q): cannot create %s: %v", c.ID, c.Name, dir, err)
			dirErrs = errors.Join(dirErrs, fmt.Errorf("create client dir %s: %w", dir, err))
		}
	}
	content, skipped := authkeys.Render(clients, s.backupBaseDir)
	for _, sk := range skipped {
		log.Printf("authkeys: skipped client %d: %s", sk.ClientID, sk.Reason)
	}
	if err := authkeys.WriteAtomic(s.authKeysPath, content); err != nil {
		return errors.Join(dirErrs, err)
	}
	return dirErrs
}

// apiBase returns the control-channel base URL for use in generated docker
// commands. If BACKITUP_PUBLIC_API is set it is used as-is; otherwise the
// scheme is derived from apiScheme ("http"/"https") and a placeholder host is
// returned. The placeholder deliberately carries no comment: the value is
// substituted into `-e BACKITUP_API=…`, and a `#` there would comment out the
// rest of the line. The operator-facing note is apiPlaceholderHint, which the
// page renders as prose so the generated command and the crontab entry stay
// free of comments.
func (s *Server) apiBase(apiScheme string) string {
	if s.publicAPI != "" {
		return s.publicAPI
	}
	if apiScheme != "https" {
		apiScheme = "http"
	}
	return apiScheme + "://YOUR-SERVER:8080"
}

// apiPlaceholderHint is the note shown above the generated command when
// BACKITUP_PUBLIC_API is unset: its BACKITUP_API value is a placeholder then.
// Returns "" when the server knows its own API address.
func (s *Server) apiPlaceholderHint() string {
	if s.publicAPI != "" {
		return ""
	}
	return "BACKITUP_PUBLIC_API is not set on this server, so the BACKITUP_API value in the " +
		"command below is a placeholder. Replace YOUR-SERVER with this server's control-channel " +
		"address, or set BACKITUP_PUBLIC_API and issue the client again."
}

// defaultCronSchedule is the schedule the generated crontab entry carries. The
// page says it is the field to edit, so the entry is a complete, working line
// rather than a note to the operator.
const defaultCronSchedule = "30 2 * * *"

// sourcePlaceholder stands in for the directory being backed up when neither
// the create form nor the client's source label names one. It deliberately
// does not say BACKUP: the path is mounted read-only as the source, and calling
// it BACKUP reads like the destination.
const sourcePlaceholder = "/path/to/source"

// looksLikePath reports whether a source label names a filesystem path rather
// than describing one. "~/Documents on my laptop" is a description (it has
// spaces); "/mnt/photos" and "~/photos" are paths.
func looksLikePath(label string) bool {
	l := strings.TrimSpace(label)
	if l == "" || strings.ContainsAny(l, " 	") {
		return false
	}
	return strings.HasPrefix(l, "/") || strings.HasPrefix(l, "~") ||
		strings.HasPrefix(l, "./") || strings.HasPrefix(l, "../")
}

// secretsDir returns the host directory that holds a client's credentials: the
// path from the form when the operator gave one, otherwise the conventional
// /etc/backitup/<name>. The page names exactly this directory in its first step
// and the generated command mounts exactly this directory, so the two can never
// disagree.
func secretsDir(name, formPath string) string {
	if p := strings.TrimSpace(formPath); p != "" {
		return p
	}
	return "/etc/backitup/" + name
}

// sourceMount returns the host directory mounted read-only as the backup
// source: the path from the form, else the client's source label when that
// looks like a path, else sourcePlaceholder. prefilled reports whether the
// value is real, so the page knows whether to ask for a replacement.
func sourceMount(formPath, sourceLabel string) (mount string, prefilled bool) {
	if p := strings.TrimSpace(formPath); p != "" {
		return p, true
	}
	if looksLikePath(sourceLabel) {
		return strings.TrimSpace(sourceLabel), true
	}
	return sourcePlaceholder, false
}

// cronEntry turns a generated command into a complete crontab entry: the
// schedule, then the whole command with its line continuations intact. Cron
// accepts continuation lines in a crontab file, so the operator pastes this
// verbatim instead of assembling a line out of two boxes.
func cronEntry(schedule, cmd string) string {
	if cmd == "" {
		return ""
	}
	return schedule + " " + cmd
}

// dockerCmds returns the ready-to-run docker commands for a client:
//   - known: mounts /secrets with BACKITUP_KNOWN_HOSTS (recommended). Empty when
//     the server has no host key, because it would then verify against a
//     known_hosts file the page never handed over.
//   - insecure: adds BACKITUP_INSECURE=1 and skips host-key verification.
//
// sourcePath and secretsPath are the concrete host paths; the caller resolves
// them with sourceMount and secretsDir so the page and the command agree.
func (s *Server) dockerCmds(token, apiBase, sourcePath, secretsPath string, withKnownHosts bool) (known, insecure string) {
	base := []string{
		"docker run --rm \\",
		"  --user $(id -u):$(id -g) \\",
		fmt.Sprintf("  --mount type=bind,src=%s,dst=/source,readonly \\", sourcePath),
		fmt.Sprintf("  -v %s:/secrets:ro \\", secretsPath),
		fmt.Sprintf("  -e BACKITUP_API=%s \\", apiBase),
		fmt.Sprintf("  -e BACKITUP_SERVER=%s \\", s.publicHost),
		fmt.Sprintf("  -e BACKITUP_TOKEN=%s \\", token),
		"  -e BACKITUP_SSH_KEY=/secrets/id \\",
	}
	join := func(extra ...string) string {
		parts := append(append([]string{}, base...), extra...)
		parts = append(parts, "  "+s.clientImage)
		return strings.Join(parts, "\n")
	}
	insecure = join("  -e BACKITUP_INSECURE=1 \\")
	if withKnownHosts {
		known = join("  -e BACKITUP_KNOWN_HOSTS=/secrets/known_hosts \\")
	}
	return
}

func atoiDefault(s string, def int) int {
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return def
	}
	return n
}

// knownHostsLine reads the sshd host public key from keyPath and formats it
// as a known_hosts entry for publicHost (host:port). Returns "" if the file
// is missing or unparseable — callers treat "" as "host key not yet available".
func knownHostsLine(publicHost, keyPath string) string {
	if keyPath == "" {
		return ""
	}
	data, err := os.ReadFile(keyPath)
	if err != nil {
		return ""
	}
	// File format: "ssh-ed25519 AAAA... comment\n"
	// known_hosts format: "[host]:port keytype base64key"
	fields := strings.Fields(strings.TrimSpace(string(data)))
	if len(fields) < 2 {
		return ""
	}
	host, port, err := net.SplitHostPort(publicHost)
	if err != nil {
		host = publicHost
		port = "22"
	}
	var addr string
	if port == "22" {
		addr = host
	} else {
		addr = fmt.Sprintf("[%s]:%s", host, port)
	}
	return fmt.Sprintf("%s %s %s", addr, fields[0], fields[1])
}

// clientTab whitelists the ?tab= parameter of the client page. An unknown or
// empty value falls back to the first tab rather than rendering a page with
// every panel waiting for JavaScript to reveal one.
func clientTab(q string) string {
	switch q {
	case "offsite", "runs", "settings":
		return q
	}
	return "snapshots"
}
