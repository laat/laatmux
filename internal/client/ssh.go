package client

import (
	"strconv"
	"time"
)

// SSHOptions is what differs between the ssh commands laatmux runs.
type SSHOptions struct {
	// TTY is an interactive command, attach or a shell: -t, and ssh may
	// ask for a password or a host key. Otherwise -T and BatchMode, so a
	// command run from a daemon or a script fails rather than prompts.
	TTY bool
	// ConnectTimeout bounds the connection; zero leaves ssh's default.
	ConnectTimeout time.Duration
	// KeepAlive, with KeepAliveCount, is ServerAliveInterval and
	// ServerAliveCountMax: a silent network loss becomes an ssh exit
	// after KeepAlive times KeepAliveCount, so the caller sees EOF
	// rather than a frozen connection. Zero leaves ssh's default, none.
	KeepAlive      time.Duration
	KeepAliveCount int
}

// SSH is the argv of ssh to the alias running command: the options in a
// fixed order, the alias, the command as one argument for the remote
// shell.
func SSH(alias string, o SSHOptions, command string) []string {
	argv := []string{"ssh"}
	if o.TTY {
		argv = append(argv, "-t")
	} else {
		argv = append(argv, "-T", "-o", "BatchMode=yes")
	}
	if o.ConnectTimeout > 0 {
		argv = append(argv, "-o", "ConnectTimeout="+strconv.Itoa(int(o.ConnectTimeout/time.Second)))
	}
	if o.KeepAlive > 0 {
		argv = append(argv, "-o", "ServerAliveInterval="+strconv.Itoa(int(o.KeepAlive/time.Second)),
			"-o", "ServerAliveCountMax="+strconv.Itoa(o.KeepAliveCount))
	}
	return append(argv, alias, command)
}
