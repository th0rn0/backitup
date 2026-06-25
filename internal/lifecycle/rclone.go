package lifecycle

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
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
	if _, err := r.run(ctx, "copyto", localPath, remote+":"+objectName); err != nil {
		return 0, err
	}
	fi, err := os.Stat(localPath)
	if err != nil {
		return 0, err
	}
	return fi.Size(), nil
}

// Delete removes a single remote object.
func (r *Rclone) Delete(ctx context.Context, remote, objectName string) error {
	_, err := r.run(ctx, "deletefile", remote+":"+objectName)
	return err
}

// Lsf returns the filenames (not paths) present in a remote directory.
func (r *Rclone) Lsf(ctx context.Context, remote, dir string) ([]string, error) {
	out, err := r.run(ctx, "lsf", remote+":"+dir)
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

func (r *Rclone) run(ctx context.Context, args ...string) (string, error) {
	// --log-level DEBUG ensures the backend-specific error (auth failure,
	// no-such-bucket, etc.) reaches stderr even when rclone's default NOTICE
	// level would swallow it. Output is captured and discarded on success;
	// on failure every line is printed individually before returning the error.
	full := []string{"--log-level", "DEBUG"}
	if r.config != "" {
		full = append(full, "--config", r.config)
	}
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
		return "", fmt.Errorf("rclone %s: %w: %s", args[0], err, detail)
	}
	return stdout.String(), nil
}
