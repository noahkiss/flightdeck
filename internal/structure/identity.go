package structure

import (
	"path"
	"strings"
)

// NormalizeRemote turns a git remote into the form that identifies a project: host/path, with
// no scheme, user, port or trailing .git, and the host in lower case. These all give
// "example.com/o/app":
//
//	https://example.com/o/app.git
//	ssh://git@Example.com:22/o/app
//	git@example.com:o/app.git
//
// The path keeps its case, since some hosts treat it as case-sensitive. A local path (or a
// file:// URL) stays a cleaned path.
func NormalizeRemote(remote string) string {
	r := strings.TrimSpace(remote)
	if r == "" {
		return ""
	}
	var host, p string
	if scheme, rest, ok := strings.Cut(r, "://"); ok {
		if strings.EqualFold(scheme, "file") {
			return cleanPath(rest)
		}
		host, p, _ = strings.Cut(rest, "/")
		host = stripPort(stripUser(host))
	} else if i := strings.Index(r, ":"); i > 0 && !strings.Contains(r[:i], "/") {
		// scp-like: [user@]host:path
		host, p = stripUser(r[:i]), r[i+1:]
	} else {
		return cleanPath(r)
	}
	p = strings.TrimSuffix(strings.Trim(p, "/"), ".git")
	return strings.ToLower(host) + "/" + strings.Trim(p, "/")
}

func stripUser(host string) string {
	if i := strings.LastIndex(host, "@"); i >= 0 {
		return host[i+1:]
	}
	return host
}

func stripPort(host string) string {
	if i := strings.LastIndex(host, ":"); i >= 0 && !strings.Contains(host[i:], "]") {
		return host[:i]
	}
	return host
}

func cleanPath(p string) string {
	return strings.TrimSuffix(path.Clean(p), ".git")
}
