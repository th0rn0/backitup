package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sync"
	"syscall"

	"github.com/th0rn0/backitup/internal/model"
)

// ClientDirMode is the mode of the two directories the server itself creates in
// the hot store: <clientDir> and, for rsync, <clientDir>/snapshots.
//
// 0750 rather than 0700 because the operator reads backups as an account that is
// in the server's group — gid 10001 in the shipped image — instead of as root.
// The group is the control; there is deliberately no per-client ACL state. What
// lives *below* these two directories is written by the rsync receiver, which
// copies the source's modes (see model.Client.GroupReadable for the per-client
// way to widen those too).
//
// The app-data directory (the SQLite database and its parent) is NOT part of
// this and stays 0700: the database is not a backup.
const ClientDirMode os.FileMode = 0o750

// dirModeLog dedupes the one line per directory an operator has to see. A
// client's page is polled, its config is fetched before every run, and the
// startup sweep walks the fleet — a directory that is already right, or whose
// owner the server must not touch, must not say so on every pass.
var (
	dirModeLogMu sync.Mutex
	dirModeLogAt = map[string]string{} // directory -> last state logged
)

// logDirModeOnce logs one directory's state at most once per distinct state.
func logDirModeOnce(dir, state, format string, args ...any) {
	dirModeLogMu.Lock()
	defer dirModeLogMu.Unlock()
	if dirModeLogAt[dir] == state {
		return
	}
	dirModeLogAt[dir] = state
	log.Printf(format, args...)
}

// reconcileDirMode makes one server-owned store directory group-traversable.
//
// It only touches a directory owned by the server's own uid. A store whose
// directories belong to a different uid is the bind-mount (or root-owned named
// volume) misconfiguration, and silently chmod-ing a tree that somebody else
// owns is worse than one legible line saying which client's directory it was.
// selfUID is a parameter rather than os.Geteuid() inside the function so the
// "owned by another uid" branch is reachable from a test running as an
// unprivileged user, which cannot chown.
func reconcileDirMode(dir, clientName string, selfUID uint32) error {
	fi, err := os.Lstat(dir)
	if errors.Is(err, os.ErrNotExist) {
		// Nothing to heal: the creation path (regenAuthorizedKeys -> MkdirAll at
		// ClientDirMode) is what makes these directories.
		return nil
	}
	if err != nil {
		return fmt.Errorf("stat %s: %w", dir, err)
	}
	if !fi.IsDir() {
		return nil
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid != selfUID {
		logDirModeOnce(dir, "foreign",
			"store: client %q: %s is owned by uid %d, not this process (uid %d) — left alone. "+
				"A backup directory owned by another user is a bind-mount/volume ownership problem: "+
				"fix the mount, chmod in the container is not enough",
			clientName, dir, st.Uid, selfUID)
		return nil
	}
	perm := fi.Mode().Perm()
	if perm&0o050 == 0o050 {
		return nil // already group-traversable: nothing more restrictive to heal
	}
	if err := os.Chmod(dir, ClientDirMode); err != nil {
		return fmt.Errorf("chmod %s: %w", dir, err)
	}
	logDirModeOnce(dir, "healed",
		"store: client %q: %s was %04o; widened to %04o so an account in the server's group (gid 10001) can read this client's backups",
		clientName, dir, perm, ClientDirMode)
	return nil
}

// storeDirsFor lists the directories the server itself creates and therefore
// owns for c.
func (s *Server) storeDirsFor(c model.Client) ([]string, error) {
	dir, err := s.clientDir(c)
	if err != nil {
		return nil, err
	}
	if c.Mode == model.ModeRsync {
		return []string{dir, filepath.Join(dir, "snapshots")}, nil
	}
	return []string{dir}, nil
}

// ReconcileClientDirModes heals every client's server-owned directories.
//
// Called at startup because a store written by a build that predates
// ClientDirMode has 0700 there, and the operator's new group membership buys
// nothing until those two directories are 0750. Also called after a client is
// created or loaded: a directory re-used by a client that was deleted and added
// again keeps whatever mode it had.
func (s *Server) ReconcileClientDirModes(ctx context.Context) {
	clients, err := s.st.ListClients(ctx)
	if err != nil {
		log.Printf("store: cannot reconcile client directory modes: %v", err)
		return
	}
	for _, c := range clients {
		s.reconcileClientDirModes(c)
	}
}

// reconcileClientDirModes is the single-client form — "after a client is
// loaded". Failures are logged, never returned: a chmod that cannot happen must
// not fail a backup run or an operator's page.
func (s *Server) reconcileClientDirModes(c model.Client) {
	dirs, err := s.storeDirsFor(c)
	if err != nil {
		log.Printf("store: client %q: %v; leaving its directories alone", c.Name, err)
		return
	}
	selfUID := uint32(os.Geteuid())
	for _, dir := range dirs {
		if err := reconcileDirMode(dir, c.Name, selfUID); err != nil {
			log.Printf("store: client %q: %v", c.Name, err)
		}
	}
}
