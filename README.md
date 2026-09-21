# laatmux

Git worktrees and coding agents across hosts, from tmux. The plan is
[issue #1](https://github.com/laat/laatmux/issues/1). This is the milestone-one
spike: a per-host status daemon, a merged multi-host listing, a launcher for
managed sessions, and jump.

## Layout

| Package | What |
|---|---|
| `cmd/laatmux` | CLI: `serve`, `bridge`, `new`, `ls`, `watch`, `jump`, `hosts`, `explain` |
| `internal/protocol` | JSON-lines wire format, protocol version 1, capability flags |
| `internal/daemon` | polls one tmux server, derives agent state, streams snapshot + upserts |
| `internal/detect` | screen and title rules, ported from herdr's manifests (Apache 2.0, see `manifests/NOTICE`) |
| `internal/procs` | agent instance identity from the tty's foreground process group (sysctl on macOS, /proc on Linux) |
| `internal/tmux` | `list-panes -a -F`, `capture-pane`, managed server config, `new-session` |
| `internal/client` | dial local daemon (start on demand) or `ssh -T host laatmux bridge` |
| `internal/home` | state dir, environment id, runtime file, startup lock |
| `internal/config` | `~/.config/laatmux/config.yaml` |

## Run

```sh
go build -o laatmux ./cmd/laatmux
./laatmux ls        # starts the local daemon on demand, lists agents
./laatmux watch     # live, redraws on change
./laatmux hosts     # reachability, daemon version, capabilities
./laatmux explain --tmux-socket default %12   # detection inputs and decision for one pane
./laatmux new work --cwd ~/code/foo -- claude # managed session on the laatmux tmux server
./laatmux jump mac/work                       # focus or open the attached pane
```

Config, one entry per host; omit `ssh` for this machine:

```yaml
hosts:
  - name: mac
  - name: box
    ssh: box          # ssh alias, ControlMaster assumed
    bin: laatmux      # remote binary, must be on PATH of a non-interactive shell
```

The daemon watches the `laatmux` tmux server (`tmux -L laatmux`) by default and
configures it explicitly: prefix and prefix2 off, status off, mouse off, root
and prefix key tables unbound. `--tmux-socket default` watches the default
server instead, which is what the sandboxed dev loop uses.

## Model

- Three status axes, never collapsed: **activity** from the screen (working,
  blocked, idle, unknown), **liveness** of the identified process (alive, gone),
  and **host connectivity**, which is client side only.
- **Identity** is the agent process, not the pane: pid plus start time, looked
  for among every process on the pane's tty, not only the foreground group, so
  a tool taking the foreground never replaces the agent. Only a process's own
  program identifies it: argv[0], comm, or the script an interpreter runs (node
  running Claude Code's `cli.js` is Claude). Arguments never count, so
  `safehouse /opt/bin/claude` and `bash -c 'claude; sleep 60'` are wrappers, not
  Claude, before, between and after their children. Among candidates the
  deepest descendant wins. An env hint (`LAATMUX_AGENT`) gives a tentative
  identity that selects detection rules only; the daemon keeps searching and a
  verified process replaces it. A verified instance is checked for existence
  each poll, so an exited agent stays on record as gone with its last activity
  until a new instance appears. A process-table read error changes nothing,
  and the same instance found again after a transient omission is restored
  without a reset. Claude
  Code's native binary is named by version (`2.1.278`), so a bare semver comm
  counts as claude.
- **Detection** evaluates herdr's rule manifests against the pane title and the
  visible screen every 300 ms. Captures are unconditional and never include
  scrollback, so a dismissed dialog in history cannot override the prompt. A
  failed capture is an unavailable observation, not an empty screen: the last
  activity is kept. Working to idle
  is debounced (3 confirmations, 700 ms cap); a visible idle prompt bypasses it.
  Fresh processes get a 3 s grace measured from process start.
- **Hooks** are not used.
- **Managed server** reconciliation runs on discovery, once per tmux server
  pid: global options, every global hook, session-level overrides of the
  isolation options, and both key tables. A cold start also passes
  `-f /dev/null`. `new` refuses a session that comes up with more than one
  pane. Protocol: a daemon with a different protocol number is refused; within
  a number, clients branch on capabilities.
- **Daemon startup** is arbitrated by a kernel-held flock. Snapshots wait for
  the first complete poll. A subscriber that falls behind is disconnected so it
  reconnects and resnapshots. Ssh connections use ServerAlive so a silent
  network loss surfaces as a disconnect within about 45 s.

## Dev loop under safehouse

The sandbox denies unix socket binds, new tmux servers and `ps`. Use:

```sh
export LAATMUX_HOME=$PWD/.spike
export LAATMUX_CONFIG=$PWD/.spike/config.yaml
export LAATMUX_SERVE_ARGS="--listen tcp:127.0.0.1:0 --tmux-socket default"
```

## Verified

Locally against live panes: identity through the safehouse bash wrapper,
working and idle for Claude Code and Codex, snapshot and remove on session
kill, `new` with tagged pane options, on-demand daemon start, hello with
capabilities.

Against a Debian VM over ssh (`laatmux-test.coder`): on-demand remote daemon
start through the ssh bridge, unix socket listening on both sides, the
dedicated `laatmux` tmux server with prefix, status, mouse off and both key
tables empty, `new --host vm` starting Claude Code, `/proc` identity on Linux,
blocked on the folder trust dialog, idle at the prompt, working during a turn and idle again when it finished, a daemon of
version 0.0.1 and one of 0.0.2 talking to the same client, and recovery under
`watch` after killing the ssh bridge and then the remote daemon, with no ghost
or duplicate entries.

One bug found remotely: tmux 3.5 strips control characters from expanded
formats, so a `\x1f` field separator fused the `list-panes` fields on Linux
while passing through on macOS 3.6. Fields are now separated by a printable
sequence (`tmux.Sep`).

After the first design review, on the VM: cold start under a hostile
`~/.tmux.conf` (status on, prefix `C-a`, mouse on, a root binding and a
split-on-new-session hook) still gave one pane with prefix, status and mouse
off and no root bindings. Claude killed under a surviving `bash -c` wrapper
was reported as a new instance with a new pid after the wrapper restarted it,
and after the final kill the daemon reported no agent on the tty. Identity
stayed on the Claude pid across a tool call.

After the second review, on the VM: a server started by hand under that config
carried prefix `C-a`, a session-level prefix override, status on, the split
hook, a second pane and 17 root bindings; a plain `laatmux ls` reconciled all
of it, and two `new` calls afterwards each produced one pane. Wrapper-before-
child, exit under a surviving wrapper, tentative-then-verified, transient read
error, capture failure on a blocked prompt, discovery-time reconciliation,
subprocess close under cancellation, and protocol mismatch are covered by
tests under `-race`.

## Not yet verified

`jump` (first attach, focus of an existing attach, session disappearance),
two simultaneous attachments with resize, paste, mouse and key passthrough,
and daemon restart during initial discovery.
A foreground change on the pane's tty during a tool call is covered by a unit
test only: Claude Code runs tools on pipes, so its tool calls never change the
tty's foreground group. The "gone" marker in `ls` after an agent exits is
covered by a unit test and by the daemon's identity output, not by a live
listing.
