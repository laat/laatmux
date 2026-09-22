# Milestone two: worktrees and workspaces

Design note for the `add` path from issue #1 and its workflow comment. It
pins down what `add` needs before it is built: the config, the workspace
record, the protocol, and how the local tmux session becomes the workspace.
Milestone one observes agents; this milestone creates them.

## The workflow

1. Pick a repository from the configured set.
2. Pick a host. The repository is cloned there on first use.
3. Pick an agent.
4. Name the task. That is the branch and the worktree name.
5. The daemon on that host creates the worktree, copies files, runs setup,
   and starts the agent in a managed session.
6. A session opens in the laptop's own tmux with the agent attached.

Defaults make the usual path `laatmux add <task>`: the repository from the
current directory, the host and agent last used for that repository.

## Config

Personal config in `~/.config/laatmux/config.yaml`. More is configured per
host and less per repository: a repository is just a known source, and each
host says once where checkouts and worktrees go.

```yaml
hosts:
  - name: mac
    repos: ~/code             # main checkouts live in <repos>/<name>
    worktrees: ~/worktrees    # worktrees live in <worktrees>/<name>/<branch>
  - name: vm
    ssh: vm
    repos: ~/src
    worktrees: ~/src/worktrees

tmux_servers: [laatmux, default]

agents:
  claude:
    cmd: [claude]
  codex:
    cmd: [codex]
  claude-safe:
    cmd: [claude-safe]        # sandboxing is the launch command's business

repos:                        # the known set; a list for now, discovery later
  - git@github.com:laat/laatmux.git
  - https://github.com/laat/other.git
```

A repository is identified by its source. Its name is a label derived from
the source, deterministically, so every host and every run derive the same
name from the same list:

1. The last path component without `.git`: `laatmux`, `other`.
2. If two sources share that, each of them is prefixed with its org, the
   path component before the name, joined with `-`: `laat-laatmux` and
   `acme-laatmux`.
3. If they still collide, or a source has no org component, the name is
   suffixed with the first six hex digits of the source's SHA-256:
   `laatmux-3fa9c1`. The hash is stable where a random suffix would differ
   per host and per run, and the name is a directory on every host.
4. A list entry may be a mapping with an explicit `name` next to `source`,
   which wins over all of the above.

Names that come out of this are validated like every other label, and
`Load` rejects the list when two entries have the same source or, after
explicit names and derivation, the same final name. Explicit names are not
exempt: they win the derivation but must still be unique.

Identity is the source and not the name, so adding a source that forces an
existing repository's name to change is harmless. The rule that makes this
hold everywhere: **the label is used only to place new things**. Existing
things are found by inspection and keep their paths. The daemon finds a
repository's checkout under `<repos>` by its `origin`, not by its directory
name. It finds the worktree for a branch by asking that checkout's
`git worktree list`, not by computing a path. It finds the managed session
for a worktree by the root recorded on the pane, not by the session name.
The laptop finds the local session for a workspace by the root in its tag,
not by the repository label in its name. After a rename, all of those still
resolve; only new clones, new worktrees and new session names use the new
label, and `ls` shows the new label next to the old paths.

A repository's main checkout on a host is `<repos>/<name>` when the daemon
clones it, which it does if no checkout under `<repos>` has the source as
`origin` when `add` runs there, so any host can be picked. A worktree's root
is `<worktrees>/<name>/<branch>`, outside the checkout, so nothing needs
excluding from git status. Per-repository overrides of these paths are not
in this milestone and can be a list added later. `repos` and `worktrees`
have no defaults: a host without them cannot `add`, and the error names the
host.

The daemon on a host reads the same file that host has, and finds its own
directories under its own entry, the one in `hosts` without `ssh`. Paths
are expanded for `~` on that host.

Host names, repository names and agent keys are labels that end up in
session names, workspace keys, ids and directory names, so `Load` validates
them: one or more of `A-Z a-z 0-9 _ -`, nothing else. A host whose name
defaults from its ssh alias must satisfy the same rule, else the config is
rejected with a message asking for an explicit `name`. A repository whose
derived name does not conform is rejected the same way. With `/`, `.`, `:`
and `%` excluded from labels, a `/`-joined name or id is parsed
unambiguously from the left, and only the branch needs encoding.

Shared setup in `.laatmux.yaml` at the repository root, committed:

```yaml
copy: [.envrc, .env.local]            # from the main checkout, skipped when present
setup: ["pnpm install"]               # each runs at least once; must tolerate a rerun
```

It is read from the new worktree after checkout, so a branch carries its own
setup. A missing file means nothing to copy and nothing to run. Each `setup`
entry is one string run as `sh -c <string>` in the worktree root, with the
`.laatmux.yaml` author responsible for its quoting; this matches how such
commands are written and keeps the file free of argv arrays. `agents.cmd`
stays an argv array because laatmux builds the tmux command from it.

Last-used host and agent per repository are state, not config. They live in
`$LAATMUX_HOME/last.json` on the laptop and are written by `add` under an
exclusive `flock` around the read, modify and write, with the write going to
a temporary file renamed into place. Two concurrent `add`s therefore keep
each other's defaults, and a crash mid-write leaves the old file.

Defaults are deterministic or absent, never "the first" of a YAML mapping.
Host: the flag, else `last.json`, else the local host if it has `repos` and
`worktrees`, else the only host that has them, else an error naming the
candidates. Agent: the flag, else `last.json`, else the only configured
agent, else an error naming them.

## The workspace record

A workspace is one worktree with one managed agent session and one local
session. The record is explicit; nothing is inferred from the agent's
current directory.

| Field | Where it is authoritative |
|---|---|
| repo, branch, root | the host's git: `git worktree list --porcelain` in each known repository's checkout, found under `<repos>` by `origin`, filtered to roots under `<worktrees>/`; branch is empty for a detached worktree; `prunable` entries are not published |
| host | the daemon that answers, by environment id |
| managed session | the host's managed tmux server: the pane carries `@laatmux_cwd` = the root, set at creation as today; repo and branch are not stored on the pane, they come from the worktree record with that root |
| local session | the laptop's default tmux server: the session carries `@laatmux_workspace` = `<environment_id>/<root>` and `@laatmux_host` = the configured host name |
| last-used defaults | `$LAATMUX_HOME/last.json` on the laptop |

The workspace key is `<environment_id>/<root>`: the environment id is
minted once and survives a renamed ssh alias or host entry, and the root is
the one path git registered and never changes for the life of the worktree.
Neither the host name nor the repository label is in the key, so a local
session still matches its workspace after either is renamed. Both labels
are for routing and display, and the client resolves a `<host>/<repo>/<branch>`
target to a key through the current records at the time of the command.

No workspace file is kept on either side. Git is the source of truth for
worktrees, as the model says, so a worktree made by hand in the right
directory is listed and can be removed, and a worktree removed by hand
disappears from the listing. A worktree whose directory was deleted outside
git stays registered as `prunable`; the poller treats those as absent and
does not publish them, and `add` runs `git worktree prune` before its
registration check so a stale registration never counts as done. The daemon lists worktrees the same way it
lists panes: by polling, published to subscribers as `worktree` records
next to `agent` records, so `ls` shows a worktree whose agent has exited or
was never started. A detached worktree in the directory is listed with an
empty branch; `add` never makes one, and it has no managed or local session
name, so it is addressed by its root.

### Session names

tmux rejects `.` and `:` in session names, and a branch may contain both
(`fix/v1.2`). Host and repository labels are already restricted by config
validation; the branch is encoded injectively: `%` becomes `%25`, `.`
becomes `%2e`, `:` becomes `%3a`, nothing else changes. Distinct branches
give distinct names (`a.b` and `a-b` stay apart) and the decode is exact,
though the options above, not the name, are what records are matched on.
Because labels cannot contain `/`, the first components of a name are the
labels and everything after them is the branch, slashes included.

- Managed session, on its host: `<repo>/<encoded branch>`. One managed server
  per host, so the host is implicit.
- Local workspace session, on the laptop: `<host>/<repo>/<encoded branch>`.
  The host is part of the name because the same repository and branch may be
  checked out on two hosts at once, and tmux refuses a duplicate name.

## Protocol

New capabilities `worktrees`, `add` and `rm`. A client without them falls
back to milestone-one behaviour.

`add` is a command with streamed progress:

```
-> {type: add, id, repo, branch, agent, cmd: [...]}
<- {type: progress, id, stage, state, detail}   zero or more
<- {type: result, id, ok, error, stage, session, pane_id, root}
```

`stage` in a result is set on failure to the stage that failed, so a client
never parses it out of `error`.

Stages, in order. Every step that mutates something has its own check, so
a retry after a crash skips exactly what is done and finishes what is not:

| Stage | Step | Skipped when |
|---|---|---|
| resolve | checkout: the directory under `<repos>` whose `origin` is the source, else `<repos>/<name>` to be cloned; root: the worktree `git worktree list` in that checkout already has for the branch, else `<worktrees>/<name>/<branch>` for a new one; agent command | never |
| clone | `git clone <source> <checkout>` | a checkout with the source as `origin` exists; `<repos>/<name>` existing with a different origin fails the stage rather than being reused |
| fetch | `git fetch origin` in the checkout | never; it is cheap and the branch base must be fresh |
| worktree | `git remote set-head origin --auto`, `git worktree prune` | never; symref update and cleanup |
| | branch: `git branch --track <branch> origin/<branch>` if the remote branch exists, else `git branch <branch> origin/HEAD` | the local branch exists, from an earlier attempt or made by hand; it is used as is and the progress line says so |
| | registration: `git worktree add <root> <branch>` | root is registered on that branch; registered on another branch, or the branch is checked out elsewhere, fails the stage |
| copy | each `copy` entry from the main checkout, written to a temporary file in the target directory and renamed into place | the target exists; it can only exist complete |
| setup | each `setup` command in the root, in order, output streamed as detail; after each success a marker named by the command's index and hash is written under the worktree's git directory | that command's marker exists; a changed command has a new hash and runs again |
| agent | one tmux invocation: `new-session` and `set-option` for the pane, `\;`-separated, so the session is never observable without its option | a managed session exists whose single pane carries `@laatmux_cwd` equal to the root, whatever its name; a session with the intended name whose pane records another root fails the stage as a name in use |

Details behind the checks. Branch creation and worktree registration are
two steps because `git worktree add -b` is not atomic: a failure to create
the directory leaves the branch behind, and a retry with `-b` would then
fail on the existing branch. Creating the branch first and registering
without `-b` makes the retry pick the branch up. It also gives `add` on an
existing branch, local or remote, for free, which is the ordinary way to
open a worktree for a branch someone else started. `git fetch origin` does
not update `origin/HEAD`, so `set-head --auto` refreshes it and the branch
base is the remote's current default branch, not the one recorded at clone
time. The
worktree's git directory is what `git -C <root> rev-parse --git-dir`
returns, never derived from the branch name: git picks it, `feature/task`
gets `.git/worktrees/task`, and duplicate basenames get suffixes. The
markers live in a `laatmux/` subdirectory there and die with the worktree.

Setup is at-least-once, and the note says so where the commands are
configured. A marker proves a command completed; a crash after a command's
effects but before its marker reruns that command on retry. Setup commands
must therefore tolerate a rerun and a partial earlier run (`pnpm install`
does; a script that appends seed data does not, and needs its own guard).
laatmux does not try to make arbitrary commands idempotent.

The agent stage creates and tags in one tmux command sequence. tmux runs the
sequence to completion once it has been submitted, whatever happens to the
daemon, so there is no window in which a managed session exists untagged.
Today's `NewSession` issues separate commands and changes accordingly. The
check is by root, not by name: a worktree made before a label change keeps
its session under the old name, and a repeat `add` finds it. A session with
the intended name whose pane records another root, or that is not a
single managed pane, fails the stage as a name in use; nothing is adopted.

A failed stage stops the sequence with `ok: false` and the stage name; the
worktree is left in place for a retry. The command id is client chosen. The
daemon keeps recent ids with their outcome for a few minutes: a repeated id
while the command runs attaches to the running stream, and a repeated id
after it finished replays the result. A dropped bridge during `add` is
therefore retried by sending the same message again.

Commands are serialized per repository inside the daemon. Two `add`s for
the same workspace with different ids do not race: the second waits for the
first, then runs its own inspection and skips everything. `fetch` and
`worktree add` write to the same main checkout, so per-repository is the
right grain; different repositories proceed in parallel. There is one
daemon per host by the startup lock, so the lock is in-process.

`rm`:

```
-> {type: rm, id, repo, branch, root, force}
<- {type: result, id, ok, error}
```

`root` is an alternative to `repo` and `branch` for a detached worktree.
Removal is two steps, each inspected, so a retry after a crash between them
finishes the job:

| Step | Does | Skipped when |
|---|---|---|
| worktree | `git worktree remove <root>`, with `--force --force` when `force` is set | the root is not a registered worktree |
| session | kill every managed session whose pane carries `@laatmux_cwd` equal to the root | none does |

Git is the judge of whether a worktree may go, and nothing is killed until
it has gone. Without `force`, git refuses a dirty worktree, untracked files,
a locked worktree and a submodule the same way it does at the command line,
and the refusal is returned as the error with the agent still running. With
`force`, all of those are removed. The session step matches on the pane's
recorded root, never on the session name, so a session someone made by hand
with the same name is left alone, and a session left behind when the daemon
died after the worktree step is found and killed on the retry. A target
where both steps skip is `ok`, so a retry after a dropped bridge is a no-op
and the client can go on to its own cleanup. Ids replay as for `add`, and
a retry after a daemon restart, when the id cache is gone, is safe for the
same reason. The branch is left alone; merging, rebasing and deleting
branches stay ordinary Git.

The local workspace session is the client's to clean up: after an `ok`
`rm` the client kills the tagged local session, switching away first if it
is the current one. A tagged local session whose workspace no longer exists
on its host, because the worktree was removed by hand or from another
machine, is listed by `ls` as stale, and `rm` on a stale entry sends the
`rm` to the host anyway, which kills any matching managed session and
returns `ok`, then kills the local session. An agent whose recorded root is
no longer a worktree shows in `ls` as orphaned for the same reason.

`worktrees` is not a request. Worktree records arrive in the subscription
stream like agents. The record:

```
{id, environment_id, repo, branch, root, session, updated_at}
```

`id` is `<environment_id>/worktree/<root>`: the root is absolute and unique
on its host, exists for detached worktrees, and the environment id is hex,
so the id parses from the left. It is stable for the life of the worktree
and opaque to clients. `session` is the managed session name when one
exists for it, else empty, so `ls` can pair the two records without
matching on cwd. The envelope grows three fields alongside the agent ones:
`worktrees` in a snapshot, `worktree` in an upsert, `worktree_id` in a
remove. A milestone-one client ignores them; a milestone-two client reads
the `worktrees` capability before expecting them.

## Client

```
laatmux add <branch> [--repo r] [--host h] [--agent a] [-- <cmd>...]
laatmux rm  <repo>/<branch> [--host h] [--force]
laatmux rm  --root <path> --host h [--force]         # a detached worktree
laatmux path <repo>/<branch> [--host h]
laatmux ls
```

`add` resolves the repository from `--repo`, else from the current
directory being inside a configured path for the local host, else fails.
Host and agent follow the default order in the config section. Progress
prints one line per step. On success the client creates the local workspace session in the
default tmux server with one window running the attach command that `jump`
uses today, tags it with `@laatmux_workspace`, and switches to it. Outside
tmux it creates the session detached and prints how to attach.

`ls` merges agents and worktrees per host. A worktree without an agent shows
as such, which is the state after the agent exits or when the worktree was
made by hand. Settled workspaces keep their agent and are only grouped
differently, see below.

## Jump and the workspace session

Today `jump` opens an attach window in whatever session it runs from. Under
this model the workspace session is the unit: `jump <host>/<repo>/<branch>`
switches to the local workspace session, creating it from the record if it
is missing. The per-window attach in the current session goes away. The
managed side keeps one session, one window, one pane.

## Shell hotkey

`laatmux shell`, meant to be bound in the user's tmux config, runs inside a
workspace session and opens a shell at the worktree root on the worktree's
host: a local window started in the root, or an ssh window. The remote
command is built with the same quoting `jump` uses for its attach command:
`cd` and the single-quoted root, then a literal `&& exec "$SHELL" -l` so
the root passes through as one argument whatever it contains and `$SHELL`
expands on the remote side. The window is tagged `@laatmux_shell`; a second
invocation selects it rather than opening another. Outside a workspace
session it says so.

## Settle

`settle` and `unsettle` set and clear `@laatmux_settled` on the local
workspace session. `ls` and the sidebar list settled workspaces in a
collapsed section, each still paired with its agent. Nothing else changes:
the worktree, the managed session and the agent stay. Automatic resurfacing
is not decided here.

## Order of work

1. This note, and the issue 3 decision recorded in issue #1.
2. Config: per-host `repos` and `worktrees`, `agents`, the `repos` list,
   `.laatmux.yaml`, `last.json`, with a `laatmux repos` command that shows
   each known repository's name and where it lands on each host.
3. Daemon: worktree polling and records, then `add` with staged progress,
   verified on the VM against a hostile checkout state: existing worktree,
   dirty setup, crashed mid-copy.
4. Client: `add` with the local workspace session, then `rm` and `path`, and
   `jump` switched to workspace sessions.
5. `shell`, `settle`, `unsettle`.

## Out of scope

Merge, rebase, push, review. Interactive pickers, which belong with the
dashboard popup in milestone three. Syncing worktrees between hosts. Any
reading of workmux config, state or hooks.
