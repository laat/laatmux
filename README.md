# laatmux

Git worktrees and coding agents across hosts, from tmux. The plan is
[issue #1](https://github.com/laat/laatmux/issues/1). This is the milestone-one
spike: a per-host status daemon, a merged multi-host listing, a launcher for
managed sessions, and jump. Milestone two, worktrees and workspaces, is
designed in [docs/milestone-two.md](docs/milestone-two.md).

## Layout

| Package | What |
|---|---|
| `cmd/laatmux` | CLI: `serve`, `bridge`, `new`, `ls`, `watch`, `jump`, `hosts`, `repos`, `explain` |
| `internal/protocol` | JSON-lines wire format, protocol version 1, capability flags, agent and worktree records |
| `internal/daemon` | polls the configured tmux servers and git, derives agent state, streams snapshot + upserts; runs `add` and `rm` |
| `internal/worktree` | checkouts found under `repos` by origin, worktrees from `git worktree list`, the git and filesystem stages of `add` |
| `internal/detect` | screen and title rules, ported from herdr's manifests (Apache 2.0, see `manifests/NOTICE`) |
| `internal/procs` | agent instance identity from the tty's foreground process group (sysctl on macOS, /proc on Linux) |
| `internal/tmux` | `list-panes -a -F`, `capture-pane`, managed server config, `new-session` in one invocation, `kill-session`, branch encoding for session names |
| `internal/client` | dial local daemon (start on demand) or `ssh -T host laatmux bridge` |
| `internal/home` | state dir, environment id, runtime file, startup lock, `last.json` |
| `internal/config` | `~/.config/laatmux/config.yaml`: hosts with their directories, agents, the repository list, `tmux_servers` for this machine's daemon; `.laatmux.yaml` per repository |

## Run

```sh
go build -o laatmux ./cmd/laatmux
./laatmux ls        # starts the local daemon on demand, lists agents
./laatmux watch     # live, redraws on change
./laatmux hosts     # reachability, daemon version, capabilities
./laatmux repos     # each known repository's name and where it lands on each host
./laatmux explain --tmux-socket default %12   # detection inputs and decision for one pane
./laatmux new work --cwd ~/code/foo -- claude # managed session on the laatmux tmux server
./laatmux jump mac/work                       # focus or open the attached pane
./laatmux jump --server default mac/notes     # switch to an observed session in this machine's tmux
```

Config, one entry per host; omit `ssh` for this machine:

```yaml
hosts:
  - name: mac
    repos: ~/code             # main checkouts live in <repos>/<name>
    worktrees: ~/worktrees    # worktrees live in <worktrees>/<name>/<branch>
  - name: box
    ssh: box                  # ssh alias, ControlMaster assumed
    bin: laatmux              # remote binary, must be on PATH of a non-interactive shell
    repos: ~/src
    worktrees: ~/src/worktrees
tmux_servers: [laatmux, default]   # what this machine's daemon watches
agents:
  claude:
    cmd: [claude]
  claude-safe:
    cmd: [claude-safe]        # sandboxing is the launch command's business
repos:                        # the known set
  - git@github.com:laat/laatmux.git
  - source: https://github.com/laat/other.git
    name: notes               # optional; otherwise derived from the source
```

`hosts`, `agents` and `repos` are read by clients. `tmux_servers` and the
local host's `repos` and `worktrees` are read by the daemon on the machine
the file lives on, so the laptop's config cannot change what a remote daemon
watches or where it clones; each host's own config does that. The default
server list is the managed `laatmux` server alone.
`laatmux serve --tmux-servers laatmux,default` overrides the file;
`--tmux-socket` is the older spelling of the same flag.

Host names, agent keys and repository names are labels: `A-Z a-z 0-9 _ -`,
nothing else, since they end up in session names, ids and directory names.
A host named after its ssh alias, and a repository named from its source,
must pass the same rule or the config is rejected asking for an explicit
`name`. `repos` and `worktrees` have no defaults, and must be absolute or
start with `~`; a host without them cannot `add`. A repository's name is derived from its source: the last path
component without `.git`; on a collision each is prefixed with its org
(`laat-laatmux`, `acme-laatmux`); if they still collide, or there is no org
to prefix, the first six hex digits of the source's SHA-256 are appended.
The derivation is deterministic, so every host derives the same name from
the same list, and duplicate sources or duplicate final names are rejected.
Identity is the source, not the name: the name only places new things.
`laatmux repos` shows each name next to its checkout and worktree paths per
host, as configured, so a `~` is the host's own.

Shared setup lives in `.laatmux.yaml` at the repository root, committed:

```yaml
copy: [.envrc, .env.local]   # from the main checkout, skipped when present
setup: ["pnpm install"]      # each runs at least once; must tolerate a rerun
```

Each `setup` entry runs as `sh -c <string>` in the worktree root. The
last-used host and agent per repository are state, not config: they live in
`$LAATMUX_HOME/last.json`, keyed by source, and are updated under a lock
with an atomic rename. The daemon side of `add` and `rm` is built, see
below; the client commands that use it (`add`, `rm`, `path`, the workspace
session, `jump` switched to it) are designed in
[docs/milestone-two.md](docs/milestone-two.md) and not yet built.

## Worktrees and add, daemon side

The daemon on a host with `repos` and `worktrees` advertises `worktrees`,
and with the managed server also `add` and `rm`. Git is the source of
truth; labels only place new things.

- **Worktree records** arrive in the subscription stream next to agents:
  `worktrees` in a snapshot, `worktree` in an upsert, `worktree_id` in a
  remove. Every two seconds the daemon finds each known repository's
  checkout under `repos` by its `origin`, asks it for
  `git worktree list --porcelain`, and publishes the entries under
  `worktrees/`. Prunable entries, whose directory is gone, are not
  published; a detached worktree has an empty branch. The record's
  `session` is the managed session whose single pane records the root in
  `@laatmux_cwd`, joined from the pane poll, so an agent exiting updates
  the record without a git call. Origin reads are cached by the mtime of
  `.git/config`, so an idle poll spawns one git process per known
  repository. The id is `<environment_id>/worktree/<root>`.
- **`add`** `{type: add, id, repo, branch, agent_name, cmd}` runs the
  stages in the note, each step skipped by inspection: resolve, clone
  (refused when `<repos>/<name>` exists with another origin), fetch,
  worktree (`set-head --auto` and prune; a remote branch is tracked, an
  existing local branch used as is, a new one made with `--no-track` from
  `origin/HEAD`; the root registered on another branch or the branch
  checked out elsewhere fails the stage), copy (through a temporary file
  renamed into place), setup (markers under the worktree's git directory,
  keyed by index and hash of the command), agent (one tmux invocation,
  skipped when a managed session already runs in the root, refused as a
  name in use when the intended name runs elsewhere). Progress streams as
  `{type: progress, id, stage, state, detail}` with state `start`, `done`,
  `skip` or `output`; the result carries `stage` on failure, and
  `session`, `pane_id` and `root` on success. `repo` is the source or the
  label as the daemon's own config knows it; the key is `agent_name`
  because `agent` is the upsert's record in the same envelope.
- **`rm`** `{type: rm, id, repo, branch, root, force}` removes the worktree
  through git, which refuses a dirty or locked one without `force` and
  says why, then kills every managed session whose pane records the
  root. Both steps skip when already done, so a repeat is `ok`. Only a
  worktree under `worktrees/` is removed, by branch or by root; one the
  user made elsewhere is left alone, as is the branch. Send `root` from
  the record whenever it is known: it is what reaches a session whose
  worktree is already gone, since a branch alone maps to no root then.
- **Retry and serialization**: commands run under the daemon's context
  and outlive the connection that sent them. Ids are kept for five
  minutes; the same id while a command runs attaches to its stream, and
  afterwards replays the result. Commands are serialized per repository
  source and run in parallel across repositories.

## Which tmux servers the daemon polls

Decided in issue #3: status for any tmux server the host config names; attach
only to managed sessions.

- The `laatmux` server (`tmux -L laatmux`) is the managed one. The daemon
  configures it explicitly (prefix and prefix2 off, status off, mouse off,
  root and prefix key tables unbound) and `new` creates sessions there. A
  daemon whose list leaves it out does not advertise the `new` capability.
- Every other server in `tmux_servers`, the user's `default` included, is
  observed read-only: `list-panes` and `capture-pane`, nothing else. No
  workmux config, state or hooks are read. `default` selects `-L default`
  explicitly, so a daemon started from inside another tmux server still
  polls the default one rather than following the inherited `TMUX`.
- Agent ids are `<environment_id>/<server>/<pane_id>` and records carry the
  server, so `%1` on two servers cannot collide. Clients treat the id as
  opaque; a daemon from before this change sends no server, which clients read
  as `laatmux`.
- Only panes with an identified agent instance, alive or gone, are published.
  Shells and other tools' panes never appear on any server, and their title
  churn produces no traffic.
- `ls` shows agents on the managed server as `@host` and others as
  `@host/server`, which is what `jump --server` takes. Jump to a session on
  this machine's default server is a `switch-client`, since it is already in
  the user's tmux; it must be run from a client of that server. Jump to a remote host's default server, or to any other
  unmanaged server, is refused.

The intended policy from issue #1: the laptop watches its default server plus
the managed server; remote hosts watch only the managed server, with the
laptop providing the UI.

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
  pane. Only the `laatmux` server is ever reconciled; the other servers the
  daemon polls are the user's and are read only. Protocol: a daemon with a different protocol number is refused; within
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
export LAATMUX_SERVE_ARGS="--listen tcp:127.0.0.1:0 --tmux-servers default"
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

## Jump and attach spike

Run from a scratch tmux session so the user's view stayed untouched. Local
and remote:

- First `jump` opens a window in the session it was run from and tags the
  pane; a second `jump` focuses that window instead of opening another.
- Keys typed into the attach pane reach the managed pane; paste-buffer too.
- The inner pane follows the outer pane's size, locally and over ssh.
- Two attachments to one session both see the same output; closing one leaves
  the other. With `window-size latest` the inner size trails one event behind
  whichever client last acted, which is tmux's semantics.
- Killing the attach pane's ssh closes the window; the remote session survives
  and the next `jump` reattaches. Killing the remote session closes the window
  and the next `jump` reports "no such session" instead of opening a dying one.
- Killing the daemon repeatedly during initial discovery while `watch` was
  subscribed left one row per agent, no ghosts, after reconnect.

Two bugs found by the spike: the inner tmux refused to nest because `TMUX` was
still set in the attach pane, and the exact-match session target `=name` was
eaten by zsh's equals expansion. Both fixed.

From review: the remote preflight (`has-session` over ssh before opening a
window) now reports an absent session, an ssh failure with ssh's own message,
and a timeout as three different errors, and is bounded to 15 s with
ConnectTimeout and keepalives. Verified live: an unresolvable host fails at
once with ssh's message. Under `remain-on-exit on` a dead attach pane is
respawned in place and keys pass through afterwards.

Mouse: a probe in the managed pane that requests mouse mode and echoes its
input (`printf '\e[?1000h\e[?1006h'; cat -v`) received SGR click and wheel
reports injected into the attach pane with `send-keys -l`, locally and over
ssh, and the mode request propagated outward: both the inner pane and the
outer attach pane showed `mouse_any_flag` set, which is what makes the outer
tmux forward a real click rather than use it. Claude Code and Codex do not
request mouse mode, so ordinary clicks stay with the local tmux. A real click
in a real terminal was not part of the spike; to do it, run the probe in a
managed pane, click in the attach pane and expect `^[[<0;12;5M` with the
column and row of the click.

## Milestone two, step 3, on the VM

Against the Debian VM through the ssh bridge with raw protocol messages,
since the client commands are not built yet: a first `add` cloned a local
bare repository into `~/src/proj`, made the branch and worktree, ran both
setup commands and started the agent in a tagged session, and the worktree
record named the session. Then, in order: a half-written copy temp file and
a removed setup marker left behind as if by a crash, after which the retry
finished the copy, reran only the unmarked command and skipped the agent
stage by root, and the same id sent again replayed the identical stream
without running anything. A branch whose committed `.laatmux.yaml` fails
stopped at `setup` with the command's output and left the worktree; the
file fixed in the worktree itself was read on the retry. A hand-made session
under the intended name on another root failed the `agent` stage as a name
in use, and once killed the retry started the agent with every other step
skipped. `rm` on a worktree with an untracked file was refused with git's
message and the session left running; with `force` the worktree went and the
session was killed; a repeat was `ok`; a detached worktree made by hand was
removed by root alone. A worktree directory deleted by hand disappeared from
the listing, and the next `add` pruned it, registered it again, reran setup
because the markers died with the git directory, and found the surviving
session by root. Two `add`s on the public laatmux repository at once, one
starting Claude Code over an HTTPS clone, ran one after the other under the
per-repository lock; the snapshot showed Claude blocked on the trust dialog
and four worktree records each with its session.

## Not yet verified

Nothing in milestone one's acceptance list. Mouse passthrough was checked with
injected reports, not a physical click.
