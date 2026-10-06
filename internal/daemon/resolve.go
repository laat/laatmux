package daemon

import (
	"path/filepath"
	"sync"
	"time"
)

// resolver is the daemon's cache of paths with their symlinks resolved,
// for attributing panes to worktree roots. The file system is asked on
// a goroutine of its own per path, so a path on a hung mount costs that
// goroutine and never the poll; until the answer is there the path is
// taken cleaned. The cache has a lock of its own and takes no other, so
// resolve may be called with or without d.mu held: the poll calls it
// before taking d.mu, the attribution test under it.
type resolver struct {
	mu        sync.Mutex
	resolved  map[string]resolution
	resolving map[string]bool // the paths being asked for now
}

func newResolver() *resolver {
	return &resolver{resolved: map[string]resolution{}, resolving: map[string]bool{}}
}

// The resolved-path cache: its size, how many resolutions may be in
// flight, and how long a resolution stands before it is made again, in
// the background, the old one standing meanwhile.
const (
	maxResolved  = 4096
	maxResolving = 64
	resolveTTL   = time.Minute
)

// resolution is a path with its symlinks resolved, and when that was.
type resolution struct {
	real string
	at   time.Time
}

// resolve is the path with its symlinks resolved as last seen, and the
// path cleaned until it has been: the file system is asked on a
// goroutine of its own, so a path on a hung mount costs that goroutine
// and never the poll, and the answer is there by a later poll. A path
// that does not resolve, gone or unreadable, is taken as it is.
func (r *resolver) resolve(path string) string {
	if path == "" {
		return ""
	}
	clean := filepath.Clean(path)
	r.mu.Lock()
	defer r.mu.Unlock()
	res, ok := r.resolved[path]
	if (!ok || time.Since(res.at) > resolveTTL) && !r.resolving[path] && len(r.resolving) < maxResolving {
		r.resolving[path] = true
		go func() {
			real := clean
			if rr, err := filepath.EvalSymlinks(clean); err == nil {
				real = rr
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			delete(r.resolving, path)
			if _, ok := r.resolved[path]; !ok && len(r.resolved) >= maxResolved {
				r.evictLocked()
			}
			r.resolved[path] = resolution{real: real, at: time.Now()}
		}()
	}
	if ok {
		return res.real
	}
	return clean
}

// evictLocked makes room in the full cache: the entries no poll has
// asked for in a while go, those of the panes there are now being
// refreshed within a TTL; failing that, one entry goes. Never all, which
// would have every path answered cleaned for a poll, and a symlinked
// pane lose its worktree for it. Called with r.mu held.
func (r *resolver) evictLocked() {
	for p, res := range r.resolved {
		if time.Since(res.at) > 3*resolveTTL {
			delete(r.resolved, p)
		}
	}
	for p := range r.resolved {
		if len(r.resolved) < maxResolved {
			return
		}
		delete(r.resolved, p)
	}
}
