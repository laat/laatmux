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

Shared setup in `.laatmux.yaml` at the repository root, committed:

```yaml
copy: [.envrc, .env.local]            # from the main checkout, skipped when present
setup: ["pnpm install"]               # run once in the new worktree
```

It is read from the new worktree after checkout, so a branch carries its own
setup. A missing file means nothing to copy and nothing to run.

Last-used host and agent per repository are state, not config. They live in
`$LAATMUX_HOME/last.json` on the laptop and are written by `add`.

## The workspace record

A workspace is one worktree with one managed agent session and one local
session. The record is explicit; nothing is inferred from the agent's
current directory.

| Field | Where it is authoritative |
|---|---|
| repo, branch, root | the host's git: `git worktree list --porcelain` in each configured repository, filtered to its `worktrees` directory |
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
was never started.

### Session names

tmux rejects `.` and `:` in session names, and a branch may contain both
(`fix/v1.2`). Both session names use one injective encoding of the branch:
`%` becomes `%25`, `.` becomes `%2e`, `:` becomes `%3a`, nothing else
changes. Distinct branches give distinct names (`a.b` and `a-b` stay apart)
and the decode is exact, though the options above, not the name, are what
records are matched on.

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

Stages, in order, each idempotent by inspection so a retry after a crash
skips what is done:

| Stage | Does | Skipped when |
|---|---|---|
| resolve | repository path for this host, worktree root, agent command | never |
| fetch | `git fetch origin` in the main checkout | never; it is cheap and the branch base must be fresh |
| worktree | `git remote set-head origin --auto`, then `git worktree add -b <branch> <root> origin/HEAD`; adds the `worktrees` directory to `.git/info/exclude` | root exists as a worktree on that branch |
| copy | each `copy` entry from the main checkout, written to a temporary file in the target directory and renamed into place | the target exists; it can only exist complete |
| setup | each `setup` command in the root, in order, output streamed as detail; after each success a marker named by the command's index and hash is written under the worktree's git directory | that command's marker exists; a changed command has a new hash and runs again |
| agent | `new` onto the managed server with the root as cwd, the agent command, `LAATMUX_AGENT` set, and the pane options above | a session of that name exists with one pane carrying matching `@laatmux_repo`, `@laatmux_branch` and `@laatmux_cwd` |

Two details behind the skip rules. `git fetch origin` does not update
`origin/HEAD`, so `set-head --auto` refreshes it and the branch base is the
remote's current default branch, not the one recorded at clone time. The
worktree's git directory is what `git -C <root> rev-parse --git-dir`
returns, never derived from the branch name: git picks it, `feature/task`
gets `.git/worktrees/task`, and duplicate basenames get suffixes. The
markers live in a `laatmux/` subdirectory there and die with the worktree.

The agent stage has two more outcomes besides skip. A session of that name
whose single pane is managed but lacks the options is the crash window
between `new-session` and `set-option`: the options are set and the stage is
done. Anything else with that name, another pane count or different
options, fails the stage as a name in use rather than being adopted.

A failed stage stops the sequence with `ok: false` and the stage name; the
worktree is left in place for a retry. The command id is client chosen. The
daemon keeps recent ids with their outcome for a few minutes: a repeated id
while the command runs attaches to the running stream, and a repeated id
after it finished replays the result. A dropped bridge during `add` is
therefore retried by sending the same message again.

`rm` checks first and destroys second: `git status --porcelain` in the root,
and a dirty worktree is refused before anything is touched unless `force` is
set. Only then does it kill the managed session and `git worktree remove` the
root. A refused `rm` leaves the agent running. The branch is left alone;
merging, rebasing and deleting branches stay ordinary Git.

The local workspace session is the client's to clean up: after a successful
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

`id` is `<environment_id>/worktree/<repo>/<branch>`, stable for the life of
the worktree and opaque to clients. `session` is the managed session name
when one exists for it, else empty, so `ls` can pair the two records without
matching on cwd. The envelope grows three fields alongside the agent ones:
`worktrees` in a snapshot, `worktree` in an upsert, `worktree_id` in a
remove. A milestone-one client ignores them; a milestone-two client reads
the `worktrees` capability before expecting them.

## Client

```
laatmux add <branch> [--repo r] [--host h] [--agent a] [-- <cmd>...]
laatmux rm  <repo>/<branch> [--host h] [--force]
laatmux path <repo>/<branch> [--host h]
laatmux ls
```

`add` resolves the repository from `--repo`, else from the current
directory being inside a configured path for the local host, else fails.
Host and agent come from the flag, else `last.json`, else the repository's
defaults, else the local host and the first agent. Progress prints one line
per stage. On success the client creates the local workspace session in the
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
