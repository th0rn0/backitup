// Package sshutil holds SSH client helpers shared by the backup modes: loading
// the client's private key and building a host-key verification callback.
package sshutil

import (
	"fmt"
	"os"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
)

// LoadSigner reads and parses an OpenSSH private key file.
func LoadSigner(path string) (ssh.Signer, error) {
	pem, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read key %s: %w", path, err)
	}
	signer, err := ssh.ParsePrivateKey(pem)
	if err != nil {
		return nil, fmt.Errorf("parse key %s: %w", path, err)
	}
	return signer, nil
}

// HostKeyCallback returns a host-key verifier. With insecure=true it accepts any
// host key (dev/test ONLY — logged by callers). Otherwise it verifies against
// the known_hosts file, which must exist and contain the server's key.
func HostKeyCallback(knownHostsPath string, insecure bool) (ssh.HostKeyCallback, error) {
	if insecure {
		return ssh.InsecureIgnoreHostKey(), nil //nolint:gosec // explicit dev opt-out
	}
	if knownHostsPath == "" {
		return nil, fmt.Errorf("host-key verification requires a known_hosts file (or set insecure)")
	}
	cb, err := knownhosts.New(knownHostsPath)
	if err != nil {
		return nil, fmt.Errorf("load known_hosts %s: %w", knownHostsPath, err)
	}
	return cb, nil
}
