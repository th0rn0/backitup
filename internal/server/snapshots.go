package server

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/th0rn0/backitup/internal/lifecycle"
	"github.com/th0rn0/backitup/internal/mode"
	"github.com/th0rn0/backitup/internal/model"
)

// getSnapshotDownload streams a local snapshot to the browser.
// For tar.gz mode: serves the archive directly.
// For rsync mode: creates an on-the-fly tar.gz from the snapshot directory.
// ?view=1 sets Content-Disposition: inline (open in tab) instead of attachment.
func (s *Server) getSnapshotDownload(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()

	c, sm, clientDir, ok := s.resolveClientAndMode(w, r, ctx)
	if !ok {
		return
	}
	snapshotID := r.PathValue("snapshotID")
	if snapshotID == "" || strings.Contains(snapshotID, "..") {
		http.NotFound(w, r)
		return
	}

	snaps, err := sm.List(ctx, clientDir)
	if err != nil {
		http.Error(w, "failed to list snapshots", http.StatusInternalServerError)
		return
	}
	var snap *mode.Snapshot
	for i := range snaps {
		if snaps[i].ID == snapshotID {
			snap = &snaps[i]
			break
		}
	}
	if snap == nil {
		http.NotFound(w, r)
		return
	}

	inline := r.URL.Query().Get("view") == "1"
	disposition := "attachment"
	if inline {
		disposition = "inline"
	}

	switch c.Mode {
	case model.ModeTarGz:
		path := filepath.Join(clientDir, snap.ID)
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename="%s"`, disposition, snap.ID))
		http.ServeFile(w, r, path)

	case model.ModeRsync:
		filename := snap.ID + ".tar.gz"
		w.Header().Set("Content-Type", "application/gzip")
		w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename="%s"`, disposition, filename))
		if err := tarGzDirectory(w, filepath.Join(clientDir, "snapshots", snap.ID)); err != nil {
			log.Printf("snapshot download: tar client=%s snap=%s: %v", c.Name, snap.ID, err)
		}
	}
}

// postSnapshotDelete deletes a single local snapshot.
func (s *Server) postSnapshotDelete(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	c, sm, clientDir, ok := s.resolveClientAndMode(w, r, ctx)
	if !ok {
		return
	}
	snapshotID := r.PathValue("snapshotID")
	if err := mode.ValidateSnapshotID(c.Mode, snapshotID); err != nil {
		// Legibility, not permission: the request is still refused (nothing is
		// unlinked), but the operator gets the reason on the page instead of a
		// bare 404 with the cause stranded in the container log.
		log.Printf("snapshot delete: client=%s snap=%q: refusing request: %v", c.Name, snapshotID, err)
		http.Redirect(w, r, clientFlashURL(c.Slug, "err", "delete refused: "+err.Error()), http.StatusSeeOther)
		return
	}

	if err := sm.DeleteSnapshot(ctx, clientDir, snapshotID); err != nil {
		log.Printf("snapshot delete: client=%s snap=%s: %v", c.Name, snapshotID, err)
		// The error's own text, not "delete failed": the path in it is what
		// identifies the failing tier (host dir, permissions, readonly mount).
		http.Redirect(w, r, clientFlashURL(c.Slug, "err", "delete failed: "+err.Error()), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/clients/"+c.Slug+"?msg=snapshot+deleted", http.StatusSeeOther)
}

// getOffsiteDownload streams an offsite object through the server via rclone cat.
func (s *Server) getOffsiteDownload(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
	defer cancel()

	c, ok := s.resolveClient(w, r, ctx)
	if !ok {
		return
	}
	if c.OffsiteRemote == "" || s.rcloneConfig == "" {
		http.Error(w, "offsite not configured", http.StatusBadRequest)
		return
	}
	snapshotID := r.PathValue("snapshotID")
	if snapshotID == "" || strings.Contains(snapshotID, "..") {
		http.NotFound(w, r)
		return
	}

	offsiteDir := c.OffsiteDir
	if offsiteDir == "" {
		offsiteDir = c.Slug
	}
	var objectName, objectPath string
	if c.Mode == model.ModeRsync {
		objectPath = offsiteDir + "/" + snapshotID + ".tar.gz"
		objectName = snapshotID + ".tar.gz"
	} else {
		objectPath = offsiteDir + "/" + snapshotID
		objectName = snapshotID
	}

	inline := r.URL.Query().Get("view") == "1"
	disposition := "attachment"
	if inline {
		disposition = "inline"
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`%s; filename="%s"`, disposition, objectName))

	if err := rcloneCatStream(ctx, s.rcloneConfig, c.OffsiteRemote+":"+objectPath, w); err != nil {
		log.Printf("offsite download: client=%s snap=%s: %v", c.Name, snapshotID, err)
	}
}

// postOffsiteObjectDelete removes a single offsite object and its DB record.
func (s *Server) postOffsiteObjectDelete(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Minute)
	defer cancel()

	c, ok := s.resolveClient(w, r, ctx)
	if !ok {
		return
	}
	if c.OffsiteRemote == "" {
		http.Redirect(w, r, "/clients/"+c.Slug+"?err=offsite+not+configured", http.StatusSeeOther)
		return
	}
	snapshotID := r.PathValue("snapshotID")
	if snapshotID == "" || strings.Contains(snapshotID, "..") {
		http.NotFound(w, r)
		return
	}

	offsiteDir := c.OffsiteDir
	if offsiteDir == "" {
		offsiteDir = c.Slug
	}
	var objectPath string
	if c.Mode == model.ModeRsync {
		objectPath = offsiteDir + "/" + snapshotID + ".tar.gz"
	} else {
		objectPath = offsiteDir + "/" + snapshotID
	}

	rclone := lifecycle.NewRclone(s.rcloneConfig)
	if err := rclone.Delete(ctx, c.OffsiteRemote, objectPath); err != nil {
		log.Printf("offsite object delete: client=%s snap=%s: %v", c.Name, snapshotID, err)
		http.Redirect(w, r, clientFlashURL(c.Slug, "err", "offsite delete failed: "+err.Error()), http.StatusSeeOther)
		return
	}
	if err := s.st.DeleteOffsiteObject(ctx, c.ID, snapshotID, c.OffsiteRemote); err != nil {
		log.Printf("offsite object delete: db record: %v", err)
	}
	http.Redirect(w, r, "/clients/"+c.Slug+"?msg=offsite+object+deleted", http.StatusSeeOther)
}

// resolveClientAndMode loads the client and its ServerMode for snapshot operations.
func (s *Server) resolveClientAndMode(w http.ResponseWriter, r *http.Request, ctx context.Context) (*model.Client, mode.ServerMode, string, bool) {
	c, ok := s.resolveClient(w, r, ctx)
	if !ok {
		return nil, nil, "", false
	}
	sm, found := mode.Server(c.Mode)
	if !found {
		http.Error(w, "unknown mode", http.StatusInternalServerError)
		return nil, nil, "", false
	}
	clientDir, err := s.clientDir(*c)
	if err != nil {
		http.Error(w, "client has no usable storage slug", http.StatusInternalServerError)
		return nil, nil, "", false
	}
	return c, sm, clientDir, true
}

func (s *Server) resolveClient(w http.ResponseWriter, r *http.Request, ctx context.Context) (*model.Client, bool) {
	c, err := s.st.GetClientBySlug(ctx, r.PathValue("name"))
	if err != nil {
		http.Error(w, "db error", http.StatusInternalServerError)
		return nil, false
	}
	if c == nil {
		http.NotFound(w, r)
		return nil, false
	}
	return c, true
}

// tarGzDirectory writes a gzip-compressed tar archive of dir to w,
// preserving permissions, ownership, and symlink targets.
func tarGzDirectory(w io.Writer, dir string) error {
	gw := gzip.NewWriter(w)
	tw := tar.NewWriter(gw)
	base := filepath.Base(dir)
	err := filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		var link string
		if fi.Mode()&os.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		}
		hdr, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		hdr.Name = base + "/" + rel
		if fi.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if !fi.Mode().IsRegular() {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		defer f.Close()
		_, err = io.Copy(tw, f)
		return err
	})
	if err != nil {
		return err
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gw.Close()
}

// postBulkDeleteSnapshots deletes multiple local snapshots in one request.
func (s *Server) postBulkDeleteSnapshots(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	ids := r.Form["ids"]
	if len(ids) == 0 {
		http.Redirect(w, r, "/clients/"+r.PathValue("name"), http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	c, sm, clientDir, ok := s.resolveClientAndMode(w, r, ctx)
	if !ok {
		return
	}
	// Validate every id BEFORE deleting anything. A request carrying one bad id
	// (`ids=.` reaches os.RemoveAll(<clientDir>/snapshots)) must delete nothing at
	// all, not "everything except the bad one" — a partial batch is data loss the
	// operator never asked for. The refusal is still legible: the flash names
	// every id that was rejected, so a hand-built form is diagnosable from the
	// page and not only from the container log.
	var refusals []string
	for _, id := range ids {
		if err := mode.ValidateSnapshotID(c.Mode, id); err != nil {
			log.Printf("bulk snapshot delete: client=%s: refusing request: %v", c.Name, err)
			refusals = append(refusals, err.Error())
		}
	}
	if len(refusals) > 0 {
		http.Redirect(w, r, clientFlashURL(c.Slug, "err", fmt.Sprintf(
			"nothing was deleted: %d of %d selected ids were refused: %s",
			len(refusals), len(ids), joinCapped(refusals, 5))), http.StatusSeeOther)
		return
	}
	var errs []string
	var firstErr error
	for _, id := range ids {
		if err := sm.DeleteSnapshot(ctx, clientDir, id); err != nil {
			log.Printf("bulk snapshot delete: client=%s snap=%s: %v", c.Name, id, err)
			errs = append(errs, fmt.Sprintf("%q", id))
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	if len(errs) > 0 {
		http.Redirect(w, r, clientFlashURL(c.Slug, "err", fmt.Sprintf(
			"deleted %d of %d snapshots; failed: %s; first cause: %v",
			len(ids)-len(errs), len(ids), joinCapped(errs, 5), firstErr)), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/clients/"+c.Slug+"?msg=snapshots+deleted", http.StatusSeeOther)
}

// postBulkDeleteOffsiteObjects deletes multiple offsite objects in one request.
func (s *Server) postBulkDeleteOffsiteObjects(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "invalid form", http.StatusBadRequest)
		return
	}
	ids := r.Form["ids"]
	if len(ids) == 0 {
		http.Redirect(w, r, "/clients/"+r.PathValue("name"), http.StatusSeeOther)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 10*time.Minute)
	defer cancel()

	c, ok := s.resolveClient(w, r, ctx)
	if !ok {
		return
	}
	if c.OffsiteRemote == "" {
		http.Redirect(w, r, "/clients/"+c.Slug+"?err=offsite+not+configured", http.StatusSeeOther)
		return
	}
	offsiteDir := c.OffsiteDir
	if offsiteDir == "" {
		offsiteDir = c.Slug
	}
	rclone := lifecycle.NewRclone(s.rcloneConfig)
	var failed, skipped []string
	var firstErr error
	deleted := 0
	for _, id := range ids {
		if id == "" || strings.Contains(id, "..") {
			// Never addressed before; still reported, so "Delete selected" that
			// quietly dropped an entry is visible on the page.
			log.Printf("bulk offsite delete: client=%s: skipping invalid id %q", c.Name, id)
			skipped = append(skipped, fmt.Sprintf("%q", id))
			continue
		}
		var objectPath string
		if c.Mode == model.ModeRsync {
			objectPath = offsiteDir + "/" + id + ".tar.gz"
		} else {
			objectPath = offsiteDir + "/" + id
		}
		if err := rclone.Delete(ctx, c.OffsiteRemote, objectPath); err != nil {
			log.Printf("bulk offsite delete: client=%s snap=%s: %v", c.Name, id, err)
			failed = append(failed, fmt.Sprintf("%q", id))
			if firstErr == nil {
				firstErr = err
			}
		} else {
			deleted++
		}
		if err := s.st.DeleteOffsiteObject(ctx, c.ID, id, c.OffsiteRemote); err != nil {
			log.Printf("bulk offsite delete: db record: %v", err)
		}
	}
	if len(failed) > 0 || len(skipped) > 0 {
		msg := fmt.Sprintf("deleted %d of %d selected offsite objects", deleted, len(ids))
		if len(failed) > 0 {
			msg += fmt.Sprintf("; failed: %s; first cause: %v", joinCapped(failed, 5), firstErr)
		}
		if len(skipped) > 0 {
			msg += "; skipped invalid ids: " + joinCapped(skipped, 5)
		}
		http.Redirect(w, r, clientFlashURL(c.Slug, "err", msg), http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/clients/"+c.Slug+"?msg=offsite+objects+deleted", http.StatusSeeOther)
}

// clientFlashURL builds the redirect that carries a handler's outcome back to
// the client page. The reason travels in the query string because that is where
// client_detail.html reads its alert from; url.Values.Encode percent-encodes it,
// so a message containing '/', ':' or spaces still lands as one parameter.
func clientFlashURL(slug, key, text string) string {
	return "/clients/" + slug + "?" + url.Values{key: {text}}.Encode()
}

// joinCapped joins items with "; ", naming at most limit of them. A hand-built
// form can carry thousands of ids: the flash names the offenders without
// turning the Location header into a wall of text.
func joinCapped(items []string, limit int) string {
	if len(items) <= limit {
		return strings.Join(items, "; ")
	}
	return fmt.Sprintf("%s; and %d more", strings.Join(items[:limit], "; "), len(items)-limit)
}

// rcloneCatStream pipes a remote object to w using rclone cat.
func rcloneCatStream(ctx context.Context, configPath, remotePath string, w io.Writer) error {
	args := []string{"cat", remotePath}
	if configPath != "" {
		args = append([]string{"--config", configPath}, args...)
	}
	cmd := exec.CommandContext(ctx, "rclone", args...)
	cmd.Stdout = w
	var stderr strings.Builder
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("rclone cat: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	return nil
}
