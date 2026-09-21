// laatmux: git worktrees and coding agents across hosts, from tmux.
//
// Milestone one: status daemon, sidebar, launcher, jump.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/laat/laatmux/internal/client"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/daemon"
	"github.com/laat/laatmux/internal/detect"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/procs"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
)

var version = "0.0.0-dev"

func main() {
	if len(os.Args) < 2 {
		usage()
		os.Exit(2)
	}
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()
	var err error
	switch os.Args[1] {
	case "serve":
		err = cmdServe(ctx, os.Args[2:])
	case "bridge":
		err = client.Bridge(ctx, os.Stdin, os.Stdout)
	case "new":
		err = cmdNew(ctx, os.Args[2:])
	case "ls":
		err = cmdLs(ctx, os.Args[2:])
	case "watch":
		err = cmdWatch(ctx, os.Args[2:])
	case "jump":
		err = cmdJump(ctx, os.Args[2:])
	case "hosts":
		err = cmdHosts(ctx, os.Args[2:])
	case "explain":
		err = cmdExplain(ctx, os.Args[2:])
	case "version", "--version":
		fmt.Println("laatmux", version, "protocol", protocol.Version)
	case "help", "-h", "--help":
		usage()
	default:
		usage()
		err = fmt.Errorf("unknown command %q", os.Args[1])
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		fmt.Fprintln(os.Stderr, "laatmux:", err)
		os.Exit(1)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: laatmux <command> [flags]

  serve     run the per-host daemon (polls tmux, serves status)
  bridge    connect stdio to the local daemon (what ssh runs on a remote host)
  new       laatmux new <name> [--host h] --cwd <path> [-- <cmd>...]
  ls        list agents across configured hosts
  watch     live list, redraws on change (sidebar)
  jump      laatmux jump <host>/<session>   focus or open a pane attached to it
  hosts     reachability and daemon version per host
  explain   laatmux explain <pane-id>       show detection inputs and decision
  version

config: ~/.config/laatmux/config.yaml   state: $LAATMUX_HOME or ~/.local/state/laatmux
`)
}

func tmuxServerFlag(fs *flag.FlagSet) *string {
	return fs.String("tmux-socket", "laatmux", `tmux server: a -L name, "default" for the default server, or a -S path`)
}

func parseServer(v string) tmux.Server {
	switch {
	case v == "default" || v == "":
		return tmux.Server{}
	case strings.Contains(v, "/"):
		return tmux.Server{Path: v}
	default:
		return tmux.Server{Name: v}
	}
}

// ---- serve ---------------------------------------------------------------

func cmdServe(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	listen := fs.String("listen", "unix:"+home.DefaultSocket(), "unix:<path> or tcp:<host:port>")
	sock := tmuxServerFlag(fs)
	interval := fs.Duration("interval", daemon.DefaultInterval, "poll interval")
	lines := fs.Int("capture-lines", daemon.DefaultCapture, "screen lines captured per pane")
	if err := fs.Parse(args); err != nil {
		return err
	}
	lock, err := home.TryLock()
	if err != nil {
		return err
	}
	defer lock.Release()
	envID, err := home.EnvironmentID()
	if err != nil {
		return err
	}
	network, addr, ok := strings.Cut(*listen, ":")
	if !ok {
		return fmt.Errorf("bad --listen %q", *listen)
	}
	if network == "unix" {
		os.Remove(addr)
	}
	ln, err := net.Listen(network, addr)
	if err != nil {
		return err
	}
	defer ln.Close()
	if network == "unix" {
		os.Chmod(addr, 0o600)
		defer os.Remove(addr)
	}
	hostname, _ := os.Hostname()
	rt := home.Runtime{Address: network + ":" + ln.Addr().String(), PID: os.Getpid(), Version: version, EnvironmentID: envID, StartedAt: time.Now()}
	if err := home.WriteRuntime(rt); err != nil {
		return err
	}
	defer home.RemoveRuntime(os.Getpid())

	logger := log.New(os.Stderr, "laatmux ", log.LstdFlags)
	logger.Printf("serve %s env=%s tmux=%q listen=%s", version, envID, *sock, rt.Address)
	d := daemon.New(daemon.Config{
		Server: parseServer(*sock), Interval: *interval, CaptureLines: *lines,
		EnvironmentID: envID, Host: hostname, Version: version, Logger: logger,
	})
	errc := make(chan error, 2)
	go func() { errc <- d.Run(ctx) }()
	go func() { errc <- d.Serve(ctx, ln) }()
	select {
	case <-ctx.Done():
		return nil
	case err := <-errc:
		return err
	}
}

// ---- new -----------------------------------------------------------------

func cmdNew(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("new", flag.ContinueOnError)
	host := fs.String("host", "", "host name from config; default local")
	cwd := fs.String("cwd", "", "working directory on the host (required)")
	// Flags may come before or after the name: flag.Parse stops at the first
	// non-flag, so parse once for leading flags and again for trailing ones.
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: laatmux new <name> [--host h] --cwd <path> [-- <cmd>...]")
	}
	name := fs.Arg(0)
	if err := fs.Parse(fs.Args()[1:]); err != nil {
		return err
	}
	cmd := fs.Args()
	if *cwd == "" {
		return errors.New("--cwd is required")
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	h, ok := cfg.Find(*host)
	if !ok {
		return fmt.Errorf("unknown host %q", *host)
	}
	c, err := client.Dial(ctx, h)
	if err != nil {
		return err
	}
	defer c.Close()
	if !protocol.Has(c.Hello.Capabilities, protocol.CapNew) {
		return fmt.Errorf("%s: daemon %s does not support new", h.Name, c.Hello.Version)
	}
	res, err := c.Request(ctx, protocol.Message{Type: protocol.TypeNew, Name: name, Cwd: *cwd, Cmd: cmd, Host: h.Name})
	if err != nil {
		return err
	}
	fmt.Printf("%s/%s %s\n", h.Name, res.Session, res.PaneID)
	return nil
}

// ---- ls / watch ------------------------------------------------------------

// merged is the client-side merge of every host's stream. Host connectivity
// is a separate axis from agent state and lives here, not in the records.
type merged struct {
	mu     sync.Mutex
	agents map[string]protocol.Agent // by agent id
	hosts  map[string]hostState      // by host name
	byHost map[string]string         // agent id -> host name
	change chan struct{}
}

type hostState struct {
	Connected bool
	Error     string
	Version   string
	EnvID     string
	Since     time.Time
}

func newMerged() *merged {
	return &merged{agents: map[string]protocol.Agent{}, hosts: map[string]hostState{}, byHost: map[string]string{}, change: make(chan struct{}, 1)}
}

func (m *merged) notify() {
	select {
	case m.change <- struct{}{}:
	default:
	}
}

func (m *merged) setHost(name string, st hostState) {
	m.mu.Lock()
	st.Since = time.Now()
	m.hosts[name] = st
	m.mu.Unlock()
	m.notify()
}

func (m *merged) apply(host string, msg protocol.Message) {
	m.mu.Lock()
	switch msg.Type {
	case protocol.TypeSnapshot:
		for id, h := range m.byHost {
			if h == host {
				delete(m.agents, id)
				delete(m.byHost, id)
			}
		}
		for _, a := range msg.Agents {
			m.agents[a.ID] = a
			m.byHost[a.ID] = host
		}
	case protocol.TypeUpsert:
		if msg.Agent != nil {
			m.agents[msg.Agent.ID] = *msg.Agent
			m.byHost[msg.Agent.ID] = host
		}
	case protocol.TypeRemove:
		delete(m.agents, msg.AgentID)
		delete(m.byHost, msg.AgentID)
	}
	m.mu.Unlock()
	m.notify()
}

// follow keeps one host subscribed, reconnecting with backoff. Cached
// agents stay visible while disconnected; the host row says so.
func (m *merged) follow(ctx context.Context, h client.Host) {
	backoff := time.Second
	for ctx.Err() == nil {
		c, err := client.Dial(ctx, h)
		switch {
		case err != nil:
			m.setHost(h.Name, hostState{Error: err.Error()})
		case !protocol.Has(c.Hello.Capabilities, protocol.CapStatus):
			c.Close()
			m.setHost(h.Name, hostState{Error: "daemon " + c.Hello.Version + " has no status capability"})
		default:
			m.setHost(h.Name, hostState{Connected: true, Version: c.Hello.Version, EnvID: c.Hello.EnvironmentID})
			backoff = time.Second
			stop := c.CloseOnDone(ctx)
			if err := c.Write(protocol.Message{Type: protocol.TypeSubscribe}); err == nil {
				for {
					msg, err := c.Read()
					if err != nil {
						break
					}
					m.apply(h.Name, msg)
				}
			}
			stop()
			c.Close()
			m.setHost(h.Name, hostState{Error: "disconnected"})
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func activityRank(a protocol.Activity) int {
	switch a {
	case protocol.Blocked:
		return 0
	case protocol.Working:
		return 1
	case protocol.Idle:
		return 2
	default:
		return 3
	}
}

func (m *merged) render() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	var b strings.Builder
	hostNames := make([]string, 0, len(m.hosts))
	for n := range m.hosts {
		hostNames = append(hostNames, n)
	}
	sort.Strings(hostNames)
	for _, n := range hostNames {
		st := m.hosts[n]
		if st.Connected {
			fmt.Fprintf(&b, "%s  connected  %s\n", n, st.Version)
		} else {
			fmt.Fprintf(&b, "%s  DOWN  %s\n", n, st.Error)
		}
	}
	agents := make([]protocol.Agent, 0, len(m.agents))
	for _, a := range m.agents {
		agents = append(agents, a)
	}
	sort.Slice(agents, func(i, j int) bool {
		ri, rj := activityRank(agents[i].Activity), activityRank(agents[j].Activity)
		if ri != rj {
			return ri < rj
		}
		return agents[i].ActivityAt.After(agents[j].ActivityAt)
	})
	if len(agents) > 0 {
		b.WriteString("\n")
	}
	now := time.Now()
	for _, a := range agents {
		host := m.byHost[a.ID]
		mark := " "
		switch a.Activity {
		case protocol.Blocked:
			mark = "!"
		case protocol.Working:
			mark = "*"
		case protocol.Idle:
			mark = "-"
		}
		live := ""
		if a.Liveness == protocol.Gone {
			live = " (gone)"
		}
		hs := m.hosts[host]
		if !hs.Connected {
			live += " (host down)"
		}
		agent := a.Agent
		if agent == "" {
			agent = "shell"
		}
		title := strings.TrimSpace(a.Title)
		if len(title) > 48 {
			title = title[:48]
		}
		fmt.Fprintf(&b, "%s %-8s %-6s %-24s @%s%s  %s  %s\n", mark, a.Activity, agent, a.Session, host, live, ago(now.Sub(a.ActivityAt)), title)
	}
	return b.String()
}

func ago(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%2ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%2dm", int(d.Minutes()))
	default:
		return fmt.Sprintf("%2dh", int(d.Hours()))
	}
}

func cmdLs(ctx context.Context, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	m := newMerged()
	var wg sync.WaitGroup
	for _, h := range cfg.Hosts {
		wg.Add(1)
		go func(h client.Host) {
			defer wg.Done()
			c, err := client.Dial(ctx, h)
			if err != nil {
				m.setHost(h.Name, hostState{Error: err.Error()})
				return
			}
			defer c.Close()
			if !protocol.Has(c.Hello.Capabilities, protocol.CapStatus) {
				m.setHost(h.Name, hostState{Error: "daemon " + c.Hello.Version + " has no status capability"})
				return
			}
			m.setHost(h.Name, hostState{Connected: true, Version: c.Hello.Version, EnvID: c.Hello.EnvironmentID})
			sctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			snap, err := c.Snapshot(sctx)
			if err != nil {
				m.setHost(h.Name, hostState{Error: err.Error()})
				return
			}
			m.apply(h.Name, snap)
		}(h)
	}
	wg.Wait()
	fmt.Print(m.render())
	return nil
}

func cmdWatch(ctx context.Context, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	m := newMerged()
	for _, h := range cfg.Hosts {
		go m.follow(ctx, h)
	}
	t := time.NewTicker(5 * time.Second) // refresh relative times
	defer t.Stop()
	for {
		fmt.Print("\033[H\033[2J" + m.render())
		select {
		case <-ctx.Done():
			return nil
		case <-m.change:
		case <-t.C:
		}
	}
}

// ---- jump ------------------------------------------------------------------

// jump focuses the local pane attached to <host>/<session>, opening one in
// the session jump was run from if none exists. Local and remote are the same
// operation: managed agents live on the dedicated laatmux server, which the
// user's tmux cannot switch-client into, so both attach through a pane.
func cmdJump(ctx context.Context, args []string) error {
	if len(args) < 1 {
		return errors.New("usage: laatmux jump <host>/<session>")
	}
	hostName, session, ok := strings.Cut(args[0], "/")
	if !ok {
		session, hostName = hostName, ""
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	h, ok := cfg.Find(hostName)
	if !ok {
		return fmt.Errorf("unknown host %q", hostName)
	}
	here := os.Getenv("TMUX_PANE")
	if os.Getenv("TMUX") == "" || here == "" {
		return errors.New("jump must run inside the local tmux")
	}
	local := tmux.Server{}
	// The session jump runs in is where a new attach window goes. Using the
	// pane rather than the current client keeps jump deterministic when run
	// from a script or a sidebar.
	out, err := local.Run(ctx, "display-message", "-p", "-t", here, "#{session_name}")
	if err != nil {
		return err
	}
	hereSession := strings.TrimSpace(string(out))
	tag := h.Name + "/" + session

	// Reuse an existing attachment, alive or dead.
	out, err = local.Run(ctx, "list-panes", "-a", "-F", strings.Join([]string{"#{@laatmux_attach}", "#{session_name}", "#{window_id}", "#{pane_id}", "#{pane_dead}"}, tmux.Sep))
	if err != nil {
		return err
	}
	attachCmd := attachCommand(h, session)
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		f := strings.Split(line, tmux.Sep)
		if len(f) != 5 || f[0] != tag {
			continue
		}
		if f[4] == "1" {
			// remain-on-exit kept a dead attachment; bring it back in place.
			if err := checkSession(ctx, h, session); err != nil {
				return err
			}
			if _, err := local.Run(ctx, "respawn-pane", "-k", "-t", f[3], attachCmd); err != nil {
				return err
			}
		}
		if f[1] != hereSession {
			if _, err := local.Run(ctx, "switch-client", "-t", f[1]); err != nil {
				return err
			}
		}
		if _, err := local.Run(ctx, "select-window", "-t", f[2]); err != nil {
			return err
		}
		_, err := local.Run(ctx, "select-pane", "-t", f[3])
		return err
	}

	if err := checkSession(ctx, h, session); err != nil {
		return err
	}
	out, err = local.Run(ctx, "new-window", "-t", hereSession+":", "-n", session, "-P", "-F", "#{pane_id}", attachCmd)
	if err != nil {
		return err
	}
	paneID := strings.TrimSpace(string(out))
	for _, kv := range [][2]string{{"@laatmux_attach", tag}, {"@laatmux_host", h.Name}} {
		if _, err := local.Run(ctx, "set-option", "-p", "-t", paneID, kv[0], kv[1]); err != nil {
			return err
		}
	}
	return nil
}

// attachCommand is the shell command an attach pane runs. TMUX is unset so
// the inner tmux does not refuse to nest; -u tells it the terminal is UTF-8.
func attachCommand(h client.Host, session string) string {
	attach := append([]string{"tmux", "-u"}, tmux.LaatmuxServer.AttachArgsBare(session)...)
	if h.Local() {
		return tmux.ShellJoin(append([]string{"env", "-u", "TMUX"}, attach...))
	}
	return tmux.ShellJoin([]string{"ssh", "-t",
		"-o", "ServerAliveInterval=15", "-o", "ServerAliveCountMax=3",
		h.SSH, tmux.ShellJoin(attach)})
}

// checkSession fails early when the target session does not exist, so jump
// reports it instead of opening a window that exits at once. A transport
// failure is reported as such, with ssh's own diagnostics, and the check is
// bounded so a stalled connection cannot block jump before the attach
// window's own keepalive protection applies.
func checkSession(ctx context.Context, h client.Host, session string) error {
	if h.Local() {
		if !tmux.LaatmuxServer.HasSession(ctx, session) {
			return fmt.Errorf("%s/%s: no such session on the laatmux tmux server", h.Name, session)
		}
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, preflightTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=10",
		"-o", "ServerAliveInterval=5",
		"-o", "ServerAliveCountMax=2",
		h.SSH,
		tmux.ShellJoin(append([]string{"tmux"}, tmux.LaatmuxServer.ArgsBare("has-session", "-t", "="+session)...)))
	var stderr strings.Builder
	cmd.Stderr = &stderr
	// After the deadline kills ssh, do not wait on its stderr pipe for
	// anything it may have left behind.
	cmd.WaitDelay = time.Second
	err := cmd.Run()
	return classifyPreflight(h.Name, session, err, ctx.Err(), strings.TrimSpace(stderr.String()))
}

const preflightTimeout = 15 * time.Second

// classifyPreflight turns the preflight's outcome into a message that says
// which of three things happened: the session is absent (tmux exited 1), the
// transport failed (ssh exits 255, or anything else), or the check timed out.
func classifyPreflight(host, session string, runErr, ctxErr error, stderr string) error {
	if runErr == nil {
		return nil
	}
	if ctxErr != nil {
		return fmt.Errorf("%s: ssh preflight timed out after %s", host, preflightTimeout)
	}
	var exit *exec.ExitError
	if errors.As(runErr, &exit) && exit.ExitCode() == 1 {
		// tmux has-session: exit 1 means no such session. Its message
		// ("can't find session") is redundant; a missing server says
		// "no server running", which is the same thing for jump.
		return fmt.Errorf("%s/%s: no such session on the laatmux tmux server", host, session)
	}
	if stderr == "" {
		stderr = runErr.Error()
	}
	return fmt.Errorf("%s: ssh failed: %s", host, stderr)
}

// ---- hosts -----------------------------------------------------------------

func cmdHosts(ctx context.Context, args []string) error {
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	type row struct {
		name, status string
	}
	rows := make([]row, len(cfg.Hosts))
	var wg sync.WaitGroup
	for i, h := range cfg.Hosts {
		wg.Add(1)
		go func(i int, h client.Host) {
			defer wg.Done()
			dctx, cancel := context.WithTimeout(ctx, 20*time.Second)
			defer cancel()
			start := time.Now()
			c, err := client.Dial(dctx, h)
			if err != nil {
				rows[i] = row{h.Name, "unreachable: " + err.Error()}
				return
			}
			defer c.Close()
			rows[i] = row{h.Name, fmt.Sprintf("ok  daemon %s  protocol %d  env %s  caps %s  %dms",
				c.Hello.Version, c.Hello.Protocol, c.Hello.EnvironmentID, strings.Join(c.Hello.Capabilities, ","), time.Since(start).Milliseconds())}
		}(i, h)
	}
	wg.Wait()
	for _, r := range rows {
		fmt.Printf("%-16s %s\n", r.name, r.status)
	}
	return nil
}

// ---- explain ---------------------------------------------------------------

func cmdExplain(ctx context.Context, args []string) error {
	fs := flag.NewFlagSet("explain", flag.ContinueOnError)
	sock := tmuxServerFlag(fs)
	lines := fs.Int("capture-lines", daemon.DefaultCapture, "screen lines")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() < 1 {
		return errors.New("usage: laatmux explain [--tmux-socket s] <pane-id>")
	}
	srv := parseServer(*sock)
	panes, err := srv.ListPanes(ctx)
	if err != nil {
		return err
	}
	var pane *tmux.Pane
	for i := range panes {
		if panes[i].ID == fs.Arg(0) {
			pane = &panes[i]
		}
	}
	if pane == nil {
		return fmt.Errorf("pane %s not found", fs.Arg(0))
	}
	fmt.Printf("pane %s session=%s tty=%s pane_pid=%d current_command=%s\ntitle: %q\n", pane.ID, pane.Session, pane.TTY, pane.PID, pane.CurrentCommand, pane.Title)
	list, err := procs.ListTTY(pane.TTY)
	if err != nil {
		fmt.Println("procs:", err)
	}
	for _, p := range list {
		argv := ""
		if len(p.Argv) > 0 {
			argv = strings.Join(p.Argv, " ")
			if len(argv) > 80 {
				argv = argv[:80] + "…"
			}
		}
		fmt.Printf("  pid=%d ppid=%d pgid=%d tpgid=%d comm=%s start=%s %s\n", p.PID, p.PPID, p.PGID, p.TPGID, p.Comm, p.Start.Format(time.TimeOnly), argv)
	}
	id, found, _ := procs.Find(pane.TTY)
	fmt.Printf("identity: found=%v agent=%q pid=%d leader=%d comm=%s\n", found, id.Agent, id.PID, id.LeaderPID, id.Comm)
	screen, err := srv.Capture(ctx, pane.ID, *lines)
	if err != nil {
		return err
	}
	in := detect.Input{Agent: id.Agent, Title: pane.Title, Screen: screen}
	res := detect.Detect(in)
	fmt.Printf("detect: state=%s rule=%q reason=%s idle=%v blocker=%v working=%v skip=%v\n", res.State, res.Rule, res.Reason, res.VisibleIdle, res.VisibleBlocker, res.VisibleWorking, res.Skip)
	fmt.Println(detect.Explain(in))
	return nil
}
