# laatmux

Git worktrees and coding agents across hosts, from tmux. The plan is
[issue #1](https://github.com/laat/laatmux/issues/1). This is the milestone-one
spike: a per-host status daemon, a merged multi-host listing, a launcher for
managed sessions, and jump. Milestone two, worktrees and workspaces, is
designed in [docs/milestone-two.md](docs/milestone-two.md) and built.
Milestone three, the sidebar, the dashboard, `split` and `run`, is designed
in [docs/milestone-three.md](docs/milestone-three.md) and built. Milestone
four, the task form and the background add, is designed in
[docs/milestone-four.md](docs/milestone-four.md).

## Layout

| Package | What |
|---|---|
| `cmd/laatmux` | CLI: `serve`, `bridge`, `add`, `rm`, `run`, `path`, `ls`, `watch`, `sidebar`, `dashboard`, `jump`, `shell`, `split`, `settle`, `unsettle`, `new`, `hosts`, `upgrade`, `stop`, `repos`, `explain` |
| `internal/protocol` | JSON-lines wire format, protocol version 1, capability flags, agent, worktree, host and session records |
| `internal/daemon` | polls the configured tmux servers and git, derives agent state, streams snapshot + upserts; runs `add`, `rm` and `run` with numbered progress a client follows by id; merges the configured hosts' streams into one for local clients |
| `internal/worktree` | checkouts found under `repos` by origin, worktrees from `git worktree list`, the git and filesystem stages of `add` |
| `internal/detect` | screen and title rules, ported from herdr's manifests (Apache 2.0, see `manifests/NOTICE`) |
| `internal/procs` | agent instance identity from the tty's foreground process group (sysctl on macOS, /proc on Linux) |
| `internal/tmux` | `list-panes -a -F`, `capture-pane`, managed server config, `new-session` in one invocation, `kill-session`, branch encoding for session names |
| `internal/client` | dial local daemon (start on demand) or `ssh -T host laatmux bridge`; request and streamed command |
| `internal/command` | the client side of `add`, `rm`, `run` and `shell`, one implementation each for the CLI and the dashboard, with the reconnect and follow logic |
| `internal/workspace` | the local workspace session on the default tmux server: tags, attach and shell commands, create, switch, kill |
| `internal/rows` | the rows the listing, the sidebar and the dashboard share: worktrees joined with agents and local sessions, dim state, groups |
| `internal/view` | the list view: pure renderer for the tile and compact layouts, keys and mouse, raw mode, the draw loop |
| `internal/home` | state dir, environment id, runtime file, startup lock, `last.json` |
| `internal/config` | `~/.config/laatmux/config.yaml`: hosts with their directories, agents, the repository list, `tmux_servers` for this machine's daemon, `sidebar`; `.laatmux.yaml` per repository |

## Run

```sh
go build -o laatmux ./cmd/laatmux
./laatmux ls        # starts the local daemon on demand, lists workspaces and agents
./laatmux watch     # live, redraws on change
./laatmux hosts     # reachability, daemon version, capabilities; marks daemons that differ from this build
./laatmux upgrade vm    # build for the host from this checkout, install over ssh, restart its daemon
./laatmux stop      # end this machine's daemon cleanly; the next command starts one again
./laatmux repos     # each known repository's name and where it lands on each host
./laatmux add fix-ls                          # worktree and agent for the repo of the current directory, on the last-used host
./laatmux add fix-ls --repo proj --host vm --agent claude
./laatmux add -p 'make ls sort by host'          # branch proposed from the prompt, made unique on the host; the agent gets the prompt
./laatmux add fix-ls -p 'make ls sort by host'   # the same with the branch given
./laatmux add -p 'make ls sort by host' --detach # hand it to the local daemon and return; laatmux tasks shows it
./laatmux tasks                                  # the background adds and their state; tasks show|dismiss|prompt <id>
./laatmux path proj/fix-ls                    # the worktree root on its host
./laatmux jump vm/proj/fix-ls                 # switch to the workspace session, creating it if missing
./laatmux shell                               # a shell at the worktree root, from inside a workspace session
./laatmux split -h '#{pane_id}'               # from a binding: split the pane; a workspace's new pane is a shell at the root on its host
./laatmux run proj/fix-ls -- go test ./...    # run in the worktree root on its host, output and exit status streamed back
./laatmux run -- make                         # inside a workspace session, that workspace
./laatmux settle                              # collapse this workspace in ls; unsettle brings it back
./laatmux rm proj/fix-ls [--force]            # remove the worktree, its managed session and the local session
./laatmux rm                                  # inside a workspace session: that workspace
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
    cmd: [claude, "{prompt}"] # {prompt} is replaced by add -p's prompt, or removed
  claude-safe:
    cmd: [claude-safe]        # sandboxing is the launch command's business;
                              # without {prompt} the prompt is typed into the pane
default_agent: claude         # what add starts when --agent and last.json say nothing
repos:                        # the known set
  - git@github.com:laat/laatmux.git
  - source: https://github.com/laat/other.git
    name: notes               # optional; otherwise derived from the source
    copy: ["config/*.local"]  # this machine's own steps for the repository's worktrees,
    setup: ["pnpm install"]   # after the committed .laatmux.yaml's
copy: ["**/.envrc.cache.enc"] # this machine's own copy rules for every worktree
```

`hosts`, `agents` and `repos` are read by clients. `tmux_servers` and the
local host's `repos` and `worktrees` are read by the daemon on the machine
the file lives on, so the laptop's config cannot change what a remote daemon
watches or where it clones; each host's own config does that. The default
server list is the managed `laatmux` server alone.
`laatmux serve --tmux-servers laatmux,default` overrides the file;
`--tmux-socket` is the older spelling of the same flag.

An agent's `cmd` may carry `{prompt}` as one argument: `add -p` replaces
it with the prompt, as one argument however many lines it has, and an add
without a prompt removes it. The prompt is then on the host's process
list for as long as the agent runs, visible to another user of that host
with `ps`; choosing the placeholder is choosing that. A `cmd` without it
gets the prompt typed into the pane once the agent is ready, as the
task section below says.

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

Each `setup` entry runs as `sh -c <string>` in the worktree root. What
is personal to a machine goes in its config rather than the committed
file: a top-level `copy` list applies to every worktree the machine's
daemon makes, and a repository entry's `copy` and `setup` to that
repository's, each run after the committed file's. A `copy` entry is a
path relative to the repository root, or a glob: `*`, `?` and `[...]`
within a path segment, `**` as a whole segment for any number of them.
A glob is matched against what git knows of the main checkout, the
files it tracks or does not ignore plus the ignored files, which is
where an encrypted env cache sits, with ignored directories collapsed,
so `**/.envrc.cache.enc` finds the caches in every package and never
walks `node_modules`. Each match that is a regular file is copied as
a literal entry is, through a temporary file renamed into place,
skipped when present; a symlink, a directory or a submodule a glob
matches is passed over. Nothing is read from outside the checkout or
written outside the worktree: a source is read where it resolves and a
target written where its directory resolves, and either resolving out
through a symlink is an error. The committed commands and a
repository's own each number their own completion markers, so a
committed list that grows does not move a personal command onto
another's marker. The
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
- **`run`** `{type: run, id, repo, branch, root, cmd}`, capability `run`,
  runs `cmd` as a subprocess of the daemon in `root`, which must be a
  registered worktree of a known repository under `worktrees/` and, when
  `repo` and `branch` are given, theirs. No shell, no tty, stdin at
  `/dev/null`, the daemon's environment, its own process group. When
  the process exits, whatever it left in its group is laatmux's own and
  is stopped the way a cancel stops it, so a background child of a run
  never outlives the result and `rm` never meets one; the process
  exiting while such a child holds the pipes costs a second before
  that. Output
  streams as `{type: progress, id, n, stage: run, state: output, fd,
  detail}` one line per message, `fd` 1 or 2, a partial last line at
  exit; the result is `ok` with `exit` when the process exited at all,
  `ok: false` with `error` for laatmux's own failures. The two streams
  are read as two pipes, so the order between a stdout line and a
  stderr line is not kept, as with any pipe pair; within one it is.
  Output is text: binary is mangled by the line split, and a line past
  64 KiB is cut there with `...` appended, as setup output is, so one
  very long JSON line arrives cut. `{type: cancel,
  id}` sends `SIGTERM` to the process group, `SIGKILL` five seconds
  later unless every member of the group has gone, and the result says
  `cancelled` once it has; the group is watched, not the child, since a
  descendant that ignores the signal outlives its parent. A follower
  whose connection ends is let go at once, not at the next event. A cancel that
  lands before the process has started means it never starts. A clean
  daemon shutdown, on a signal or a failure, closes the registry so no
  run starts after it, cancels its runs the same way and waits for
  them. Runs take no
  repository lock. `rm`, once git has removed the worktree and before it
  kills the sessions, cancels every run in that root and waits, so its
  `ok` means nothing of laatmux's is left there. The two interlock on a
  removal generation per root: a run reads it before it asks git, and
  registers only if it is unchanged; `rm` bumps it under the same mutex
  it takes the root's runs under, so a run that resolved before the
  removal is refused with `worktree removed; retry` whether or not a
  worktree is back at that root.
- **Follow and numbered progress**, capability `follow`: every progress
  message carries `n`, from 1 per command, and a client that lost its
  connection sends `{type: follow, id, after}` in place of the command;
  the daemon replays from `after + 1` and keeps sending. An unknown id
  gets `{type: result, ok: false, error: "unknown command"}`; `add` and
  `rm` clients then resend the command as a new execution, `run` reports
  the outcome unknown. Retention differs per command: `add` and `rm`
  drop output past 1 MiB for good after one line saying so; a run keeps
  streaming past it and forgets its oldest lines for replay, and a
  follow from before the retained tail gets one `{state: gap, n,
  detail: "<count> lines dropped"}` numbered as the last dropped line.
  The ring is the one buffer, so a connected follower that falls more
  than the budget behind, on a slow link, gets a gap the same way: the
  process is never stalled by a reader, as a slow subscriber of the
  status stream is dropped rather than throttling the daemon. Each line
  is charged its bytes plus 64 for the message around it, so a stream
  of empty lines is bounded too.
  Without the capability the client's older path holds: the same id
  resent attaches to a running command and replays a finished one from
  the start, filtered by position.
- **Retry and serialization**: commands run under the daemon's context
  and outlive the connection that sent them. Ids are kept for five
  minutes. `add` is serialized per repository source, so adds for
  different repositories run in parallel; `rm` takes every repository's
  lock while it resolves and removes, since its root checks ask every
  checkout, and so waits for any add in flight.
- **Tasks**, capability `task`, the host's side of
  [milestone four](docs/milestone-four.md): a command journal, one file
  per add under `<state>/commands/`, for the two decisions a retry
  cannot inspect. `add` takes `prompt`, `generated` and `submitted_at`.
  The journal is read before anything is done for an id it has seen: a
  terminal entry is answered with its recorded result and nothing runs,
  one the daemon died in is resumed, the steps by inspection, the
  allocation and the launch by the record, and `follow` for an id the
  memory has let go is answered from it: the result, `removed` after an
  `rm`, or `interrupted` with the stage reached, on which the client
  resends the add under the same id. An id the journal never saw is
  `unknown command` as before. An add whose `submitted_at` is more than
  a day in the host's future or more than thirty days in its past is
  refused as `submission expired`; entries are swept thirty days after
  they became terminal. The client's side of the contract is in one
  send path: an add is neither sent nor resent more than seven days
  after its submission.
  With `generated`, `branch` is a proposal and an `allocate` stage after
  `fetch` picks the first free of `<name>`, `<name>-2`, `<name>-3`
  against the local and remote branches, the registered worktrees and
  the names other unfinished entries hold, writes it to the journal
  before the branch is made, and reports `{stage: allocate, state, detail,
  branch, root}`; a given branch gets the same line as `skip`. The
  result carries `branch`.
  The agent stage is journaled as `launching` before `new-session` and
  `launched` after, with the pane and the tmux server instance. The
  prompt reaches the agent on the argv when the `cmd` has `{prompt}`,
  and `launched` is then `delivered`; otherwise it is typed in once the
  pane is ready: a fresh observation by the detector, after the startup
  grace, with the prompt box on screen (`VisibleIdle`, not the idle
  fallback), a verified live agent identified in the pane, the pane and
  server instance the journal names. That identity is bound into the
  journal before the paste and required by every later one. The paste
  is `load-buffer` from stdin into a buffer named for the attempt,
  `paste-buffer -p`, `send-keys Enter`, `delete-buffer`; a daemon that
  starts deletes every `laatmux-attempt-*` buffer. A pane not ready
  within a minute gets nothing. Claude Code asks, the first time it
  starts in a folder, whether the folder is trusted, and takes neither
  a typed prompt nor one on its command line until it is answered;
  every worktree can be such a folder. After a launch in a root under
  the host's `worktrees` directory, and again after a restart that
  finds the typed prompt not yet delivered, the daemon watches the pane
  for that question, for up to the same minute. It answers only while
  the pane is still in the session and on the tmux server instance it
  was launched in, with a verified Claude identified in it, and only
  when the bottom of the screen is the whole question, naming exactly
  that root, with its two options, one cursor, and nothing after the
  footer. Each key, two at most, is pressed under the root's
  delivery lock on a capture made under it: the cursor moved onto `Yes,
  I trust this folder`, then Enter with it there. Text that does not
  match is left alone, and the wait times out as before; `StopRuns`
  cancels the watchers. The result carries `prompt`, the
  delivery state: `none` (no prompt), `delivered`, `not delivered` with
  the reason in `error` on an ok result (pane never ready, paste refused
  before it began, a managed session already in the root: `session
  existed`, a launch that failed before anything started), or `unknown`
  (a daemon death between `launching` and `launched` or during a paste,
  `new-session` failing after the session may have been made, Enter
  refused after the paste). Nothing is inferred from a session's
  existence, and no daemon delivers again on its own. The prompt is
  never in the journal, a progress line, or a tmux error: the agent
  stage's `start` line names the command with the placeholder in it,
  and an error that echoes the command line has the prompt replaced.
  `{type: prompt, id, attempt, prompt}` delivers a pending prompt later,
  as attempt `attempt` from 1 in order: the journal serializes attempts
  per add, answers a repeat of a number with its recorded outcome rather
  than pasting again, checks the target is still the recorded pane on
  the recorded server with the bound agent (`not delivered: session
  replaced` otherwise), and for an entry without a target adopts the
  managed session in the root when it is the only one and its pane has
  a verified agent (`not delivered: no agent to deliver to` otherwise).
  `follow` with `attempt` reattaches to one in flight or answers from
  the record; an attempt the journal never saw is `unknown attempt`,
  on which the client resends; an id the journal no longer holds is
  `recovery expired`. `laatmux add -p` in the foreground prints the
  delivery state as its last line and exits 0 when the add succeeded,
  whatever the delivery.
  Listings are stamped: the daemon counts observations owed in a
  `revision`, stepped at the end of every add that succeeded and by
  every `rm` that removed a worktree; a poll reads it before it asks
  git and publishes its listing stamped `{generation, revision}`, the
  generation being the daemon's start, in the snapshot and in an upsert
  of `listing` alone when it changes, with `listing_error` when the last
  listing failed. An add's result carries the barrier its mutation made;
  a listing at that revision or later in the same generation reflects
  it, and so does any successful listing of a later generation.

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
  their flags, else `last.json`, else for the agent `default_agent`, else
  the only candidate, else an error naming the candidates. The
  command id is chosen once per invocation; a transport failure mid-way
  dials again and follows the id from the last numbered progress seen,
  or against an older daemon resends it and prints the replay once.
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
- **`rm`** with no target, inside a workspace session, removes that
  workspace: the root from the session's key, on the host the session's
  tag names, or, when that tag names no configured host any more, the
  host that answers as the key's environment, as the dashboard routes
  a row; the host must answer as the environment the key names, since
  a host entry moved to another machine must not remove that machine's
  worktree at the same path, and every connection of the command is
  held to that. The host's worktree record for the root gives the
  repository and branch, which the session's tags can misname after a
  switch or a detach in the worktree; without a record the tags do,
  when this machine's config knows the source, else the root alone as
  `--root` does. That is how the dashboard's `x` resolves a row, and
  what a binding needs:
  `bind-key W confirm-before -p "remove this workspace? (y/n)" "run-shell 'laatmux rm'"`.
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
  daemon does not publish worktrees, says nothing about its workspaces.
  The rows are the same the sidebar and the dashboard show, see below.
  `ls`, `watch`, `jump`, `path` and `rm` read the local daemon's merged
  stream when it has one, see below; against an older daemon each dials
  the hosts itself as before, and `watch` then re-reads the local
  sessions on each redraw.
- **`shell`** runs inside a workspace session and opens a window at the
  worktree root: started there for a local host, `ssh -t` with `cd` and
  the single-quoted root then `exec "$SHELL" -l` for a remote one. The
  window is tagged `@laatmux_shell`; a second call selects it. Meant to
  be bound in the user's tmux config, as
  `bind-key S run-shell 'laatmux shell'`: a `run-shell` job has `TMUX`
  naming the session the key was pressed in but no `TMUX_PANE`, and the
  session is resolved from either.
- **`split [-h|-v] [<pane-id>]`** is the split binding for every window:
  from the pane, passed in since a `run-shell` job has no `TMUX_PANE`, it
  reads the session's tags on the server `TMUX` names. Not a workspace
  session: `split-window -t <pane> -c '#{pane_current_path}'`, the plain
  split. A workspace on the local host: `-c <root>`. On a remote host:
  the ssh command `shell` builds. The new pane lands at the worktree
  root, not the split pane's directory, and split panes carry no tags:
  the session's decide, so a split of a split resolves the same way.
- **`run [<repo>/<branch>] [--host h] -- <cmd>...`** runs the command in
  the worktree root on its host: inside a workspace session that
  workspace, from the session's key and tags; elsewhere the record found
  as `path` finds it. stdout lines go to stdout and stderr lines to
  stderr, so `laatmux run proj/x -- go test ./... | tail` behaves, and
  the exit status is the process's; 255 means the outcome is unknown, a
  lost connection whose follow found the daemon no longer knew the run;
  130 is cancelled. Ctrl-C sends `cancel` and waits for the result; a
  second Ctrl-C gives up waiting, and since the cancel went on the
  connection open at the time, and during a reconnect there was none,
  the message says the run may still be going. A client that just
  disconnects leaves the run going, as an `add` keeps going.
- **`settle`** and **`unsettle`** set and clear `@laatmux_settled` on the
  workspace session they run from, or the one named.

## Upgrading a host

`laatmux upgrade <host>...` puts this checkout's build on a host and
restarts its daemon. It asks the host for `uname -sm`, builds for that
platform with `CGO_ENABLED=0` and the version from `git describe`, once
per platform per run (`--bin` installs a binary built elsewhere, `--src`
names the checkout when the command runs from another directory; flags
go before, between or after the hosts), then streams the binary over the
same ssh alias the client uses into an `sh` script: written to a fresh
temporary name beside the configured `bin`, checked to answer
`version` as laatmux does, one line with the protocol and nothing after
it, exit 0, which a build for the wrong platform, a truncated copy or
an empty file fails (an empty file would run as a shell script that
succeeds) and then leaves the working binary as it was, and renamed
over it, so the install is atomic, the running daemon keeps its own
inode and two installs at once do not share a file; then `laatmux stop`
with the new binary. The `bin` is the same shell word the bridge runs:
a path under `~` is the remote home, anything else one quoted word, a
bare name found on the remote PATH, so a bare name cannot be
reinstalled once the binary is gone, there being no path to put it at;
set `bin` to a path for that. `stop` reaches the daemon over its
socket, without starting one, and sends `shutdown`, capability
`shutdown`, which the daemon answers and then exits on as it does on
`SIGTERM`, cancelling its runs and waiting for them; the process that
ends is the one that answered the hello, never a pid a file remembers,
which a crash can leave for another process to inherit. The runtime
record is read once and its address dialled, and the daemon's hello
carries its pid, so a replacement that took the same socket path
between the read and the dial is seen and the record read again. A
daemon from before the message has no pid in its hello and gets
`SIGTERM` at the record's pid when the record still stands. A daemon
that holds the startup lock but answers on no socket is starting or
shutting down, and `stop` keeps trying to reach it while that holder has
the lock. Then `stop` waits for the lock to leave that daemon's hands,
released by the kernel when it exits, reaped or not, or taken by a
replacement a client started meanwhile. The next connection starts the new build, and
`upgrade` makes that connection last and prints the version. Nothing
before that connection needs a daemon on the host, so a host whose
daemon is stopped or whose binary is gone is upgraded too; the script
says what the old binary was. Each remote step is bounded, so a host
that stops answering is reported and skipped rather than holding the
others. The local host is upgraded in place of the running executable
the same way. `hosts` marks every
daemon whose build is not this client's, since versions are `git
describe` strings, equal or not, never ordered.

## Sidebar and dashboard

The listing, the sidebar and the dashboard show the same rows, built in
`internal/rows`: each host's worktrees joined with its agents by the
managed session the record names, then managed agents with no worktree,
then observed agents on other servers, then local workspace sessions
whose worktree is gone from a host that is connected, listed and
publishes worktrees. Local sessions are joined in by key, or by the
attach tag for a `new` session's attachment, so a row knows its local
session, whether it is settled, and whether it is the one the viewer is
in. A row is dim from measured axes only: no identified agent, an agent
that is gone, a host that is down, a stale session, a settled workspace.
Age is shown, never judged. The order is blocked, working, idle, then
rows without a live agent, most recent activity first within a group;
settled rows sit in a collapsed group at the bottom, stale rows after
them.

The view, in `internal/view`, is a tmux pane's worth of terminal: raw
mode through termios, ANSI for cursor, dim and reverse, SGR mouse
reporting for clicks and the wheel, no TUI library. The renderer is a
pure function from rows, size and selection to lines and is tested
against golden files for both layouts. `tiles` is three lines per row:
the mark and `<repo>/<branch>` with the host tag right-aligned and dim
for every host but the local one, the agent with its activity and age,
the pane title trimmed to the width; a row without an agent has two
lines, the second `no session`, `no agent` or `no worktree`. `compact`
is one line per row, two in the dashboard, which has room for the
title. Keys in both: `j` `k` and arrows move, `g` `G` first and last,
`Enter` jumps, `1`..`9` jump to the nth row of the selection's group,
`v` toggles the layout, `/` filters by name or host and `Esc` clears,
`f` shows and hides the settled and stale groups, `q` quits. A click
jumps to the row under it; the wheel moves the selection. Hosts that
are not connected and listed, and a local daemon that is down, are
lines above the list.

Jump from the view is `jump`'s logic in-process against the merged
records: a workspace row switches to its local session, creating it from
the record when missing; a `new` session's row does the same through a
plain attachment; an observed agent on this machine's default server is
a `switch-client`; one on a remote host's default server is refused with
`jump`'s message; a worktree with no session shows the `add` line that
would start one in the footer. A stale row's session exists locally and
is switched to.

- **`sidebar [toggle|on|off]`**, meant for a key binding. `on` sets five
  server hooks at indexes laatmux owns, `after-new-window[9101]` and
  `after-new-session[9102]` running `sidebar attach '#{window_id}'`,
  `pane-exited[9103]` and `after-kill-pane[9104]` running `sidebar reap`,
  and `window-resized[9105]` running `sidebar fit '#{window_id}'`,
  then walks every window on the default server and splits a pane off
  the left edge of each that has none, full height, at the configured
  width, running `sidebar pane`. The split is detached, so focus stays
  where it was, and the pane is tagged `@laatmux_sidebar` by the id
  `split-window -P` printed, never as the window's active pane: an
  `after-split-window` hook of the user's runs between the commands of a
  sequence and may select or split another pane. Every check-and-create
  runs under an exclusive flock on `$LAATMUX_HOME/sidebar.lock`, held
  from the check to the tag, so no `attach` sees the pane untagged; and
  `attach` reads the hooks under it and does nothing when they are gone,
  so an attach queued behind `off` puts no pane back. `reap` kills a sidebar pane that is alone in its
  window, counting a dead pane kept by `remain-on-exit`, such as a
  workspace's attach pane, as the window's. `fit` puts a window's
  sidebar back to the configured width: tmux shares a window's change of
  width out among its panes, so a session made detached, 80 columns
  wide, would otherwise widen its sidebar by a share of the terminal
  once a client switches to it, and so would resizing the terminal. In
  a window narrower than twice the width the sidebar gets half, when it
  is split off as when it is fitted. The width is the
  sidebar's own, set in the config: a border dragged by hand is put
  back at the next resize of the window. A zoomed window stays zoomed. A sidebar turned on by an older build gets the
  hook when `on` runs again. `off` unsets the five hooks and kills every tagged pane. `toggle` reads the hooks. The hook
  commands name the binary by its absolute path. `q` in a sidebar pane
  closes it; that window has no sidebar until a new window is made or
  `on` runs again.
- **`sidebar pane`** is one client of the local daemon's merged stream,
  in the configured layout, marking the session it sits in from
  `TMUX_PANE`, redrawing on every change and every five seconds for the
  ages, staying after a jump. It reads no local sessions itself: settled
  and stale come from the stream.
- **`dashboard`** is the same view filling whatever it runs in, compact
  with titles by default, `--layout tiles` otherwise. A jump exits, so
  under `display-popup -E` the popup closes:
  `bind-key C-s display-popup -E -w 90% -h 80% -d '#{pane_current_path}' -T ' laatmux ' 'laatmux dashboard'`.
  `-d` matters: the form's repository defaults to the repository of
  the directory the popup runs in, which without it is the session's.
  Actions: `a` opens the task form of
  [milestone four](docs/milestone-four.md): chips for the repository,
  the host and the agent, preselecting what `add` would take, a prompt
  box, and a branch line filled from the prompt as it is typed until
  it is edited. `Tab` and `Shift-Tab` move between the fields; on a
  chip `Left` and `Right` cycle and `Enter` opens the picker; in the
  prompt `Enter` submits and `Ctrl-J` inserts a newline. `Shift-Enter`
  and `Ctrl-Enter` insert one too where the terminal reports them as
  distinct keys, and are `Enter` and submit elsewhere: the view asks
  for xterm's modifyOtherKeys at level 1 and reads both the
  `CSI 27 ; m ; 13 ~` and the `CSI 13 ; m u` forms, which under tmux
  takes `extended-keys on` (the default is off) and the outer
  terminal's `extkeys` feature, for example
  `set -as terminal-features 'xterm*:extkeys'`, with either
  `extended-keys-format`; a terminal bound to send `ESC CR` for
  `Shift-Enter` gets a newline as well. `Esc` cancels,
  and an Alt chord, `Alt-Backspace` say, which the terminal sends as an
  escape and the key in one read, is not `Esc` and is dropped.
  Bracketed paste is on, so a pasted line break is a newline, never a
  submit, and a paste goes into the prompt wherever the focus is but
  the branch line. A paste whose bytes stop for a second is shown as
  far as it came, with its framing kept, and one whose end marker
  never comes is ended by `Esc` or `Ctrl-C` pressed alone after that
  and left for a second, the key spent on ending it. On a worktree row
  without a session the chips and the branch
  are pre-filled from the record, the branch explicit. A submit with
  the local daemon's `relay` hands the add to it and closes the popup
  on `accepted`; the task is a row in the views from then on, and in
  `laatmux tasks`; a refusal keeps the form up with the
  error. Without it
  the add runs in the foreground with its progress in place of the
  list and jumps on success, a failure staying until a key; a prompt
  that did not reach the agent, or may not have, or was refused before
  the host, comes up first in a scrollable notice with its text, kept
  as well in a file under `undelivered/` in the state directory, and
  the jump follows the notice; `Ctrl-C` while the add runs puts up a
  notice naming the same kept file, the add itself may or may not have
  been sent, and its dismissal ends the view. The footer
  says `tasks not supported by <host>'s daemon` for a host whose cached
  capabilities lack `task`. `x` confirms then removes
  the worktree; a refusal that asks for force carries the hint to use
  `X`. On a pending task's row `x` dismisses it instead, with a confirm
  line, where it needs the user or would otherwise be stuck, and `p`
  delivers a prompt that did not reach the agent, or may not have.
  `s` settles or unsettles; `S` opens the shell window and jumps.
  The commands are `internal/command`, the same implementations the
  CLI's `add`, `rm`, `run` and `shell` call, with the printing separated
  from the doing.
- **`compose`** is the form alone, for a binding from any window:
  `bind-key T display-popup -E -w 80% -h 60% -d '#{pane_current_path}' -T ' task ' 'laatmux compose'`.
  It exits on submit or cancel, the repository defaulting to the
  directory the popup was opened from.
- In both, and in `ls`, a background add from the form is a row of its
  own at the top of the main group, the newest first, until it hands
  over to its worktree row. Its mark spins while the add runs and is
  `!` once it needs the user, when the row is dim too. The second line
  in tiles, or the state column in compact, says where it is:
  `adding: <stage>`, `host unreachable, retrying`, `failed at <stage>`,
  `prompt not delivered`, `prompt delivery unknown`, `outcome unknown`,
  `done, awaiting the listing`, `done, worktree gone`, or `host
  removed`; the line under it, the title line in compact, has the
  detail or the reason. The worktree row at the same root is not drawn
  while a task for it stands, and the task's row takes its agent and
  session: `Enter` jumps once the add is done and does nothing while it
  runs, and refuses a task whose host is removed or now answers as
  another machine, or whose worktree is gone. A task that failed, or
  whose worktree was gone after the add, does not stand for the
  worktree row, so one made again at the root is drawn beside it. `p` and `x` on a task's row work in the sidebar as
  in the dashboard, the sidebar's only keys that act.
- In both, a working row's mark spins: braille frames in cyan, one per
  tenth of a second from the clock, so every pane spins in step; the
  view redraws at that rate only while a working row is on the list. A
  working agent that is gone, or on a host that is down, keeps the
  plain `*`, as `ls` always prints it.
- In both, the selection follows the viewer's own row, the session the
  sidebar pane sits in or the popup was opened from, wherever the sort
  moves it, and rests on nothing when no row is that session, so `Enter`
  does nothing and a digit counts the main group. The first key or
  wheel step that moves the selection makes it the user's: from then on
  it stays on the row it was put on across refreshes, as before. A
  task's row hands the selection to its worktree row when it hands
  over, through the handoffs the merged stream carries for a day even
  when the view missed the steps between. A selected row that goes with
  nothing to hand over to leaves the selection on none, not on the row
  that took its place, until the row is back or a key moves it.
- Both refuse a local daemon without `merged` with what to do; a sidebar
  per window is the case the capability exists for. `watch` stays the
  plain scrolling list for a terminal that is not a tmux pane.

Config: `sidebar: {width: 35, layout: tiles}`; width is at least 10,
layout `tiles` or `compact`.

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

## The merged stream

Every client used to dial every host: a sidebar pane per window would be
an ssh channel per host per window. The daemon on the machine the user
sits at is the one process there, so it is the merge point. A daemon
whose config has `hosts` advertises `merged`, and `subscribe` with
`merged: true` gets one stream with every host's records:

```
-> {type: subscribe, merged: true}
<- {type: snapshot, seq, hosts, agents, worktrees, sessions, sessions_error}
<- {type: upsert, seq, host_status: {...}}          a host's connectivity changed
<- {type: upsert, seq, agent | worktree: {...}}      as before, from any host
<- {type: upsert, seq, local_session: {...}}         a local workspace session changed
<- {type: remove, seq, host_name | agent_id | worktree_id | local_session_name}
```

- **Host records** `{name, ssh, environment_id, connected, listed, error,
  version, capabilities, since}` are the connectivity axis, one per
  configured host. `connected` is a live connection with a completed
  hello; `listed` is that the host's records come from a snapshot of that
  connection. Records from before a drop stay while `listed` is false,
  so a listing shows what was last known with the host row saying `DOWN`
  and ssh's own message. Absence is authoritative only when both bits are
  set: a local session is stale only against a host that is connected,
  listed and publishes worktrees, and `jump` reports a workspace missing
  only from a listed host. The local host is itself, not a dial of its
  own socket; it is listed once the daemon's first poll of every server
  and of git is complete, and a merged subscription does not wait for
  that as a plain one does. Records are forwarded unchanged, ids included; a client maps
  a record's `environment_id` to a host name through the host records.
  `seq` is the merging daemon's own.
- **The relay**, capability `relay`, the laptop's side of
  [milestone four](docs/milestone-four.md): `{type: add, id, relay:
  <host>, repo, name, branch, generated, agent_name, cmd, prompt,
  submitted_at}` is written to `<state>/pending/<id>.json`, mode 0600
  with the prompt, before the answer says `accepted`; the daemon then
  dials the host as the client would have, sends the add under the
  client's id and follows it to the result, reconnecting with the merged
  stream's backoff for as long as the task is outstanding, and to the
  host's listing after it. Every connection is pinned to the host's
  environment id, from the host row at accept or from the first hello,
  and requires `add`, `follow` and `task`; a host known to lack `task` is
  refused at accept. The pending records travel in the merged stream:
  `pendings` and `handoffs` in a snapshot, `pending` in an upsert,
  `pending_id` with `replaced_by` in a remove. A record carries `taken`
  (the host has the add), `reachable` with `unreachable` saying why not
  and `mismatch` when another machine answers under the host's name,
  the last progress line's `stage`, `state` and `detail`, `branch` and
  `root` as the host reports them, then `done` with `ok`, `error`,
  `prompt` (the delivery state), `attempt`, `attempt_open` and
  `attempt_error` (the host's refusal of the last attempt), `listed`,
  `listing_error` and `gone`. A record is complete when the add succeeded and the prompt
  is delivered or there was none; the relay takes the host's listings
  until one passes the result's barrier, and a complete record whose
  worktree is in it is retired: the handoff is written to the file,
  which is kept a day, then the remove is published with `replaced_by`,
  the worktree row's id. No worktree at the root is `gone`, a record
  the user dismisses. A record that needs the user stays, prompt
  retained, until `{type: dismiss, id}` or until `{type: prompt, id}`
  delivers it; its worktree removed meanwhile, by `rm` here or
  elsewhere or by hand, makes it `gone` too: a removal the host reports,
  and a host's successful listing that lacks the worktree, a
  reconnect's snapshot or a later poll's, have the host asked again, as
  the handoff asks it, until it answers; a remote host's listings are
  followed while a view is subscribed. `rm` drops the finished tasks at the worktree it
  removed, through `{type: dismiss, environment_id, root}` to a local
  daemon that is running and has the capability `dismiss-root`. Until then, `{type: prompt, id}`
  delivers it as the next attempt, written to the file first, one
  unresolved at a time; `recovery expired` from the host ends that, and
  `laatmux tasks show <id>` prints the prompt for pasting by hand. An add
  older than seven days is not sent and is `outcome unknown`, as is one
  the host refuses as `submission expired`; a follow answered
  `interrupted` or `unknown command` resends the add. A daemon that
  starts resumes every file: adds without an outcome, open attempts
  first, listings owed. A host gone from the config leaves its records
  waiting with `host removed`. `laatmux add --detach` submits; `laatmux
  tasks` lists.
- **Hosts follow the config file.** The daemon re-reads `hosts` on every
  merged subscription, so a host added shows up on the next `ls`; one
  removed gets a `remove` for its records and then its host record.
- **Held only while wanted.** Remote subscriptions are opened by the
  first merged subscriber and dropped 60 seconds after the last leaves,
  so a laptop with no sidebar open holds no ssh channels; each remote
  daemon sees one subscriber per laptop whatever the laptop shows. A host
  that is down is redialled with the backoff `watch` used, 1 s doubling
  to 30 s.
- **Local sessions** are listed by the daemon while it has a merged
  subscriber, once a second against the default server, and published as
  `{name, key, host, source, branch, attach, settled}`, so a settle
  reaches every subscriber within a second and a view needs no tmux
  access of its own. The listing also runs once, synchronously, before
  each merged snapshot, so the snapshot is as fresh as the connection. A
  listing that fails for a reason other than no server puts its message
  in `sessions_error`, which `ls` prints where the settled and stale
  groups would be.
- **One-shot clients wait for readiness.** The snapshot comes at once
  with what the daemon knows, on a cold daemon the host rows alone. `ls`
  reads on until every host is listed or carries an error, or 20 seconds
  have passed, and marks the hosts still neither. `jump`, `path`, `rm`
  and `run` wait the same way for the one host they act on and then talk
  to that host directly, as before. A connection that was up and dropped
  is not an error to stop on: the host record carries `reconnecting`
  from the drop until the next dial's outcome, and a client waits on it
  as on a host still connecting, since a daemon restarted for an upgrade
  is back within seconds; ssh's own errors, a refused connection or a
  missing binary, end the wait at once as before. The header line says
  `DOWN disconnected (reconnecting)` meanwhile.
- **Older daemons.** A local daemon without `merged` is an older build
  still running; `ls`, `watch`, `jump`, `path` and `rm` fall back to
  dialling each host. Plain `subscribe` still means this host's own
  records, which is what a remote daemon serves to the merging one.

## Model

- Three status axes, never collapsed: **activity** from the screen (working,
  blocked, idle, unknown), **liveness** of the identified process (alive, gone),
  and **host connectivity**, which is client side: the merging daemon's
  host records, never a field of an agent record.
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

## Milestone three, step 2, on the VM

The merged stream, with an isolated daemon (`LAATMUX_HOME=.spike`, a
scratch copy of the config with hosts `mac` and `vm`) and the VM's daemon
an older build without `merged`. `ls` through the local daemon listed
both hosts in 1.8 s cold and left one `ssh -T ... laatmux bridge` behind;
twenty `watch` processes at once held that one channel and one bridge on
the VM. A host with an unresolvable alias appended to the config file
showed up on the next `ls` and in every running `watch` as `DOWN` with
ssh's own message, and was gone after its removal. Killing the bridge on
the VM turned its row `DOWN  disconnected` with its worktrees still
listed, then `connected (snapshot pending)`, then `connected`, within the
backoff. `jump` on a worktree without a session answered in 25 ms from
the stream with the `add` hint; `jump` on an unknown session was refused
by the direct preflight as before. Seventy seconds after the last `watch`
was killed there was no ssh channel on the laptop and no bridge on the
VM, and the next `ls` reconnected in 1.3 s.

## Milestone three, step 3, under tmux

The sidebar and the dashboard, on an isolated default server
(`TMUX_TMPDIR` pointed at a scratch directory) started with the user's
tmux.conf and given a hook like their sidebar's that splits a pane off
every new window and session, against the laptop's real daemon. `sidebar
on` in a server with two windows set the four hooks and left each window
with a 35-column tagged pane on the left running `sidebar pane`, focus on
the pane that had it, in 0.8 s, also with an `after-split-window` hook
that selects the next pane. A new window and a new session each got
a sidebar through the hooks with the other hook's pane beside it; a
second `attach` on a window that has one changed nothing. `hook_window`
and `hook_session` expand to nothing in after-hooks on tmux 3.6a, so the
hooks use `window_id`, which is the new window in both. In the pane, `j`
moved the reverse-video selection, `v` switched to compact, `Enter` on a
worktree without a session put the `add` line in the footer, `/bro`
filtered to the one matching row and `Esc` restored the list, host tags
for the remote host were dim. Killing a window's other panes ran `reap`
and the window was gone within a second. `off` removed the hooks and
every tagged pane. `dashboard` in a window drew the compact layout with
the hint line, kept the `add` message on a failed jump, and `q` closed
it. `ls` printed as before from the shared rows.

## Milestone three, step 5, on the VM

With the VM's daemon rebuilt and a scratch laptop config naming its
repositories, from the laptop: `run proj/step5 --host vm -- sh -c ...`
printed the stdout line to stdout and the stderr line to stderr, the
VM's hostname and the worktree root, and exited 4 as the command did.
Ctrl-C during `sleep 100` printed `cancelling`, then `cancelled`, exit
130, and no sleep was left on the VM. During a 12-line one-per-second
run, `pkill -f "laatmux bridge"` on the VM made the client print
`connection lost (EOF); reconnecting to follow run`; the output had all
twelve lines once, in order, then `done`, exit 0. `SIGTERM` to the VM's
daemon mid-run ended the run as `cancelled`, exit 130, with the process
gone and the next client restarting the daemon. `rm proj/step5 --force`
during a run returned ok after the run's client had printed
`cancelled`, and the worktree was gone. On an isolated laptop daemon
the same held, plus: inside a workspace session `run -- pwd` printed
the root with no target named; `SIGKILL` to the daemon mid-run made the
client reconnect, start a fresh daemon, follow, and exit 255 with the
outcome-unknown message while the process kept running, as documented;
`split -h` on the workspace's pane, by pane id with `TMUX` set as a
`run-shell` job has it, opened a pane with its shell at the worktree
root while the attach pane stayed where it was, and on a plain session
opened one in the pane's directory; the bound form
`run-shell "laatmux split -h '#{pane_id}'"` did the same.

## Not yet verified

Nothing in milestone one's acceptance list. Mouse passthrough was checked with
injected reports, not a physical click.
