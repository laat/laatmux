package tmux

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"os/user"
	"runtime"
	"strings"
	"sync"
)

// LoginShell is the user's login shell from the password database: on
// macOS what dscl reads for the user, elsewhere the user's /etc/passwd
// line, else getent; "" when none says. The daemon's own SHELL is not
// it: a daemon started from a sandboxed or a scripted shell carries
// that shell's, and a managed server started by it would give every
// new pane bash on a zsh machine.
var LoginShell = sync.OnceValue(func() string {
	u, err := user.Current()
	if err != nil || u.Username == "" {
		return ""
	}
	if runtime.GOOS == "darwin" {
		out, err := exec.Command("dscl", ".", "-read", "/Users/"+u.Username, "UserShell").Output()
		if err == nil {
			if _, v, ok := strings.Cut(strings.TrimSpace(string(out)), "UserShell:"); ok {
				return strings.TrimSpace(v)
			}
		}
	}
	if data, err := os.ReadFile("/etc/passwd"); err == nil {
		if sh := shellFromPasswd(data, u.Username); sh != "" {
			return sh
		}
	}
	if out, err := exec.Command("getent", "passwd", u.Username).Output(); err == nil {
		return shellFromPasswd(out, u.Username)
	}
	return ""
})

// shellFromPasswd is the shell field of name's line in passwd data, ""
// when the line is missing or short.
func shellFromPasswd(data []byte, name string) string {
	for _, line := range bytes.Split(data, []byte("\n")) {
		f := strings.Split(string(line), ":")
		if len(f) >= 7 && f[0] == name {
			return strings.TrimSpace(f[6])
		}
	}
	return ""
}

// ensureShell gives the managed server the user's login shell as its
// default-shell and as SHELL in the global environment, so a session
// made with no command, and a pane's programs asking $SHELL, get the
// shell the user has and not the daemon's. Nothing when the login
// shell is not known.
func (s Server) ensureShell(ctx context.Context) error {
	sh := LoginShell()
	if sh == "" {
		return nil
	}
	_, err := s.Run(ctx, "set-option", "-g", "default-shell", sh,
		Next, "set-environment", "-g", "SHELL", sh)
	return err
}
