package server

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/th0rn0/backitup/internal/lifecycle"
	"github.com/th0rn0/backitup/internal/model"
	"github.com/th0rn0/backitup/internal/mode"
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
		if err := tarGzDirectory(w, filepath.Join(clientDir, snap.ID)); err != nil {
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
	if snapshotID == "" || strings.Contains(snapshotID, "..") {
		http.NotFound(w, r)
		return
	}

	if err := sm.DeleteSnapshot(ctx, clientDir, snapshotID); err != nil {
		log.Printf("snapshot delete: client=%s snap=%s: %v", c.Name, snapshotID, err)
		http.Redirect(w, r, "/clients/"+c.Slug()+"?err=delete+failed", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/clients/"+c.Slug()+"?msg=snapshot+deleted", http.StatusSeeOther)
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
		offsiteDir = model.Slug(c.Name)
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
		http.Redirect(w, r, "/clients/"+c.Slug()+"?err=offsite+not+configured", http.StatusSeeOther)
		return
	}
	snapshotID := r.PathValue("snapshotID")
	if snapshotID == "" || strings.Contains(snapshotID, "..") {
		http.NotFound(w, r)
		return
	}

	offsiteDir := c.OffsiteDir
	if offsiteDir == "" {
		offsiteDir = model.Slug(c.Name)
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
		http.Redirect(w, r, "/clients/"+c.Slug()+"?err=offsite+delete+failed", http.StatusSeeOther)
		return
	}
	if err := s.st.DeleteOffsiteObject(ctx, c.ID, snapshotID, c.OffsiteRemote); err != nil {
		log.Printf("offsite object delete: db record: %v", err)
	}
	http.Redirect(w, r, "/clients/"+c.Slug()+"?msg=offsite+object+deleted", http.StatusSeeOther)
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
	clientDir := filepath.Join(s.backupBaseDir, model.Slug(c.Name))
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

// tarGzDirectory writes a gzip-compressed tar archive of dir to w.
func tarGzDirectory(w io.Writer, dir string) error {
	gw := gzip.NewWriter(w)
	tw := tar.NewWriter(gw)
	base := filepath.Base(dir)
	err := filepath.Walk(dir, func(path string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		hdr, err := tar.FileInfoHeader(fi, "")
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
		if fi.IsDir() || !fi.Mode().IsRegular() {
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
	var errs []string
	for _, id := range ids {
		if id == "" || strings.Contains(id, "..") {
			continue
		}
		if err := sm.DeleteSnapshot(ctx, clientDir, id); err != nil {
			log.Printf("bulk snapshot delete: client=%s snap=%s: %v", c.Name, id, err)
			errs = append(errs, id)
		}
	}
	if len(errs) > 0 {
		http.Redirect(w, r, "/clients/"+c.Slug()+"?err=some+deletes+failed", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/clients/"+c.Slug()+"?msg=snapshots+deleted", http.StatusSeeOther)
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
		http.Redirect(w, r, "/clients/"+c.Slug()+"?err=offsite+not+configured", http.StatusSeeOther)
		return
	}
	offsiteDir := c.OffsiteDir
	if offsiteDir == "" {
		offsiteDir = model.Slug(c.Name)
	}
	rclone := lifecycle.NewRclone(s.rcloneConfig)
	var errs []string
	for _, id := range ids {
		if id == "" || strings.Contains(id, "..") {
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
			errs = append(errs, id)
		}
		if err := s.st.DeleteOffsiteObject(ctx, c.ID, id, c.OffsiteRemote); err != nil {
			log.Printf("bulk offsite delete: db record: %v", err)
		}
	}
	if len(errs) > 0 {
		http.Redirect(w, r, "/clients/"+c.Slug()+"?err=some+offsite+deletes+failed", http.StatusSeeOther)
		return
	}
	http.Redirect(w, r, "/clients/"+c.Slug()+"?msg=offsite+objects+deleted", http.StatusSeeOther)
}

// rcloneLsf lists filenames in a remote directory using rclone lsf.
func rcloneLsf(ctx context.Context, configPath, remote, dir string) ([]string, error) {
	args := []string{"lsf", remote + ":" + dir}
	if configPath != "" {
		args = append([]string{"--config", configPath}, args...)
	}
	cmd := exec.CommandContext(ctx, "rclone", args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("rclone lsf: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(stdout.String()), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
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
