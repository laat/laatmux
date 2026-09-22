# laatmux

Git worktrees and coding agents across hosts, from tmux. The plan is
[issue #1](https://github.com/laat/laatmux/issues/1). This is the milestone-one
spike: a per-host status daemon, a merged multi-host listing, a launcher for
managed sessions, and jump. Milestone two, worktrees and workspaces, is
designed in [docs/milestone-two.md](docs/milestone-two.md) and built.
Milestone three, the sidebar, the dashboard, `split` and `run`, is designed
in [docs/milestone-three.md](docs/milestone-three.md).

## Layout

| Package | What |
|---|---|
| `cmd/laatmux` | CLI: `serve`, `bridge`, `add`, `rm`, `path`, `ls`, `watch`, `jump`, `shell`, `settle`, `unsettle`, `new`, `hosts`, `repos`, `explain` |
| `internal/protocol` | JSON-lines wire format, protocol version 1, capability flags, agent and worktree records |
| `internal/daemon` | polls the configured tmux servers and git, derives agent state, streams snapshot + upserts; runs `add` and `rm` |
| `internal/worktree` | checkouts found under `repos` by origin, worktrees from `git worktree list`, the git and filesystem stages of `add` |
| `internal/detect` | screen and title rules, ported from herdr's manifests (Apache 2.0, see `manifests/NOTICE`) |
| `internal/procs` | agent instance identity from the tty's foreground process group (sysctl on macOS, /proc on Linux) |
| `internal/tmux` | `list-panes -a -F`, `capture-pane`, managed server config, `new-session` in one invocation, `kill-session`, branch encoding for session names |
| `internal/client` | dial local daemon (start on demand) or `ssh -T host laatmux bridge`; request and streamed command |
| `internal/workspace` | the local workspace session on the default tmux server: tags, attach and shell commands, create, switch, kill |
| `internal/home` | state dir, environment id, runtime file, startup lock, `last.json` |
| `internal/config` | `~/.config/laatmux/config.yaml`: hosts with their directories, agents, the repository list, `tmux_servers` for this machine's daemon; `.laatmux.yaml` per repository |

## Run

```sh
go build -o laatmux ./cmd/laatmux
./laatmux ls        # starts the local daemon on demand, lists workspaces and agents
./laatmux watch     # live, redraws on change
./laatmux hosts     # reachability, daemon version, capabilities
./laatmux repos     # each known repository's name and where it lands on each host
./laatmux add fix-ls                          # worktree and agent for the repo of the current directory, on the last-used host
./laatmux add fix-ls --repo proj --host vm --agent claude
./laatmux path proj/fix-ls                    # the worktree root on its host
./laatmux jump vm/proj/fix-ls                 # switch to the workspace session, creating it if missing
./laatmux shell                               # a shell at the worktree root, from inside a workspace session
./laatmux settle                              # collapse this workspace in ls; unsettle brings it back
./laatmux rm proj/fix-ls [--force]            # remove the worktree, its managed session and the local session
./laatmux explain --tmux-socket default %12   # detection inputs and decision for one pane
./laatmux new work --cwd ~/code/foo -- claude # managed session without a worktree
./laatmux jump mac/work                       # a local session attached to it
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
with an atomic rename. The design is in
[docs/milestone-two.md](docs/milestone-two.md); both sides are built, see
the two sections below.

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
  published; a detached worktree has an empty branch. The record carries
  the repository's label and its source. The record's
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
  afterwards replays the result. `add` is serialized per repository
  source, so adds for different repositories run in parallel; `rm` takes
  every repository's lock while it resolves and removes, since its root
  checks ask every checkout, and so waits for any add in flight.

## Workspaces, client side

A workspace is one worktree, one managed agent session on its host and one
local session on the laptop. The local session lives in the user's default
tmux server, named `<host>/<repo>/<encoded branch>`, with one window
running the attach command (`env -u TMUX tmux -L laatmux attach` locally,
the same through `ssh -t` remotely). It carries `@laatmux_workspace` =
`<environment_id>/<root>`, the workspace key, `@laatmux_host`, and
`@laatmux_repo` and `@laatmux_branch`, the source and branch; the attach
pane carries `@laatmux_attach_pane` and `remain-on-exit`. Sessions are
matched on the key, never the name, so a renamed host or repository label
still finds its session. The host tag is refreshed on every reuse, and the
source and branch whenever the reuse knows them: the worktree record
carries the source from the daemon, so `jump` never derives it from a
label that may mean another repository on this machine. The session is
created detached and tagged in one tmux command sequence, then the attach
pane is tagged by the id `new-session` printed, since the user's hooks may
split the window at once. The pane runs a placeholder until then, so it
cannot exit before `remain-on-exit` is set on it; the attach command
replaces the placeholder in the same sequence as the tags, and an attach
that fails at once leaves a dead pane for the next `jump` to respawn.

- **`add <branch>`** resolves the repository from `--repo`, else from the
  current directory: its git origin is matched against the known sources,
  since identity is the source and a checkout keeps its directory after a
  label change; an origin that is not configured is an error rather than
  a guess from the directory name; only a directory with no origin falls
  back to its place under the local host's `repos` or `worktrees`, the
  more specific first, where the next path component is the label. Host
  and agent come from
  their flags, else `last.json`, else the config's default order. The
  command id is chosen once per invocation; a transport failure mid-way
  dials again with the same id, and the daemon's replay is printed once.
  Progress prints one line per step. On success `last.json` is updated
  and the workspace session is created, or found by key; inside the
  default tmux server the client switches to it, elsewhere it prints how
  to attach, with the default server selected explicitly, and inside
  another tmux server it says to detach first.
- **`rm <repo>/<branch>`** sends the root along whenever it is known: from
  the host's record, or, when the worktree is already gone, from the key
  of the local session found by its source and branch tags; a session
  found by name is accepted only when it carries no identity tags at all,
  never when they name another workspace. Git's refusal of a dirty worktree comes back as the error
  with everything left in place; `--force` removes it. After an `ok` the
  local session with that key is killed, switching away first if it is the
  current one. `rm --root <path> --host h` removes a detached worktree.
- **`path <repo>/<branch>`** prints the root from the host's records.
  Records are matched by source, since the host's label for a source may
  differ from this machine's; a record from a daemon without the source
  is matched by label. `rm` finds its record the same way.
- **`jump <host>/<repo>/<branch>`** switches to the workspace session,
  creating it from the record when missing, respawning a dead attach pane,
  and opening a new attach window when the pane is gone altogether. A
  managed session that is no worktree's, one `new` made, is reached the
  same way through a session named `<host>/<session>` tagged
  `@laatmux_attach`. The repository in the target is read as this
  machine's label first, then the host's, and the target after the host
  may also be the managed session's name, with the branch encoded, which
  is how a worktree detached in place is still reached. The local
  session's name follows the managed session's. `--server default` still
  switches to an observed session on this machine's tmux.
- **`ls`** joins each host's worktrees with its agents by the managed
  session the record names. A worktree shows `no agent` when its session
  has no identified agent and `no session` when it has none; a managed
  agent with no worktree says so; observed agents name their server.
  Settled workspaces are listed under `settled`, and a local workspace
  session whose worktree is gone from a connected host under `stale`, from
  which `rm` still works; a host whose snapshot has not arrived, or whose
  daemon does not publish worktrees, says nothing about its workspaces. `watch` re-reads the local sessions on each
  redraw.
- **`shell`** runs inside a workspace session and opens a window at the
  worktree root: started there for a local host, `ssh -t` with `cd` and
  the single-quoted root then `exec "$SHELL" -l` for a remote one. The
  window is tagged `@laatmux_shell`; a second call selects it. Meant to
  be bound in the user's tmux config, as
  `bind-key S run-shell 'laatmux shell'`: a `run-shell` job has `TMUX`
  naming the session the key was pressed in but no `TMUX_PANE`, and the
  session is resolved from either.
- **`settle`** and **`unsettle`** set and clear `@laatmux_settled` on the
  workspace session they run from, or the one named.

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

Milestone one's jump opened an attach window in whatever session it ran
from; milestone two replaced that with the workspace session above, and
the first and sixth findings below describe the old behaviour: jump now
switches to the workspace session, and a dead attach stays as a dead pane
under `remain-on-exit` until the next jump respawns it. The attach command
and the transport findings, keys, size, two attachments, the preflight and
mouse, carried over unchanged. Run from a scratch tmux session so the
user's view stayed untouched. Local and remote:

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

## Milestone two, steps 4 and 5, on the VM

From the laptop with a scratch config naming the VM as the only host and a
`sleep` agent, run outside tmux so nothing switched the user's client:
`add` streamed every stage and created the tagged local session; a repeat
`add` skipped every step and found the session by key; `path` printed the
root; `jump` on a worktree without a session said how to start one, and on
an unknown name reported no such session. `shell` run with `TMUX_PANE` set
to the workspace's attach pane opened an ssh window at the root on the VM,
a second call selected it, and outside a workspace session it said so.
`settle` moved the row under `settled` in `ls` and `unsettle` by name
cleared it. Killing the attach pane's ssh left it dead and `jump`
respawned it; a session whose attach pane had been closed got a new attach
window on the next `jump`. `rm` was refused with git's message while
`setup.log` was untracked and the sessions stayed; `rm --force` removed
the worktree, killed the managed session and the local one. A worktree
removed by hand on the VM showed the local session under `stale`, and `rm`
on it returned `ok`, killed the surviving managed session by root and the
local session. A detached worktree was removed with `--root`.

The user's tmux config splits every new session with a sidebar pane, which
is why the attach pane is tagged by id rather than taken as the active
pane.

After review, on the VM: a stale `@laatmux_host` set by hand on a
workspace session was refreshed by the next `jump`; a workspace session
renamed by hand whose worktree was then removed on the VM was found by its
source and branch tags, and `rm` killed the surviving managed session by
root and the renamed local session; `add` run from a checkout whose origin
is not configured refused with the origin named rather than guessing from
the directory. Then, with the source in the record: `add` under a local
label that differs from the host's, and `jump` by the host's label, left
the source tag intact; `rm` on a branch with no worktree whose local
session by name carried another source and branch sent no root and left
both that session and the other worktree alone, while the same session
with no identity tags had its root sent and was killed.

## Not yet verified

Nothing in milestone one's acceptance list. Mouse passthrough was checked with
injected reports, not a physical click.
