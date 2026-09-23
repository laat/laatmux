# Milestone four: the task form and the background add

Design note for issue #22, the first thing daily use asked for after
milestone three: starting a task should be writing what the agent should
do, not naming a branch and then waiting. The form takes the host, the
agent, the repository and a prompt, submits, and closes; the worktree is
made and the agent started with the prompt while the user works on, and
the row shows up in the sidebar as it happens. Milestones one to three
observe, create and act on agents; this milestone changes how a task
begins.

Nothing here changes the model. The local tmux is the only UI, the
managed agent keeps its one session, one window, one pane, git and the
daemons stay the sources of truth, and nothing crosses the network
except state and commands. What is new is that a command can outlive
the client that sent it on the laptop's side too, and that a prompt is
part of starting an agent.

## The workflow

1. A key opens the form: from the dashboard, `a`; from any window, a
   binding to `laatmux compose` in a popup. The host, agent and
   repository are preselected to what `add` would pick, the branch line
   is empty, the cursor is in the prompt.
2. The user writes the prompt, corrects a field if needed, and submits.
   The branch name was derived from the prompt as they typed; they can
   edit it before submitting.
3. The popup closes at once. The laptop's daemon runs the add against
   the host and starts the agent with the prompt. The sidebar shows a
   row for the task from the moment of submit, with the stage the add is
   at, then the worktree row as the host publishes it, then the agent's
   activity as the agent starts working.
4. When the user wants the agent, `Enter` on its row jumps, creating the
   local session then as `jump` does today. A failed add stays as a row
   saying where it failed until dismissed.

`laatmux add <branch> -p <prompt>` is the same from the CLI, in the
foreground as today; `--detach` hands the add to the daemon and returns.

## Where the background add lives

Today `add` runs in the client: it dials the host, streams the stages,
writes `last.json`, makes the local session, jumps. A form that closes
on submit needs a process that stays. The daemon on the host already
runs the add to completion whatever happens to the connection, and
`follow` reattaches by id, so the host side needs nothing. What is
missing is a place on the laptop that keeps the add's progress and
outcome for the views, and it is the laptop's own daemon: the one
process per machine, started on demand, already holding a connection to
every host for the merged stream, and already the thing every sidebar
pane and the dashboard subscribe to.

So the laptop's daemon gains a `relay` capability: a client sends it an
`add` naming the host, the daemon dials that host as the client would
have, sends the add under the client's id, and follows it to the result,
reconnecting and following as the client does today. While it does, it
publishes a pending record in the merged stream, and the views draw it
as a row. The client is answered at once, before the host is dialled,
and may disconnect. The relay is the client's `command.Add` moved into
the daemon, not a second implementation: the same `stream` with its
follow and its resend rules, the same result handling. What the client
did after the result, `last.json` and the local session, splits: the
client writes `last.json` at submit, since it is the client's state and
the submit is the choice; the local session is not made at all, `jump`
makes it from the record when the user goes there, which is what `jump`
already does for a worktree row with a session.

Pending records are the daemon's state, not a file the views read. They
are also persisted, one JSON file per id under `$LAATMUX_HOME/pending/`,
with the host, repository, branch, agent and the time of submit, and
never the prompt, so a daemon restarted for an upgrade picks them up
again and follows their ids; the host keeps a finished command for five
minutes, and a `follow` the host no longer knows marks the record failed
with `outcome unknown`, which the worktree row, present or absent, then
settles. A record whose add succeeded is removed once the host has
published the worktree with its session, since the row it stood for is
there now; a failed one stays until dismissed. The relay takes no
lock and has no limit: two submits for two repositories run in parallel
on the host as two adds do today, and two for the same repository
serialize there on the repository lock.

An older laptop daemon without `relay` is what the views refuse for the
sidebar already; for the form the dashboard falls back to the
foreground add with its log overlay, as `a` does today, so the form
works either way and the background is what the capability adds.

## The prompt reaches the agent

An agent is started by its configured argv in the managed session. Two
ways for the prompt to get there, the first preferred:

- **On the command line.** `{prompt}` in an agent's `cmd` is replaced by
  the prompt when the session is made, as one argument however many
  lines it has; `claude` and `codex` both take a positional prompt. An
  agent whose `cmd` has the placeholder and is started without a prompt
  gets the argument removed, not an empty one.

  ```yaml
  agents:
    claude:
      cmd: [claude, "{prompt}"]
  ```

- **Typed in.** An agent without the placeholder gets the prompt typed
  into its pane once it is ready: the daemon's detector already says
  when the pane shows the prompt box, `idle` after the startup grace,
  and that is the one place typing into an agent makes sense. The
  daemon loads the prompt into a tmux buffer and pastes it with
  bracketed paste, then sends Enter, once, and only while the session's
  pane is the one it made, so an agent that died and was replaced by a
  shell is not typed into. A pane that never reports idle within a
  minute gets nothing, and the add's result says so in a final progress
  line, not as a failure: the agent is up and the prompt is in the
  user's hands.

The prompt travels in the `add` message as a new field, on the relay
and on the host, and is dropped from the daemon's memory once the agent
stage is done. It is never logged, never written to a pending file, and
never in a progress line: the `agent` stage's `start` line names the
session and the command with the placeholder still in it. A prompt is
plain text; images and attachments are out of scope.

## The branch name

The branch is derived from the prompt as it is typed, and shown in the
branch line where it can be edited: the first words of the prompt,
lowercased, runs of anything but letters and digits turned into one
`-`, trimmed, cut at forty characters on a word boundary, and made
unique against the branches and worktrees the host has for the
repository by a `-2`, `-3` suffix. An edited branch line stops
following the prompt. `add`'s validation applies at submit, so a name
git would refuse is refused in the form with the same message.

## The form

One overlay in the view package, next to the pickers, the prompt and
the log, filling the popup:

```
 add a task
 ┌ repository ─────────┐ ┌ host ────┐ ┌ agent ───────┐
 │ laatmux             │ │ vm       │ │ claude       │
 └─────────────────────┘ └──────────┘ └──────────────┘
 branch  make-the-sidebar-follow-the-current-row

 ┌ prompt ─────────────────────────────────────────────┐
 │ Make the sidebar follow the current row when the    │
 │ sort moves it, and select nothing when no row is    │
 │ this session.█                                      │
 └─────────────────────────────────────────────────────┘
 tab next field  enter submit  ctrl-j newline  esc cancel
```

Keys: `Tab` and `Shift-Tab` move between the fields; on a chip, `Left`
and `Right` cycle its candidates and `Enter` opens the existing picker
with its filter; on the branch line, typing edits it; in the prompt,
typing edits, `Ctrl-J` inserts a newline, since `Enter` is `\r` and a
newline is `\n` in raw mode and the two are told apart without any
terminal extension, and `Enter` submits when the prompt is not empty.
`Esc` cancels the whole form. A field with one candidate is shown, not
skipped, so the form reads the same every time. The prompt box wraps
and scrolls; the renderer stays a pure function of the fields, the
cursor and the size, so the layouts are tested against golden strings.

Submitting sends the add to the local daemon with `relay` set to the
host, writes `last.json` for the repository, and ends the view. From
the dashboard, `a` opens the form in place of today's picker sequence,
and `a` on a worktree row without a session pre-fills it as today. The
popup binding is `laatmux compose`, the form alone: the dashboard's
model without the list, exiting on submit or cancel, so a binding like
`display-popup -E -d '#{pane_current_path}' 'laatmux compose'` is the
whole of it and the repository defaults to the directory the popup was
opened from, as `a` does.

## Pending rows

A pending record is a row in the main group, sorted first, since it is
what the user just asked for: the mark is the spinner while the add
runs, `!` when it failed, and the name is `<repo>/<branch>` with the
host tag; the second line in tiles, or the activity column in compact,
is `adding: <stage>` from the progress, `failed at <stage>: <error>`
after a failure, the title line the error's detail. The row is not dim
while running and dim once failed, as a stale row is. `Enter` on a
running one does nothing; on a failed one it shows the error in the
footer; `x` on a failed one dismisses it, with a confirm line, which is
`{type: dismiss, id}` to the local daemon. A succeeded record's row is
replaced by the worktree row it became, so a jump lands on the agent
without a gap.

Rows are built as today: the rows package takes the pending records in
its input next to the worktrees and joins one with the worktree row
whose host, repository and branch it names, so the pending row stops
the moment the worktree row can stand in for it, whichever message
arrives first.

## Protocol

New capability on the laptop's daemon, `relay`, next to `merged`:

```
-> {type: add, id, relay: <host>, repo, branch, agent_name, cmd, prompt}
<- {type: result, id, ok}                                accepted; the host is dialled after
-> {type: dismiss, id}                                   drop a failed pending record
<- {type: result, id, ok}
```

The merged stream carries the pending records with the host, agent and
session records:

```
<- {type: snapshot, seq, hosts, agents, worktrees, sessions, pendings: [...]}
<- {type: upsert, seq, pending: {...}}
<- {type: remove, seq, pending_id}
```

The pending record:

```
{id, host, repo, source, branch, agent, submitted_at, stage, state, detail,
 done, error, root, session}
```

`stage`, `state` and `detail` are the last progress message's; `done`
with `error` empty and `root` and `session` set is the success that the
worktree row takes over from; `done` with `error` is the failure that
stays. Ids are the client's, `add-<pid>-<nanos>` as today, so a relay
resent after a lost laptop daemon is the same id on the host and
attaches rather than starts again.

On the host, `add` gains `prompt`, under the existing `add` capability:
a daemon that does not know the field ignores it, and the client then
knows the agent got no prompt only from the version; the `hosts`
listing marks such a daemon as it marks differing builds, and the form
says `prompt not supported by <host>'s daemon` in the footer before
submit rather than after.

## Order of work

1. This note.
2. The prompt on the host: `prompt` in the add message, `{prompt}` in
   an agent's `cmd`, the typed-in fallback through the detector with a
   `SendKeys` on the managed server, `add -p` in the CLI. Verified on
   the VM with `claude` taking the prompt positionally and with a
   `cmd` without the placeholder.
3. The relay in the laptop's daemon: `add` with `relay`, the pending
   records in the merged stream, their files under the state directory,
   re-follow on restart, `dismiss`, `add --detach`. Verified with the
   laptop daemon restarted mid-add and with the host's daemon restarted
   mid-add.
4. The branch name from the prompt and the form overlay with its golden
   tests, `compose`, the dashboard's `a` on it with the foreground
   fallback for an older daemon.
5. The pending rows in the rows package and both views, the join with
   the worktree row, `x` to dismiss; verified under the user's tmux
   config with a submit from the popup and the row followed through to
   the agent working.

## Out of scope

Attachments and images in the prompt. Choosing a model or a permission
mode in the form; those are the agent's command line in `agents`. A
second prompt to a running agent, or typing into an agent from the
views at all beyond the one prompt at start. Background `run`. A queue
of tasks per repository. Templates or history for prompts.
