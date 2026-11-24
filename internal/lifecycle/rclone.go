package lifecycle

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/th0rn0/backitup/internal/model"
)

// Rclone is the production Offsite backend. It shells out to the rclone CLI; the
// remote is expected to be an encrypted crypt remote (so the provider only sees
// ciphertext). Offsite encryption lives in rclone.conf, not here.
type Rclone struct {
	bin    string
	config string // optional --config path
}

// NewRclone returns an Rclone using the given config path (empty = rclone default).
func NewRclone(configPath string) *Rclone {
	return &Rclone{bin: "rclone", config: configPath}
}

// Upload copies localPath to <remote>:<objectName>, creating remote parents as
// needed. Returns the local (plaintext) size.
func (r *Rclone) Upload(ctx context.Context, localPath, remote, objectName string) (int64, error) {
	// --retries 1: rclone retries an operation three times by default. That is
	// right for a transient network error, but a refusal that is a property of
	// the destination — a bucket the key cannot see, which rclone reports by
	// trying to create it — repeats identically three times per snapshot, per
	// pass, per client. One attempt; the next scheduled pass is the retry.
	if _, err := r.run(ctx, "copyto", []string{"--retries", "1"}, localPath, remote+":"+objectName); err != nil {
		if strings.Contains(err.Error(), "duplicate_bucket_name") {
			return 0, duplicateBucketError(remote, objectName)
		}
		return 0, err
	}
	fi, err := os.Stat(localPath)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// duplicateBucketError is what the operator reads instead of rclone's raw
// `400 duplicate_bucket_name`. rclone's B2 backend creates a bucket when its own
// lookup does not find it, so that 400 means "the name exists and it is not
// yours to create": the key cannot see the bucket the remote was configured
// with. The raw rclone output is already in the server log (run prints every
// line at debug level), so the run's error carries the cause and the fix rather
// than quoting it.
func duplicateBucketError(remote, objectName string) error {
	bucket := model.FirstSegment(objectName)
	if bucket == "" {
		bucket = "(unknown)"
	}
	return fmt.Errorf("rclone could not see bucket %q on remote %q, so it tried to create it and B2 refused "+
		"(duplicate_bucket_name): the key configured on that remote has no access to the bucket — "+
		"create a B2 application key with read and write access to %q and save its keyID and applicationKey "+
		"on the remote", bucket, remote, bucket)
}

// Delete removes a single remote object.
func (r *Rclone) Delete(ctx context.Context, remote, objectName string) error {
	_, err := r.run(ctx, "deletefile", nil, remote+":"+objectName)
	return err
}

// Move renames one object inside a remote with rclone moveto.
//
// On a single backend (B2, and a crypt wrapping it) moveto is a server-side copy
// followed by a server-side delete, so no bytes cross the wire twice — the wire
// already carried them once, for the upload that created the source. B2's copy
// is atomic: the destination name holds either the previous object or the new
// one, never a half-written blend, which is what lets the single-generation mode
// replace its only copy without a window where none exists.
//
// --retries 1 for the same reason as Upload: the next scheduled pass is the
// retry, and a refusal that is a property of the destination repeats identically.
func (r *Rclone) Move(ctx context.Context, remote, from, to string) error {
	_, err := r.run(ctx, "moveto", []string{"--retries", "1"}, remote+":"+from, remote+":"+to)
	return err
}

// Lsf returns the filenames (not paths) present in a remote directory.
func (r *Rclone) Lsf(ctx context.Context, remote, dir string) ([]string, error) {
	out, err := r.run(ctx, "lsf", nil, remote+":"+dir)
	if err != nil {
		return nil, err
	}
	var files []string
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		if line != "" {
			files = append(files, line)
		}
	}
	return files, nil
}

// run executes one rclone command and returns its stdout. op is the rclone
// subcommand, flags are extra arguments that must come before it (rclone
// accepts global flags anywhere, but the retry knob belongs to the call that
// needs it), and args are the command's own arguments.
func (r *Rclone) run(ctx context.Context, op string, flags []string, args ...string) (string, error) {
	// --log-level DEBUG ensures the backend-specific error (auth failure,
	// no-such-bucket, etc.) reaches stderr even when rclone's default NOTICE
	// level would swallow it. Output is captured and discarded on success;
	// on failure every line is printed individually before returning the error.
	full := []string{"--log-level", "DEBUG"}
	if r.config != "" {
		full = append(full, "--config", r.config)
	}
	full = append(full, flags...)
	full = append(full, op)
	full = append(full, args...)
	cmd := exec.CommandContext(ctx, r.bin, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		// Collect both streams; some backends write the specific error to stdout.
		detail := strings.TrimSpace(stderr.String())
		if s := strings.TrimSpace(stdout.String()); s != "" {
			if detail != "" {
				detail += "\n" + s
			} else {
				detail = s
			}
		}
		// Log each line separately so structured logs don't bury the cause.
		for _, line := range strings.Split(detail, "\n") {
			if line != "" {
				fmt.Fprintf(os.Stderr, "rclone: %s\n", line)
			}
		}
		return "", fmt.Errorf("rclone %s: %w: %s", op, err, detail)
	}
	return stdout.String(), nil
}
