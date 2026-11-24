package model

import "strings"

// ObjectDir joins a remote's root — its bucket plus optional prefix,
// Remote.RootPath — with the directory a client's objects live in.
//
// A directory that merely repeats the bucket is dropped. The offsite worker
// names a client's directory after its slug, so a client named like its bucket
// would write to "<bucket>/<bucket>/<snapshot>"; the bucket decorates nothing
// there and makes the destination hard to read. One rule, used by the upload
// path, the verification listing and the download/delete routes, so all three
// address the same object.
//
// An absolute root (the test-only local backend addresses a directory) keeps its
// leading slash.
func ObjectDir(root, dir string) string {
	if b := FirstSegment(root); b != "" && strings.Trim(dir, "/") == b {
		dir = ""
	}
	parts := make([]string, 0, 2)
	for _, p := range []string{root, dir} {
		if p = strings.Trim(p, "/"); p != "" {
			parts = append(parts, p)
		}
	}
	p := strings.Join(parts, "/")
	if strings.HasPrefix(root, "/") && p != "" {
		p = "/" + p
	}
	return p
}

// ObjectPath is ObjectDir plus the object's own file name:
// "<root>/<dir>/<name>", with an empty root or dir simply omitted.
func ObjectPath(root, dir, name string) string {
	if d := ObjectDir(root, dir); d != "" {
		return d + "/" + name
	}
	return name
}

// FirstSegment returns the first path segment of p — the bucket name of a remote
// root such as "<bucket>[/<prefix>]" — or "" when p is empty. It is the name
// rclone addresses a bucket by.
func FirstSegment(p string) string {
	p = strings.Trim(p, "/")
	if i := strings.IndexByte(p, '/'); i >= 0 {
		return p[:i]
	}
	return p
}
