package config

import (
	"net/url"
	"strings"
)

// SourceKey is a repository source's identity. The usual forms of one
// hosted repository give the same key: git@host:owner/repo,
// ssh://git@host/owner/repo and https://host/owner/repo, each with or
// without .git and a trailing slash. The host is compared without case,
// user and port are left out, since they choose a transport and not a
// repository, and the path is compared as written. Any other source,
// a local path or a file URL say, is its own key.
func SourceKey(source string) string {
	host, path, ok := hosted(source)
	if !ok {
		return source
	}
	return "hosted:" + strings.ToLower(host) + "/" + path
}

// SameSource reports whether two sources name one repository.
func SameSource(a, b string) bool { return a == b || SourceKey(a) == SourceKey(b) }

// hosted splits a hosted repository's source into host and path, the
// path without its leading slash, a trailing slash or .git.
func hosted(source string) (host, path string, ok bool) {
	if i := strings.Index(source, "://"); i >= 0 {
		switch strings.ToLower(source[:i]) {
		case "ssh", "git+ssh", "ssh+git", "https", "http", "git":
		default:
			return "", "", false
		}
		u, err := url.Parse(source)
		if err != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" {
			return "", "", false
		}
		host, path = u.Hostname(), u.Path
	} else {
		// git's scp-like syntax: a colon before any slash, and a host
		// before the colon. A path with a slash before its first colon
		// is local.
		// A single letter before it is a drive, as git takes it.
		i := strings.IndexByte(source, ':')
		if i <= 1 || strings.Contains(source[:i], "/") {
			return "", "", false
		}
		host, path = source[:i], source[i+1:]
		if j := strings.LastIndexByte(host, '@'); j >= 0 {
			host = host[j+1:]
		}
		if host == "" || strings.HasPrefix(host, "[") {
			return "", "", false
		}
	}
	path = strings.TrimSuffix(strings.Trim(path, "/"), ".git")
	path = strings.TrimSuffix(path, "/")
	if path == "" || strings.HasPrefix(path, "~") {
		return "", "", false
	}
	return host, path, true
}
