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
	// Durations are whole seconds to ssh; a fraction is rounded up.
	ConnectTimeout time.Duration
	// KeepAlive is ServerAliveInterval, and KeepAliveCount
	// ServerAliveCountMax: a silent network loss becomes an ssh exit
	// after KeepAlive times KeepAliveCount, so the caller sees EOF
	// rather than a frozen connection. A zero KeepAlive leaves ssh's
	// default, none; a zero count leaves ssh's, three.
	KeepAlive      time.Duration
	KeepAliveCount int
}

// SSH is the argv of ssh to the alias running command: the options in a
// fixed order, --, the alias, the command as one argument for the remote
// shell. ssh reads no option after --, so neither the alias nor the
// command is read as one; without it ssh reads options after the alias
// as well as before.
func SSH(alias string, o SSHOptions, command string) []string {
	argv := []string{"ssh"}
	if o.TTY {
		argv = append(argv, "-t")
	} else {
		argv = append(argv, "-T", "-o", "BatchMode=yes")
	}
	if o.ConnectTimeout > 0 {
		argv = append(argv, "-o", "ConnectTimeout="+seconds(o.ConnectTimeout))
	}
	if o.KeepAlive > 0 {
		argv = append(argv, "-o", "ServerAliveInterval="+seconds(o.KeepAlive))
		if o.KeepAliveCount > 0 {
			argv = append(argv, "-o", "ServerAliveCountMax="+strconv.Itoa(o.KeepAliveCount))
		}
	}
	return append(argv, "--", alias, command)
}

// seconds is a positive duration as ssh takes it: whole seconds,
// rounded up, so a fraction never becomes 0, which would mean none.
func seconds(d time.Duration) string {
	return strconv.Itoa(int((d + time.Second - 1) / time.Second))
}
