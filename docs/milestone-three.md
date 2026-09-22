# Milestone three: sidebar, dashboard, split, run

Design note for the third milestone in issue #1: the sidebar as a tile
layout with dimmed stale entries, the dashboard popup with jump on enter,
`split` for remote panes, and `run`. It also picks up the interactive
pickers that milestone two left for the dashboard. Milestone one observes
agents, milestone two creates them; this milestone puts them on screen and
lets the user act on them without leaving tmux.

Nothing here changes the model: the local tmux is the only UI, the managed
agent keeps its one session, one window, one pane, and git and the daemons
stay the sources of truth. What changes is where the laptop merges the
hosts' streams, and that change is the first decision below because
everything else sits on it.

## The workflow

1. A sidebar pane on the left of every window in the laptop's tmux shows
   every workspace and agent across the configured hosts, sorted by what
   needs attention. `Enter` or a click on a tile jumps to it.
2. A hotkey opens the same list as a popup over the current pane. `Enter`
   jumps and closes the popup. From the popup, `a` creates a workspace
   through pickers for repository, host and agent, `x` removes one, `s`
   settles or unsettles one.
3. A split hotkey works in every window. In a workspace session it opens a
   pane with a shell at the worktree root on the worktree's host. Anywhere
   else it is the plain split the user has today.
4. `laatmux run` runs a command in a worktree on its host and streams the
   output back, so a test run or a build on the VM is one line from the
   laptop, without a second agent pane.

## One stream for the laptop

Today every client dials every host itself: `ls` opens one connection per
host, `watch` keeps one open per host, and `jump` opens one for a
snapshot. That was fine for one `watch`. A sidebar pane in every window is
a client per window, and with the user's twenty-odd windows and two remote
hosts that is forty ssh channels, each a `laatmux bridge` process on its
host and a subscriber in that host's daemon. sshd's default `MaxSessions`
is 10 channels per connection, so a ControlMaster connection stops
accepting them at the eleventh window and ssh falls back to new TCP
connections and key exchanges. That cost lands on every new window.

The laptop's own daemon becomes the merge point. It already exists on
every machine, is started on demand by the first client, and is the one
process per machine by the startup lock. It gains a `merged` capability:
it dials the hosts in its own config, subscribes to each, and republishes
one stream with every host's records plus a record per host saying whether
that host is reachable. Sidebar panes, the dashboard, `ls`, `watch` and
`jump` read that stream over the local unix socket. Each remote daemon sees
one subscriber per laptop, whatever the laptop shows.

Issue #1 says host connectivity is a client-side axis. It stays a separate
axis, never folded into agent state, but it moves from each client to the
laptop's daemon, which is the client of the remote daemons now. The rule
that nothing crosses the network except state holds: the merged stream is
the same records, forwarded.

What the merging daemon does:

- Reads `hosts` from its config when a merged subscription starts, so a
  host added to the file shows up on the next `sidebar` or `ls` without a
  daemon restart. A host removed from the file gets a `remove` for its
  host record; its agent and worktree records are removed with it.
- Keeps one subscription per configured host, reconnecting with the
  backoff `watch` uses today, cached records visible while a host is down
  with the host record saying so. The local host is itself: it does not
  dial its own socket, it publishes its own records into the merged
  stream directly.
- Holds remote subscriptions only while it has a merged subscriber, and
  drops them 60 seconds after the last one leaves. A laptop with no
  sidebar and no dashboard open holds no ssh channels. The first merged
  subscriber after that pays the reconnect, which is what `ls` pays
  today.
- Dials with the same `ssh -T <alias> laatmux bridge` the client uses,
  from `internal/client`. The daemon is started by a client of the user's
  and inherits its environment, so `SSH_AUTH_SOCK` and the ControlMaster
  socket paths are the ones the user's shell has. A daemon started some
  other way, at login say, may lack the agent; the host record then
  carries ssh's own error, which is what the client prints today.

The merged snapshot and stream:

```
-> {type: subscribe, merged: true}
<- {type: snapshot, seq, hosts: [...], agents: [...], worktrees: [...]}
<- {type: upsert, seq, host_status: {...}}        connectivity changed
<- {type: upsert, seq, agent: {...}}               as today, any host
<- {type: remove, seq, host_name, ...}             host left the config
```

The host record:

```
{name, ssh, environment_id, connected, listed, error, version, capabilities, since}
```

`environment_id` is empty until the host has answered a hello once. Agent
and worktree records already carry their `environment_id`, and the client
maps it to a name through the host records rather than the daemon
rewriting records. Ids are unchanged, so nothing a client stored breaks.
`seq` is the merging daemon's own sequence over the merged stream; the
remote sequences are consumed by it and never forwarded, so a subscriber
that falls behind is disconnected and resnapshots exactly as today.

`connected` and `listed` are the two readiness bits `watch` keeps per host
today. `connected` is a live connection with a completed hello. `listed`
is that the host's records in the merged stream come from a snapshot of
the current connection: it is false from the moment a merged subscription
first brings the host in, or the connection drops, until the next
snapshot from that host has replaced every record of that host's in one
step, as `apply` does today. Cached records from before a drop stay
visible while `listed` is false, so the sidebar shows what was last known
with the host row saying `DOWN`. Absence is authoritative only when both
bits are set: `ls` and the views mark a local session stale only when its
host is connected, listed and has the `worktrees` capability, which is the
rule in `stale` today, and `jump` reports a workspace missing only from a
listed host.

One-shot consumers wait for readiness. The merged snapshot is sent at
once with what the daemon knows, which on a cold daemon is nothing but
the host rows. `ls` then reads upserts until every host is listed or
carries an error, or 20 seconds have passed, and prints, naming the hosts
that are still neither, which is what its per-host snapshot timeout does
today. `jump` and `path` wait the same way for the one host they need.
The views draw whatever has arrived and let the host rows say the rest.

Plain `subscribe` without `merged` keeps meaning this host's own records,
which is what a laptop's daemon serves to a remote laptop, should that
ever happen, and what remote daemons serve to the merging one. A daemon
that lacks `merged` is an older build still running; `sidebar` and
`dashboard` refuse with a message to restart the local daemon, since a
sidebar per window is the case the capability exists for. `ls`, `watch`
and `jump` keep their direct per-host path as the fallback for that
daemon. Commands stay direct: `add`, `rm` and `run` dial the host they act
on, as today, because a command is one connection for its duration and
its retry story is by id on that host.

## Rows

The sidebar, the dashboard and `ls` show the same rows, built the same
way `ls` builds them today: each host's worktrees joined with its agents
by the managed session the worktree record names, then agents with no
worktree, then observed agents on other servers. Two additions:

- Local workspace sessions are joined in, by key, so a row knows its local
  session name, whether it is settled, and whether it is the session the
  viewer is in.
- A row has a dim state, decided from measured axes only:
  the agent's liveness is `gone` or there is no identified agent in the
  session, the worktree has no session, the host is down, the local
  session is stale because its worktree is gone from a reachable host, or
  the workspace is settled. A row with an alive agent on a reachable host
  is never dim, however long it has been idle. Age is shown, not judged:
  an agent that has been idle for an hour is a fact the user reads off the
  tile, and guessing at "stale" from age is how the previous tool's
  sidebar got it wrong often enough to want a filter for it.

Sort order is unchanged: blocked, working, idle, then rows without a live
agent; within a group, most recent activity first, then host, then name.
Settled rows sit in a collapsed group at the bottom, stale rows after
them. The dim state and the groups are the whole of what "dimmed stale
entries" means here.

Row construction moves out of `cmd/laatmux` into a package the three
views share, with a test that feeds it records and local sessions and
checks the rows, since three views now depend on the join being right.

## The view

One list view serves the sidebar pane and the dashboard popup. It is a
tmux pane's worth of terminal: raw mode through the termios calls in
`golang.org/x/sys`, which is already a dependency, ANSI for cursor,
colour and dim, SGR mouse reporting for clicks and wheel. No TUI library:
both views are a list with a selection and a footer, and a framework
would be the largest dependency in the module for a screen that redraws a
few dozen lines. The renderer is a pure function from rows, width, height
and selection to lines, so the layouts are tested against golden strings
without a terminal.

Two layouts, chosen in config and toggled with `v`:

```
tiles (default)                     compact
┌─────────────────────────────┐     ! blocked  claude  laatmux/fix-ls    @vm   2m
│ ! laatmux/fix-ls        @vm │     * working  codex   proj/task         @mac  8s
│   claude  blocked  2m       │     - idle     claude  proj/other        @vm   1h
│   Permission to run pnpm?   │       no agent         proj/spike        @mac
├─────────────────────────────┤
│ * proj/task                 │
│   codex  working  8s        │
│   Editing src/api.ts        │
└─────────────────────────────┘
```

A tile is three lines: the activity mark and `<repo>/<branch>` with the
host tag right-aligned, the agent, its activity and the age of that
activity, then the pane title trimmed to the width. A row without an agent
has a two-line tile whose second line says `no agent` or `no session` as
`ls` does; a stale row says `no worktree`. The host tag is dim for every
host but the local one, so a glance separates the VM's agents from the
laptop's. A dim row is drawn entirely with the dim attribute. The row for
the viewer's own session, the one the sidebar pane sits in or the popup
was opened from, is marked in the gutter. Marks are the ASCII ones `ls`
prints, `!`, `*`, `-`, so the sidebar reads the same in a plain terminal
and over a serial console; icons are a config switch later if wanted, not
a default.

Keys, in both views:

| Key | Does |
|---|---|
| `j` `k` or arrows | move the selection |
| `g` `G` | first, last |
| `Enter` | jump to the selected row |
| `1`..`9` | jump to the nth row of the active group |
| `v` | toggle tiles and compact |
| `/` | filter rows by name; `Esc` clears |
| `f` | show or hide the settled and stale groups |
| `q` | quit; in a sidebar pane this closes that pane |

Mouse: a click on a tile jumps to it, the wheel moves the selection. The
view requests mouse mode so tmux, with the user's `mouse on`, forwards the
click to the pane instead of using it for pane selection. Wheel over the
sidebar therefore does not scroll tmux history, which is the trade the
previous sidebar made too.

Jump from the view is the `jump` command's logic run in-process against
the merged records: a workspace row switches to its local session,
creating it from the record when missing; a `new` session's row does the
same through a plain attachment; an observed agent on the laptop's default
server is a `switch-client`; an observed agent on a remote host's default
server is refused with the message `jump` prints. A worktree row with no
session cannot be jumped to; `Enter` on it shows the `add` line that
would start one, and in the dashboard `a` on it pre-fills the pickers.
The view runs inside the default tmux server in every case, so
`switch-client` is always allowed.

## Sidebar

```
laatmux sidebar [toggle|on|off]        # toggle by default; meant for a key binding
laatmux sidebar pane                   # what runs in a sidebar pane
laatmux sidebar attach <window-id>     # add a pane to one window, called by a hook
laatmux sidebar reap                   # close sidebar panes left alone, called by a hook
```

`on` first installs hooks on the server, then walks every window on the
default server and, where no pane carries `@laatmux_sidebar`, splits one
off the left edge, full height, at the configured width, with focus left
where it was:

```
split-window -d -h -b -f -l <width> -t <window> laatmux sidebar pane \;
set-option -p -t <new pane> @laatmux_sidebar 1
```

The pane id comes from `-P -F '#{pane_id}'` and is tagged in the same
sequence, so a sidebar pane is never observable untagged, as for the
attach pane in milestone two. Hooks go in before the walk so a window
made during the walk is caught by its hook rather than missed by both;
`attach` skipping a window that has a pane makes the overlap harmless.
Every check-and-create, in `on` and in `attach`, runs under an exclusive
`flock` on `$LAATMUX_HOME/sidebar.lock`, since two `attach`es for the same
window, or an `attach` racing `on`, would each see no tagged pane and make
two. `attach` also reads the hooks under that lock and does nothing when
they are gone: an `attach` that was queued behind `off` must not put a
pane back. The hooks, each at an index laatmux owns so `off` removes
exactly what `on` set and the user's own hooks at other indexes stay:

| Hook | Runs |
|---|---|
| `after-new-window[9101]` | `laatmux sidebar attach '#{hook_window}'` |
| `after-new-session[9102]` | `laatmux sidebar attach '#{hook_session}:'`, the session's first window |
| `pane-exited[9103]`, `after-kill-pane[9104]` | `laatmux sidebar reap` |

`attach` is idempotent, so a window that already has a sidebar is left
alone whatever fires. `reap` lists every window and kills a sidebar pane
that is alone in its window, so a window whose real pane exited closes at
once instead of surviving as a sidebar. The user has this exact script
under a `pane-exited` hook today, because the previous sidebar noticed
only on its own poll; `reap` is the same scan, run by tmux, and the
sidebar process does not poll for it. The indexes are a constant and the
hooks are `-g`, on the server, so a new session is covered from its first
window.

`off`, under the same lock, unsets those four hooks and then kills every
pane tagged `@laatmux_sidebar`. `toggle` looks for the hooks: present
means on. A sidebar pane whose
process exits, on `q` or a crash, is gone from that window until a new
window is made or `on` runs again; that is the intended way to dismiss one
window's sidebar.

A sidebar pane is one client of the local daemon's merged stream and
nothing else. It reads its own session name from `TMUX_PANE` to mark the
current workspace, redraws on every change, and every five seconds for
the ages. It does not read local sessions itself.

Settled and stale come from tags on local sessions. Today `watch` lists
the local sessions on every redraw. With a client per window that is a
`tmux list-sessions` per window per change. While it has a merged
subscriber, the merging daemon runs the `list-sessions` that
`workspace.List` runs today against the default server once a second and
publishes local workspace sessions as records in the merged stream:

```
{name, key, host, source, branch, attach, settled}
```

This is its own poll, not a rider on the status poll: `tmux_servers`
defaults to the managed server alone, and a laptop that does not observe
its default server still has its workspace sessions there. Reading
session options is observation like `list-panes` is, and a default server
that is not running is an empty list, not an error. The records are sent
once per change, so settle and unsettle reach every sidebar within a
second, and the view needs no tmux access beyond `switch-client` on jump.

A merged snapshot is never sent with session records from before the
subscriber arrived. The daemon runs `list-sessions` once, synchronously,
before writing each merged snapshot, so the snapshot's `sessions` are as
fresh as the connection, whether the poll had been idle for an hour or
was never started; the call is local and takes milliseconds. A
`list-sessions` that fails for a reason other than no server puts its
message in the snapshot as `sessions_error`, and `ls` prints it where the
settled and stale groups would be, so an incomplete listing says so
rather than looking complete. The readiness wait in `ls` therefore covers
hosts only; the sessions are complete by construction.

Config, in the laptop's `~/.config/laatmux/config.yaml`:

```yaml
sidebar:
  width: 35            # columns; default 35
  layout: tiles        # tiles or compact; default tiles
```

Position is the left edge. A top sidebar with a compact layout is not in
this milestone; the tile layout is why the sidebar exists.

## Dashboard

```
laatmux dashboard
```

The same view, filling whatever it is run in, with actions. Meant for
`display-popup -E`, where the popup closes when the command exits, so a
jump is `switch-client` then exit:

```
bind-key C-s display-popup -E -w 90% -h 80% -T ' laatmux ' 'laatmux dashboard'
```

The dashboard has room for a title line per row in compact layout and
uses it; its default layout is compact because a popup is wide and short.
Actions beyond the shared keys:

| Key | Does |
|---|---|
| `a` | add: pickers for repository, host, agent, then the branch name |
| `x` | rm the selected workspace, after a confirmation line naming it; `X` with force |
| `s` | settle or unsettle the selected workspace |
| `S` | open a shell window in the selected workspace and jump to it |

Pickers are the deferred item from milestone two. Each is a list with the
same keys as the main view and a filter that starts typing at once, over
what the config has: repositories with their labels and sources, hosts
that can `add`, agents. The defaults `laatmux add` would pick are
preselected: the repository of the directory the popup was opened from
when it is one, the host and agent from `last.json` for that repository,
else the config's default order. Fewer than two candidates skips the
picker for that step. The branch name is a text prompt with the same
validation `add` applies. `Esc` in any step returns to the list with
nothing done.

The sequence then runs the `add` command exactly as the CLI does: the
same command id rules, the same reconnect, the progress lines drawn in
the dashboard in place of the list. On success it creates the workspace
session, writes `last.json`, and jumps. On failure the stage and error
stay on screen until a key, then the list returns with the new worktree
row in it, since the daemon publishes the worktree as soon as it is
registered. `x` runs `rm` the same way, and a refusal from git stays on
screen with the hint to use `X`. There is one implementation of each
command, in a package the CLI and the dashboard both call, with the
printing separated from the doing; today they are one function each in
`cmd/laatmux` and this milestone splits them.

The dashboard does not preview the agent's screen. Capture is a non-goal
in issue #1, and the agent is one jump away in a pane that shows the
real thing. Diff stats, PR links and typing into the agent from the list
are likewise not here.

## Split

```
laatmux split [-h|-v] [<pane-id>]
```

Meant to replace the user's split bindings so one key works in every
window:

```
bind | run-shell "laatmux split -h '#{pane_id}'"
bind - run-shell "laatmux split -v '#{pane_id}'"
```

`run-shell` has no `TMUX_PANE`, so the pane id is passed in, format
expanded by tmux; without it, `split` uses `TMUX_PANE`. From the pane it
finds the session and reads the workspace tags:

- Not a workspace session: `split-window -h -t <pane> -c '#{pane_current_path}'`,
  the plain split, so the binding loses nothing anywhere else.
- A workspace session on the local host: `split-window -h -t <pane> -c <root>`.
- A workspace session on a remote host: `split-window -h -t <pane> <ssh shell command>`,
  the command `shell` builds today, `ssh -t` with `cd` and the single-quoted
  root then `exec "$SHELL" -l`.

The new pane lands at the worktree root, not at the current directory of
the pane that was split. For a local pane that is a change from the plain
split, and it is deliberate: the attach pane's `pane_current_path` is
wherever the attach command was started, and a remote pane's is the
laptop directory ssh ran from, so neither means anything, and one rule for
the whole session beats two. A remote shell's live directory is not
knowable from the laptop without the shell reporting it; if a later
version wants splits to follow it, an OSC 7 handler in the shell config
and a pane option are the way, and the daemon can also answer the live
cwd of a process it started. Neither is in this milestone.

Split panes are not tagged. The session carries the tags, and every pane
in it resolves the same way, so a split of a split, or of the `shell`
window, is another shell at the root on the same host. This is where the
first design's per-pane `@laatmux_host` and `@laatmux_cwd` options went:
the workspace session made them redundant.

The managed side is untouched. The split is a local pane in the workspace
window, next to the attach pane; the agent on the host still has its one
pane, which is the invariant jump routing relies on.

## Run

```
laatmux run [<repo>/<branch>] [--host h] -- <cmd>...
```

Runs a command in the worktree root on the worktree's host and streams
its output back, exit code and all. Inside a workspace session the target
defaults to that workspace, like `shell`; elsewhere it must be named.
Resolution is `path`'s: the record by source, else by label.

The command runs as a subprocess of the daemon, not in a tmux pane. The
previous tool runs commands in the worktree's window; laatmux's managed
window is the agent's alone, and putting a build next to it would break
the one-pane invariant for the sake of seeing scrollback that the stream
already delivers. `cmd` is an argv array like `agents.cmd` and `add -- <cmd>`,
run directly with no shell; `-- sh -c '...'` is how to get one. It runs
with the daemon's environment, its stdin at `/dev/null`, no tty, in the
root.

Protocol, capability `run`:

```
-> {type: run, id, repo, branch, root, cmd: [...]}
<- {type: progress, id, n, stage: run, state: start, detail: <root>}
<- {type: progress, id, n, stage: run, state: output, fd: 1|2, detail}   zero or more
<- {type: result, id, ok, error, exit}
-> {type: cancel, id}                                                      from the client, any time
-> {type: follow, id, after}                                               on a redial, in place of run
```

`output` carries one line per message with `fd` saying which stream it
came from, so the client writes stdout lines to its stdout and stderr to
its stderr and `laatmux run proj/x -- go test ./... | tail` behaves. A
partial last line is sent at exit. Output is text: a command that prints
binary gets it mangled by the line split and this is documented rather
than solved. `ok` is true when the process exited at all; `exit` is its
status, and `laatmux run` exits with it. `ok: false` with `error` is for
laatmux's own failures: no such worktree, the command not found, the run
cancelled.

Runs use the daemon's command plumbing from milestone two, with two
changes to it that `add` and `rm` take as well.

Starting and following become different messages, under a new
capability `follow`. Today a repeated id attaches to a running command,
replays a finished one for five minutes, and starts the command when the
id is unknown, which is what makes a redial after a dropped bridge a
plain resend. That is right for `add` and `rm`, whose every step is
skipped by inspection, and wrong for `run`: an unknown id after a daemon
restart or after the five minutes would run `pnpm db:seed` a second time.
So the first send is the command and every redial is `{type: follow, id,
after}`. `follow` on a known id attaches or replays as today. `follow` on
an unknown id returns `{type: result, ok: false, error: unknown command}`;
the `add` and `rm` clients then resend the command as a new execution
under the same id, with their replay cursor reset to zero since the new
execution numbers from 1 again, and the `run` client prints that the
outcome is unknown and exits 255, because the process may be running
still, may have finished, or may never have started, and only the user
can tell which. A daemon that does not advertise `follow` is an older
build: it still takes `add` and `rm`, emits unnumbered progress and would
reject `follow` as an unknown type, so against it the client keeps
today's resend and positional filter, and `run` is refused before it
starts, as any missing capability is. A daemon with `run` always has
`follow`.

Progress messages carry `n`, a per-command sequence from 1, and a client
keeps the highest it has seen. `follow` sends it as `after`, the daemon
replays from `after + 1`, and the client drops anything at or below its
mark, so the replay filter counts nothing and a shorter replay cannot
swallow live output. This replaces the positional `replayFilter`, which
assumes every replay is a prefix of the same stream. Retention is then
free to differ per command. `add` keeps dropping output past 1 MiB for
good, since setup output is a diagnostic. A run's output is the point, so
past the retained megabyte a run keeps streaming live to its current
followers and forgets the oldest lines for replay; a `follow` whose
`after` is below the oldest retained `n` gets one `{state: gap, n, detail:
"<count> lines dropped"}` at the right position before the retained tail.

`cancel` is new and is for `run` alone. Ctrl-C in `laatmux run` sends it
and waits for the result; the daemon sends `SIGTERM` to the process group
and `SIGKILL` five seconds later, and the result says `cancelled`. Without
it there is no way to stop `laatmux run proj/x -- pnpm dev`. A client
that disconnects without cancelling leaves the run going, as an `add`
keeps going, and the same id follows it again. `add` and `rm` do not take
`cancel`: they are short and every step of theirs is idempotent, so
letting them finish is the safe thing. Under a clean daemon shutdown,
`SIGTERM`, runs are cancelled the same way, so a restart for an upgrade
leaves no orphan. A daemon that crashes leaves its runs going, in their
own process groups, unknown to the daemon that replaces it; the note says
so and does not try to adopt them.

Runs take no repository lock; they do not touch the main checkout and a
long one must not block `add`. `rm` gains one step: after git has removed
the worktree and before killing its sessions, the daemon cancels every
run whose root is that root and waits for each to exit, so the `ok` the
client gets means nothing of laatmux's is left in the root. A process in
a deleted directory is laatmux's own and is treated like the session,
and, like the session, it is not touched until git has agreed to the
removal.

The daemon keeps a registry of runs by root under its mutex, with a
removal generation per root, and the two sides interlock on it. A run
reads the root's generation under the mutex before it resolves, resolves
against git outside it, then under the mutex registers itself only if the
generation is unchanged, then starts the process; a run registered before
it has started is still cancellable, and a cancel then means the process
is never started. `rm`, after git has removed the worktree, under the same
mutex bumps the generation and takes the list of the root's runs, then
cancels them outside the mutex and waits. A run that resolved before the
removal and reaches registration after the bump is refused with
`worktree removed; retry`, whether or not an `add` or a hand-made
`git worktree add` has put a worktree back at that root meanwhile: the
request was for the old one. A generation, unlike a flag, needs no one to
clear it, so a worktree recreated by hand takes runs like any other,
which is what git being the source of truth requires. `rm` holds every
repository lock, so no `add` reuses the root before `rm` has returned.

## Client changes, collected

```
laatmux sidebar [toggle|on|off]
laatmux dashboard
laatmux split [-h|-v] [<pane-id>]
laatmux run [<repo>/<branch>] [--host h] -- <cmd>...
laatmux ls / watch                         # read the merged stream when the daemon has it
laatmux jump                               # snapshot from the merged stream when the daemon has it
```

`watch` stays as the plain scrolling list for a terminal that is not a
tmux pane; it is `sidebar pane` without raw mode and keys. `ls` is
unchanged in output.

## Order of work

1. This note.
2. The merged stream in the daemon: host records, local session records,
   reconnect and idle drop; `ls`, `watch` and `jump` on it with the direct
   fallback. Verified against the VM: a host down and back, a host added
   to the config live, the ssh channels held with one sidebar and with
   twenty, and none a minute after the last one closes.
3. Rows and the view: the shared row package with its join test, the
   renderer with golden tests for both layouts, raw mode and keys,
   `dashboard` with jump; then `sidebar` with hooks and `reap`, verified
   under the user's tmux config with its own new-session hook.
4. Pickers and actions in the dashboard, with the command implementations
   split from their CLI printing.
5. `follow` and numbered progress for `add` and `rm`, with the old path
   kept for a daemon without the capability. Then `split`, then `run` with
   `cancel` and the `rm` step, verified on the VM with a run cancelled from
   the laptop, a bridge dropped mid-run and followed again, a daemon
   restarted mid-run, and `rm` during a run.

## Out of scope

Screen preview and typing into an agent from the dashboard; capture is a
non-goal. Diff stats, PR state and any git reading beyond worktrees. A
top-edge sidebar. Per-session sidebar scope. Icons. Splits that follow a
remote shell's current directory. Background or timed `run`. Reading the
previous tool's config, state or hooks; the sidebar and the split
bindings replace theirs.
