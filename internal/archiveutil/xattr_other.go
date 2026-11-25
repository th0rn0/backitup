//go:build !linux

package archiveutil

// xattr exists so the tar builder compiles on the platforms backitup does not
// ship for (the server and the client image are both Linux). Nothing is read,
// which is the same shape as a Linux filesystem that cannot hold xattrs.
type xattr struct {
	name  string
	value string
}

func readUserXattrs(path string) []xattr { return nil }
