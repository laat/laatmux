package config

import (
	"net/url"
	"strings"
)

// SourceKey is a repository source's identity. The forms a forge gives
// one repository get the same key: git@host:owner/repo,
// ssh://git@host/owner/repo and https://host/owner/repo, each with or
// without .git and a trailing slash, the host compared without case.
// Only the forge convention is unified: ssh as the user git on the
// default port, with a path relative to the forge's root, and http or
// https on the default port, whose user is a credential. git+ssh:// and
// ssh+git:// are git's other spellings of ssh:// and count as it. Any other
// source is its own key, since there a user, a port or a leading slash
// can name another repository: alice@box:proj and bob@box:proj are two
// home directories, and two ports can be two servers.
func SourceKey(source string) string {
	host, path, ok := forge(source)
	if !ok {
		// Its own key, in a space of its own: a source could spell
		// another's key.
		return "exact\x00" + source
	}
	return "forge\x00" + strings.ToLower(host) + "/" + path
}

// SameSource reports whether two sources name one repository.
func SameSource(a, b string) bool { return a == b || SourceKey(a) == SourceKey(b) }

// forge splits a source in one of the forge forms into host and path,
// the path without a trailing slash or .git.
func forge(source string) (host, path string, ok bool) {
	if i := strings.Index(source, "://"); i >= 0 {
		u, err := url.Parse(source)
		if err != nil || u.Hostname() == "" || u.RawQuery != "" || u.Fragment != "" || u.Opaque != "" {
			return "", "", false
		}
		switch strings.ToLower(u.Scheme) {
		case "ssh", "git+ssh", "ssh+git":
			if u.User == nil || u.User.Username() != "git" || (u.Port() != "" && u.Port() != "22") {
				return "", "", false
			}
		case "https":
			if u.Port() != "" && u.Port() != "443" {
				return "", "", false
			}
		case "http":
			if u.Port() != "" && u.Port() != "80" {
				return "", "", false
			}
		default:
			return "", "", false
		}
		host, path = u.Hostname(), strings.TrimPrefix(u.Path, "/")
	} else {
		// git's scp-like syntax: user@host:path, a colon before any
		// slash. Only the user git counts, with a path relative to the
		// forge's root: an absolute path or one from a home directory
		// is the server's own.
		at := strings.IndexByte(source, '@')
		colon := strings.IndexByte(source, ':')
		if at < 0 || colon < at || strings.Contains(source[:colon], "/") || source[:at] != "git" {
			return "", "", false
		}
		host, path = source[at+1:colon], source[colon+1:]
		if host == "" || strings.HasPrefix(host, "[") || strings.HasPrefix(path, "/") || strings.HasPrefix(path, "~") {
			return "", "", false
		}
	}
	path = strings.TrimSuffix(strings.TrimSuffix(path, "/"), ".git")
	if path == "" || strings.HasPrefix(path, "/") || strings.HasPrefix(path, "~") || strings.Contains(path, "//") {
		return "", "", false
	}
	return host, path, true
}
