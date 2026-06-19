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

func (r *Rclone) run(ctx context.Context, args ...string) (string, error) {
	full := args
	if r.config != "" {
		full = append([]string{"--config", r.config}, args...)
	}
	cmd := exec.CommandContext(ctx, r.bin, full...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("rclone %s: %w: %s", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
