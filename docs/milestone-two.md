# Milestone two: worktrees and workspaces

Design note for the `add` path from issue #1 and its workflow comment. It
pins down what `add` needs before it is built: the config, the workspace
record, the protocol, and how the local tmux session becomes the workspace.
Milestone one observes agents; this milestone creates them.

## The workflow

1. Pick a repository from the configured set.
2. Pick a host where that repository has a path.
3. Pick an agent.
4. Name the task. That is the branch and the worktree name.
5. The daemon on that host creates the worktree, copies files, runs setup,
   and starts the agent in a managed session.
6. A session opens in the laptop's own tmux with the agent attached.

Defaults make the usual path `laatmux add <task>`: the repository from the
current directory, the host and agent last used for that repository.

## Config

Personal config in `~/.config/laatmux/config.yaml`, next to `hosts` and
`tmux_servers`:

```yaml
agents:
  claude:
    cmd: [claude]
  codex:
    cmd: [codex]
  claude-safe:
    cmd: [claude-safe]        # sandboxing is the launch command's business

repos:
  laatmux:
    paths:                    # main checkout per host name
      mac: ~/code/laatmux
      vm: ~/src/laatmux
    worktrees: .worktrees     # under the main checkout; this is the default
    host: mac                 # default host; optional
    agent: claude             # default agent; optional
```

Repository paths and agent commands are personal because they differ per
person and per host. The daemon on a host reads the same file that host
has, and finds its own paths under its own host name, the entry in `hosts`
without `ssh`.

Host names and repository keys are labels the user picks, and they end up
in session names, workspace keys and ids, so `Load` validates them: one or
more of `A-Z a-z 0-9 _ -`, nothing else. A host whose name defaults from
its ssh alias must satisfy the same rule, else the config is rejected with
a message asking for an explicit `name`. `agents` keys follow the same rule.
With `/`, `.`, `:` and `%` excluded from labels, a `/`-joined name or id is
parsed unambiguously from the left, and only the branch needs encoding.

Shared setup in `.laatmux.yaml` at the repository root, committed:

```yaml
copy: [.envrc, .env.local]            # from the main checkout, skipped when present
setup: ["pnpm install"]               # each runs at least once; must tolerate a rerun
```

It is read from the new worktree after checkout, so a branch carries its own
setup. A missing file means nothing to copy and nothing to run.

Last-used host and agent per repository are state, not config. They live in
`$LAATMUX_HOME/last.json` on the laptop and are written by `add`.

Defaults are deterministic or absent, never "the first" of a YAML mapping.
Host: the flag, else `last.json`, else the repository's `host`, else the
local host if the repository has a path there, else the only host that has
one, else an error naming the candidates. Agent: the flag, else `last.json`,
else the repository's `agent`, else the only configured agent, else an
error naming them.

## The workspace record

A workspace is one worktree with one managed agent session and one local
session. The record is explicit; nothing is inferred from the agent's
current directory.

| Field | Where it is authoritative |
|---|---|
| repo, branch, root | the host's git: `git worktree list --porcelain` in each configured repository, filtered to its `worktrees` directory; branch is empty for a detached worktree |
| host | the daemon that answers, by environment id |
| managed session | the host's managed tmux server: the pane carries `@laatmux_repo`, `@laatmux_branch` and `@laatmux_cwd`, set at creation as `@laatmux_cwd` is today |
| local session | the laptop's default tmux server: the session carries `@laatmux_workspace` = `<environment_id>/<repo>/<branch>` and `@laatmux_host` = the configured host name |
| last-used defaults | `$LAATMUX_HOME/last.json` on the laptop |

The workspace key is the environment id, not the host name: the id is minted
once and survives a renamed ssh alias or host entry, so a local session
still matches its workspace after the config changes. The host name is for
routing and display.

No workspace file is kept on either side. Git is the source of truth for
worktrees, as the model says, so a worktree made by hand in the right
directory is listed and can be removed, and a worktree removed by hand
disappears from the listing. The daemon lists worktrees the same way it
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
<- {type: result, id, ok, error, session, pane_id, root}
```

Stages, in order. Every step that mutates something has its own check, so
a retry after a crash skips exactly what is done and finishes what is not:

| Stage | Step | Skipped when |
|---|---|---|
| resolve | repository path for this host, worktree root, agent command | never |
| fetch | `git fetch origin` in the main checkout | never; it is cheap and the branch base must be fresh |
| worktree | `git remote set-head origin --auto` | never; it is a symref update |
| | `git worktree add -b <branch> <root> origin/HEAD` | root is a registered worktree on that branch |
| | the `worktrees` directory line in `.git/info/exclude` | the line is present |
| copy | each `copy` entry from the main checkout, written to a temporary file in the target directory and renamed into place | the target exists; it can only exist complete |
| setup | each `setup` command in the root, in order, output streamed as detail; after each success a marker named by the command's index and hash is written under the worktree's git directory | that command's marker exists; a changed command has a new hash and runs again |
| agent | one tmux invocation: `new-session` and every `set-option` for the pane, `\;`-separated, so the session is never observable without its options | a session of that name exists with one pane carrying matching `@laatmux_repo`, `@laatmux_branch` and `@laatmux_cwd` |

Details behind the checks. `git fetch origin` does not update
`origin/HEAD`, so `set-head --auto` refreshes it and the branch base is the
remote's current default branch, not the one recorded at clone time. The
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
Today's `NewSession` issues separate commands and changes accordingly. A
session of that name with anything other than one pane carrying the
expected options fails the stage as a name in use; nothing is adopted.

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
Git is the judge of whether a worktree may go, and nothing is killed until
it has gone: `rm` runs `git worktree remove <root>` first, with
`--force --force` when `force` is set, and only after git has removed the
directory does it kill the managed session. Without `force`, git refuses a
dirty worktree, untracked files, a locked worktree and a submodule the same
way it does at the command line, and the refusal is returned as the error
with the agent still running. With `force`, all of those are removed. A
target that is already absent, no worktree and no session, is `ok`, so a
retry after a dropped bridge is a no-op and the client can go on to its own
cleanup. Ids replay as for `add`. The branch is left alone; merging,
rebasing and deleting branches stay ordinary Git.

The local workspace session is the client's to clean up: after an `ok`
`rm` the client kills the tagged local session, switching away first if it
is the current one. A tagged local session whose workspace no longer exists
on its host, because the worktree was removed by hand or from another
machine, is listed by `ls` as stale, and `rm` on a stale entry kills the
local session and nothing else.

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
2. Config: `agents`, `repos`, `.laatmux.yaml`, `last.json`, with a
   `laatmux repos` command that shows how each repository resolves per host.
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
