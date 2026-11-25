//go:build linux

package archiveutil

import (
	"strings"

	"golang.org/x/sys/unix"
)

// xattr is one extended attribute read off a directory entry.
type xattr struct {
	name  string
	value string
}

// readUserXattrs returns the `user.*` extended attributes of path, without
// following a symlink. A filesystem or file type that cannot hold them (or a
// permission the process does not have — the `user.` namespace is the only one
// an unprivileged process may read on ext4, which refuses symlinks and FIFOs)
// yields nothing rather than an error: the caller is archiving content, and an
// absent attribute is not a failure.
//
// Only the `user.` namespace is carried. That is the namespace rsync's
// --fake-super writes the source's ownership into (`user.rsync.%stat`), and the
// only one an unprivileged process may both read here and set again on restore;
// `security.*` and `trusted.*` are unreadable to the app's uid, and ACLs are
// carried by tar as ACL records rather than as system.posix_acl_access.
func readUserXattrs(path string) []xattr {
	names, err := listXattrs(path)
	if err != nil {
		return nil
	}
	var out []xattr
	for _, name := range names {
		if !strings.HasPrefix(name, "user.") {
			continue
		}
		value, err := getXattr(path, name)
		if err != nil {
			continue
		}
		out = append(out, xattr{name: name, value: string(value)})
	}
	return out
}

// listXattrs mirrors llistxattr(2), growing the buffer when the attribute set
// changed size between the sizing call and the read.
func listXattrs(path string) ([]string, error) {
	for size := 256; ; size *= 4 {
		buf := make([]byte, size)
		n, err := unix.Llistxattr(path, buf)
		switch {
		case err == unix.ERANGE && size < 1<<20:
			continue // more names than fit: try a bigger buffer
		case err != nil:
			return nil, err
		case n > size:
			continue
		}
		return splitNulTerminated(buf[:n]), nil
	}
}

// getXattr mirrors lgetxattr(2) with the same growth loop.
func getXattr(path, name string) ([]byte, error) {
	for size := 256; ; size *= 4 {
		buf := make([]byte, size)
		n, err := unix.Lgetxattr(path, name, buf)
		switch {
		case err == unix.ERANGE && size < 1<<20:
			continue
		case err != nil:
			return nil, err
		case n > size:
			continue
		}
		return buf[:n], nil
	}
}

func splitNulTerminated(b []byte) []string {
	var out []string
	start := 0
	for i, c := range b {
		if c == 0 {
			if i > start {
				out = append(out, string(b[start:i]))
			}
			start = i + 1
		}
	}
	return out
}
