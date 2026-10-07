package tmux

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"
)

// localeNames are the variables a process reads its locale from: LC_ALL
// overrides the rest, LC_CTYPE, the character set, overrides LANG, and
// each other category overrides LANG for itself. The categories glibc
// has beyond POSIX's are among them.
var localeNames = []string{
	"LC_ALL", "LC_CTYPE", "LANG",
	"LC_COLLATE", "LC_MESSAGES", "LC_MONETARY", "LC_NUMERIC", "LC_TIME",
	"LC_ADDRESS", "LC_IDENTIFICATION", "LC_MEASUREMENT", "LC_NAME", "LC_PAPER", "LC_TELEPHONE",
}

var localeMu sync.Mutex

// ensureLocale gives the managed server's global environment, which
// every new session and pane starts its process with, a UTF-8 locale.
// The server took that environment from the process that started it,
// the daemon at a cold start, and LANG and LC_* are not among the
// variables a client updates a session with; the client that makes a
// session is the daemon anyway. So a daemon run over ssh without a
// locale forwarded, or as a service without LANG, left every agent in
// the C locale, although a pane is a UTF-8 terminal whatever the
// locale. new-session -e would not do: it can set a variable for the
// session but not remove one, and an LC_ALL=C the server was started
// with overrides any LANG.
func (s Server) ensureLocale(ctx context.Context) error {
	// One call at a time, from the read to the write: the changes are
	// worked out against what was read, and two calls that read the
	// same environment, one of them with a probe that failed and the
	// other with its retry, would each write half of a different
	// locale.
	localeMu.Lock()
	defer localeMu.Unlock()
	out, err := s.Run(ctx, "show-environment", "-g")
	if err != nil {
		return err
	}
	// A variable marked for removal prints as -NAME, with no =.
	global := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		if k, v, ok := strings.Cut(line, "="); ok {
			global[k] = v
		}
	}
	// The host is asked where the panes look locales up, which is in
	// the server's environment: the variables that move the locale
	// files, glibc's LOCPATH, a Nix profile's LOCALE_ARCHIVE and its
	// versioned name, macOS's PATH_LOCALE, as the server has them.
	var lookup []string
	for _, k := range []string{"LOCPATH", "LOCALE_ARCHIVE", "LOCALE_ARCHIVE_2_27", "PATH_LOCALE"} {
		if v, ok := global[k]; ok {
			lookup = append(lookup, k+"="+v)
		}
	}
	utf8 := func(k, v string) bool { return utf8Locale(lookup, k, v) }
	want := wantLocale(os.Getenv, func(k string) string { return global[k] }, utf8, func() string { return fallbackLocale(lookup) })
	if want == nil {
		return nil
	}
	var args []string
	for _, k := range localeNames {
		v, set := want[k]
		g, had := global[k]
		switch {
		case set && (!had || g != v):
			args = append(args, Next, "set-environment", "-g", k, v)
		case !set && had:
			args = append(args, Next, "set-environment", "-gu", k)
		}
	}
	if len(args) == 0 {
		return nil
	}
	// One sequence, which tmux runs to completion once submitted, so a
	// session made meanwhile, by hand or by another process, never
	// starts in a locale halfway through the changes.
	_, err = s.Run(ctx, args[1:]...)
	return err
}

// wantLocale is the locale the managed server's global environment is
// given, by variable, or nil to leave it as it is. keptLocale drops the
// variables the host does not take, and a locale counts when what is
// left has a UTF-8 character set. The daemon's own locale is taken when
// it counts. Otherwise a server whose locale counts keeps it: one
// started from the user's shell, or by a daemon that had one before it
// was restarted without. Otherwise the locale is LANG alone, set to the
// host's fallback.
func wantLocale(daemon, global func(string) string, utf8 func(k, v string) bool, fallback func() string) map[string]string {
	if d := keptLocale(daemon, utf8); utf8Kept(d) {
		return d
	}
	if g := keptLocale(global, utf8); utf8Kept(g) {
		return g
	}
	if fb := fallback(); fb != "" {
		return map[string]string{"LANG": fb}
	}
	return nil
}

// keptLocale is the locale variables of env that the host takes: C,
// POSIX, and values utf8 says are UTF-8 locales the host has for them.
// setlocale(LC_ALL, "") takes all of them or none, so one category that
// names a locale the host lacks, an LC_TIME forwarded by ssh from a
// machine that has it say, would leave a program in C. A locale with
// another character set is dropped too: tmux reads what a program
// writes as UTF-8, whatever its locale.
func keptLocale(env func(string) string, utf8 func(k, v string) bool) map[string]string {
	kept := map[string]string{}
	for _, k := range localeNames {
		if v := env(k); v == "C" || v == "POSIX" || v != "" && utf8(k, v) {
			kept[k] = v
		}
	}
	return kept
}

// utf8Kept is whether variables keptLocale kept have a UTF-8 character
// set: the first of LC_ALL, LC_CTYPE and LANG that is set, as setlocale
// reads them, is set to something else than C or POSIX, which leaves a
// UTF-8 locale.
func utf8Kept(vars map[string]string) bool {
	for _, k := range []string{"LC_ALL", "LC_CTYPE", "LANG"} {
		if v := vars[k]; v != "" {
			return v != "C" && v != "POSIX"
		}
	}
	return false
}

// fallbackLocale is the locale given to a server and daemon that have
// no UTF-8 one, as pickFallback picks it on this host, with the locale
// files lookup names.
func fallbackLocale(lookup []string) string {
	return pickFallback(func(name string) bool { return utf8Locale(lookup, "LANG", name) }, func() []string { return hostLocales(lookup) })
}

// pickFallback is C.UTF-8, which differs from the C locale in the
// character set alone, where the host has it; else en_US.UTF-8; else
// the first UTF-8 locale the host lists. "" when the host has none: a
// locale it does not have would make programs in the pane warn about
// it and run in C anyway.
func pickFallback(utf8 func(string) bool, listed func() []string) string {
	for _, name := range []string{"C.UTF-8", "en_US.UTF-8"} {
		if utf8(name) {
			return name
		}
	}
	for _, name := range listed() {
		if utf8Name(name) && utf8(name) {
			return name
		}
	}
	return ""
}

// utf8Locale reports whether a process whose locale variable k is name,
// and that has no other, has the UTF-8 character set on this host, as
// the host's own locale(1) says with the locale files lookup names.
// Only the host's libc knows which names it takes: glibc takes
// en_US.utf8 for en_US.UTF-8 and macOS does not, and either takes a
// UTF-8 name it has no locale for as C. LC_CTYPE is asked for as
// LC_CTYPE, the character set alone, which macOS has a bare UTF-8 for;
// any other variable as LC_ALL, every category, so a bare UTF-8 does
// not count there. Where there is no locale to run, as on musl without
// musl-locales, the name's spelling decides. A locale that fails
// otherwise, by a timeout say, gives no answer, and the name does not
// count until it is asked again.
func utf8Locale(lookup []string, k, name string) bool {
	if name == "" {
		return false
	}
	if k != "LC_CTYPE" {
		k = "LC_ALL"
	}
	cs, err := charmap(lookup, k, name)
	switch {
	case errors.Is(err, exec.ErrNotFound):
		return utf8Name(name)
	case err != nil:
		return false
	}
	return cs == "UTF-8"
}

// utf8Name is a locale name whose character set, between the . and any
// @, is UTF-8 in any of its spellings.
func utf8Name(name string) bool {
	_, cs, ok := strings.Cut(name, ".")
	cs, _, _ = strings.Cut(cs, "@")
	cs = strings.ToLower(strings.NewReplacer("-", "", "_", "").Replace(cs))
	return ok && cs == "utf8"
}

var charmaps = struct {
	sync.Mutex
	m map[string]charmapResult
}{m: map[string]charmapResult{}}

type charmapResult struct {
	cs  string
	err error
}

// charmap is the character set `locale charmap` prints with k set to
// name, the lookup variables, and nothing else in its environment: no
// other locale variable of the daemon's, and none of its lookup
// variables either, since the panes have the server's. glibc's locale
// sets LC_CTYPE on its own before it sets every category, and says on
// stderr, and nowhere else, that one of them failed: a locale with a
// character set but a category missing would print UTF-8 and still
// leave a program in C. Anything on stderr makes the answer nothing.
// An answer, or a locale that is not there, is kept for the process:
// a locale generated after the daemon started counts from its next
// start. Any other failure is asked again next time.
func charmap(lookup []string, k, name string) (string, error) {
	charmaps.Lock()
	defer charmaps.Unlock()
	env := append(slices.Clip(lookup), k+"="+name)
	key := strings.Join(env, "\x00")
	if r, ok := charmaps.m[key]; ok {
		return r.cs, r.err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "locale", "charmap")
	cmd.Env = env
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	r := charmapResult{cs: strings.TrimSpace(string(out)), err: err}
	if stderr.Len() > 0 {
		r.cs = ""
	}
	if err == nil || errors.Is(err, exec.ErrNotFound) {
		charmaps.m[key] = r
	}
	return r.cs, r.err
}

// hostLocales is the host's locales as locale -a lists them with the
// lookup variables; empty when it cannot be run.
func hostLocales(lookup []string) []string {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "locale", "-a")
	cmd.Env = append([]string{}, lookup...) // never nil, the daemon's
	out, err := cmd.Output()
	if err != nil {
		return nil
	}
	return strings.Fields(string(out))
}
