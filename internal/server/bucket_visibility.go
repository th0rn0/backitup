package server

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

// bucketCheckTimeout bounds the rclone calls the save-time bucket check makes.
// A settings form must not hang on a slow provider.
const bucketCheckTimeout = 20 * time.Second

// verifyBucketVisible asks rclone which buckets the submitted credentials can
// see, and refuses the save when the configured bucket is not one of them.
//
// rclone's B2 backend creates a bucket when its own lookup does not find it, so
// a key that cannot see the bucket named on the remote fails at upload time with
// `400 duplicate_bucket_name` — "that name exists, and it is not yours to
// create" — on a destination path, with nothing pointing back at the remote that
// was configured with it and nothing checking it when it was configured. The
// check belongs at the form: rclone lsd <remote>: lists the buckets the key can
// see, and a bucket missing from that listing is refused by name, with the
// buckets the key *can* see named so the operator can tell a typo from a key
// without access.
//
// A bucket-scoped application key cannot enumerate the account's buckets at all
// (B2's listBuckets capability is not part of such a key), and that is the key a
// remote should carry — so a failed or empty listing falls back to listing the
// bucket itself: rclone lsd <remote>:<bucket> succeeds exactly when the key can
// address it. Only rclone running and saying no is a refusal; a check that could
// not run at all (rclone not installed, provider unreachable) is logged and the
// save goes ahead, because a machine fault must not make the remote impossible
// to configure.
//
// Returns an operator-facing refusal, or an error when the check itself failed
// in a way that is not about the operator's input.
func (s *Server) verifyBucketVisible(ctx context.Context, name string, cfg map[string]string) (string, error) {
	bucket := strings.TrimSpace(cfg["bucket"])
	if bucket == "" || s.rcloneConfig == "" {
		// Nothing to check against: no bucket named on the remote, or rclone is
		// not managed here at all, in which case the remote is inert.
		return "", nil
	}

	confPath, cleanup, err := writeProbeConfig(name, cfg)
	if err != nil {
		return "", err
	}
	defer cleanup()

	ctx, cancel := context.WithTimeout(ctx, bucketCheckTimeout)
	defer cancel()

	visible, listErr := s.rcloneBuckets(ctx, confPath, name)
	if listErr == nil && len(visible) > 0 {
		for _, b := range visible {
			if b == bucket {
				return "", nil
			}
		}
		return fmt.Sprintf("Bucket %q is not visible to this key: rclone lsd %s: lists %s. "+
			"B2 application keys cannot be widened once they are created, so add a new key with read "+
			"and write access to %q and save its keyID and applicationKey here.",
			bucket, name, strings.Join(visible, ", "), bucket), nil
	}

	// Either the key cannot enumerate buckets, or it enumerated none. Ask about
	// the bucket itself: a key that can address it does see it.
	probeErr := s.rcloneListBucket(ctx, confPath, name, bucket)
	if probeErr == nil {
		return "", nil
	}
	if ctx.Err() != nil || !ranRclone(probeErr) {
		// The command never ran or never answered (rclone not installed,
		// deadline, exec failure). Say so in the log and let the operator save:
		// the upload path reports a real permission problem when it meets one.
		log.Printf("remote %q: bucket %q visibility could not be checked: %v", name, bucket, probeErr)
		return "", nil
	}
	return fmt.Sprintf("Bucket %q is not visible to this key: rclone could not list %s:%s (%s). "+
		"B2 application keys cannot be widened once they are created, so add a new key with read "+
		"and write access to %q and save its keyID and applicationKey here.",
		bucket, name, bucket, probeDetail(probeErr.Error()), bucket), nil
}

// writeProbeConfig writes a throwaway rclone config holding only the candidate
// section, so the check runs before the row exists (create) and without writing
// credentials that have not been accepted yet into the shared file (edit). It is
// created 0600 and the caller removes it: it carries the application key.
func writeProbeConfig(name string, cfg map[string]string) (path string, cleanup func(), err error) {
	f, err := os.CreateTemp("", "backitup-bucket-check-*.conf")
	if err != nil {
		return "", nil, fmt.Errorf("create bucket-check config: %w", err)
	}
	path = f.Name()
	remove := func() { _ = os.Remove(path) }
	section := model.Remote{Name: name, Backend: model.BackendB2, Config: cfg}.RcloneSectionFor(nil)
	if _, err := f.WriteString(section); err != nil {
		_ = f.Close()
		remove()
		return "", nil, fmt.Errorf("write bucket-check config: %w", err)
	}
	if err := f.Close(); err != nil {
		remove()
		return "", nil, fmt.Errorf("write bucket-check config: %w", err)
	}
	return path, remove, nil
}

// rcloneBuckets lists the buckets the key in confPath can see. A bucket-scoped
// key answers with an error (no listBuckets capability), which the caller
// handles by asking about the bucket directly.
func (s *Server) rcloneBuckets(ctx context.Context, confPath, name string) ([]string, error) {
	out, err := exec.CommandContext(ctx, "rclone", "--config", confPath, "lsd", name+":").CombinedOutput()
	if err != nil {
		return nil, newProbeFailure(err, out)
	}
	var buckets []string
	for _, line := range strings.Split(string(out), "\n") {
		fields := strings.Fields(line)
		if len(fields) == 0 {
			continue
		}
		buckets = append(buckets, strings.TrimSuffix(fields[len(fields)-1], "/"))
	}
	return buckets, nil
}

// rcloneListBucket asks whether the key in confPath can address one bucket.
func (s *Server) rcloneListBucket(ctx context.Context, confPath, name, bucket string) error {
	out, err := exec.CommandContext(ctx, "rclone", "--config", confPath, "lsd", name+":"+bucket).CombinedOutput()
	if err != nil {
		return newProbeFailure(err, out)
	}
	return nil
}

// probeFailure carries rclone's own one-line explanation as the error text while
// keeping the exec error in the chain, so ranRclone can still tell "rclone ran
// and refused" from "rclone never ran".
type probeFailure struct {
	exit   *exec.ExitError
	detail string
}

func (e *probeFailure) Error() string { return e.detail }
func (e *probeFailure) Unwrap() error { return e.exit }

// newProbeFailure builds a probeFailure from an rclone invocation's error and
// output. When the command never started (rclone not on PATH) there is no exit
// status, only exec's reason, and Unwrap returns nil.
func newProbeFailure(err error, out []byte) error {
	detail := probeDetail(string(out))
	if detail == "no output" {
		detail = err.Error()
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return &probeFailure{exit: exitErr, detail: detail}
	}
	return fmt.Errorf("%v: %s", err, detail)
}

// ranRclone reports whether err came from rclone itself exiting non-zero — an
// answer about the credentials — as opposed to the command never running
// (rclone not installed) or an exec failure. The two callers wrap with %w so the
// *exec.ExitError stays in the chain.
func ranRclone(err error) bool {
	var exitErr *exec.ExitError
	return err != nil && errors.As(err, &exitErr)
}

// probeDetail returns the first non-empty line of an rclone failure, capped, for
// the operator-facing message. The full output of an upload failure stays in the
// log at debug level; this is the settings form's one line.
func probeDetail(s string) string {
	for _, line := range strings.Split(s, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		const max = 200
		if len(line) > max {
			line = line[:max] + "…"
		}
		return line
	}
	return "no output"
}
