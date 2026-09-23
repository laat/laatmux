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
part of starting an agent, with a delivery of its own that is never
inferred from anything else.

## The workflow

1. A key opens the form: from the dashboard, `a`; from any window, a
   binding to `laatmux compose` in a popup. The host, agent and
   repository are preselected to what `add` would pick, the cursor is
   in the prompt.
2. The user writes the prompt. A branch name is derived from it as they
   type, into the branch line after the prompt; `Tab` moves there to
   change it. Submit is `Enter` in either.
3. The popup closes once the laptop's daemon has taken the task. The
   daemon runs the add against the host and starts the agent with the
   prompt. The sidebar shows a row for the task from the moment of
   submit, with the stage the add is at, then the agent's activity once
   the agent is working with the prompt.
4. When the user wants the agent, `Enter` on its row jumps, creating the
   local session then as `jump` does today. An add that failed, or whose
   prompt did not reach the agent, stays as a row saying so until the
   user acts on it: `p` delivers the prompt to the agent now, `x`
   dismisses the row.

`laatmux add <branch> -p <prompt>` is the same from the CLI, in the
foreground as today; `--detach` hands the add to the daemon and returns.

## Where the background add lives

Today `add` runs in the client: it dials the host, streams the stages,
writes `last.json`, makes the local session, jumps. A form that closes
on submit needs a process that stays. The daemon on the host already
runs the add to completion whatever happens to the connection, and
`follow` reattaches by id, so the host side needs nothing for that. What
is missing is a place on the laptop that keeps the add's progress and
outcome for the views, and it is the laptop's own daemon: the one
process per machine, started on demand, and the thing every sidebar
pane and the dashboard subscribe to.

So the laptop's daemon gains a `relay` capability: a client sends it an
`add` naming the host, the daemon dials that host as the client would
have, sends the add under the client's id, and follows it to the result.
While it does, it publishes a pending record in the merged stream, and
the views draw it as a row. The relay is the client's `command.Add`
moved into the daemon, not a second implementation: the same `stream`
with its follow, the same result handling, with two differences the
prompt forces, below. What the client did after the result splits: the
client writes `last.json` at submit, since it is the client's state and
the submit is the choice; the local session is not made at all, `jump`
makes it from the record when the user goes there, which is what `jump`
already does for a worktree row with a session.

A relay is the daemon's own goroutine with its own connection to the
host, one ssh channel per outstanding add, ended by the result; it does
not ride on the merged subscription, which comes and goes with the
viewers, and it runs whether or not a viewer is open. Two submits for
two repositories run in parallel on the host as two adds do today; two
for the same repository serialize there on the repository lock, each
holding its channel meanwhile. There is no limit; the user submits a
handful at a time, not hundreds.

Accepting a task means it cannot be lost. The daemon writes the pending
file first, atomically, and answers the client only then; the form
closes on that answer. A daemon that crashes between the answer and the
host answering still has the task on disk, prompt included, and its
successor picks it up. The client's answer says `accepted`, not started;
the pending record says when the host has taken it.

Connectivity is not outcome. The client's `stream` gives up after three
lost connections, which is right for a user watching; the relay follows
with the reconnect backoff the merged stream uses, for as long as the
task is outstanding, and the pending record says `host unreachable,
retrying` meanwhile, never failed. A `follow` the host no longer knows,
after its five-minute retention or a restart, is the one case the relay
treats differently from the client: the client resends, since every
step of `add` is skipped by inspection; the relay resends too, but the
agent stage's prompt is not inspectable, and the rules below say what a
resend does with it.

An older laptop daemon without `relay` gets the foreground add with its
log overlay, from the dashboard's `a` and from `compose` alike, so the
form works either way and the background is what the capability adds.
The views' own requirement is unchanged: they refuse a daemon without
`merged`, as today. `--detach` against such a daemon is an error saying
so, not a foreground add in disguise.

## The prompt reaches the agent

An agent is started by its configured argv in the managed session. Two
ways for the prompt to get there, the first preferred:

- **On the command line.** `{prompt}` in an agent's `cmd` is replaced by
  the prompt when the session is made, as one argument however many
  lines it has; `claude` and `codex` both take a positional prompt. An
  agent whose `cmd` has the placeholder and is started without a prompt
  gets the argument removed, not an empty one. The argument is on the
  host's process list for as long as the agent runs, visible to another
  user of that host with `ps`; choosing the placeholder is choosing
  that, and the note says so where the config is documented.

  ```yaml
  agents:
    claude:
      cmd: [claude, "{prompt}"]
  ```

- **Typed in.** An agent without the placeholder gets the prompt typed
  into its pane once it is ready. Ready is a fresh observation by the
  daemon's detector, after the startup grace, that says all of: the
  prompt box is on screen, `VisibleIdle`, not the idle the detector
  falls back to when nothing matches; the pane's identified agent is
  alive, so a wrapper that became a shell after the agent died is not
  typed into; and the pane is the one the add made, on the same server
  instance. The daemon loads the prompt into a tmux buffer named for
  the command id, pastes it with bracketed paste, sends Enter, deletes
  the buffer, and does this once. A pane that is not ready within a
  minute gets nothing.

Delivery is a state of its own, `prompt` in the result and in the
pending record: `delivered`, `not delivered` with a reason, `none` when
no prompt was given, and `unknown` when the daemon cannot say. It is
never inferred: a worktree exists from the worktree stage on, a session
from the agent stage on, and neither says whether the prompt arrived.
The rules:

- A session the agent stage creates gets the prompt, by argument or by
  typing, and the result says `delivered`, or `not delivered: <reason>`
  when the pane was not ready in time or the paste failed.
- A session the agent stage finds already running in the root, which is
  the skip path a retry takes, gets nothing: the prompt may be there
  already, or the session may be the user's own. The result says `not
  delivered: session existed`. This is at most once, chosen over the
  chance of the same prompt twice.
- A daemon restarted between creating the session and delivering has no
  memory of either; the resend lands on the skip path and reports as
  above.

A result that is not `delivered` keeps the pending row, with the reason,
and the prompt stays in the pending file until the user acts: `p` on
the row sends `{type: prompt, session, prompt}` to the host, which types
it in under the same readiness rules and answers with the delivery
state; `x` dismisses the row and deletes the file. `jump` works on the
row meanwhile, since the agent is up. `laatmux add -p` in the
foreground prints the delivery state as its last line and exits 0 when
the add succeeded whatever the delivery, since the worktree and the
agent are there; the state is what the user reads.

The prompt is sensitive. It travels in the `add` message and the
`prompt` message, over the same ssh as everything. On the laptop it is
in the pending file, mode 0600 under the state directory, from accept
until delivered or dismissed, and in the daemon's memory meanwhile. On
the host it is in the command's memory until the agent stage is done,
never in the replay cache, never in a progress line: the `agent` stage's
`start` line names the session and the command with the placeholder in
it, and a tmux invocation that fails with the prompt in its arguments
has the prompt replaced by the placeholder before the error reaches the
result, the log or anyone. The paste buffer is deleted whether the paste
worked or not. Nothing logs it.

The host advertises the field as a capability, `prompt`, and every
connection that carries a prompt checks it, in the foreground and the
relay, on the first connection and on each reconnect, as `follow` and
`run` are checked today; a host without it refuses the submit with a
message, it does not take the add and drop the prompt. The form reads
the cached capability from the host row and says `prompt not supported
by <host>'s daemon` in its footer before submit.

## The branch name

The branch line is filled from the prompt as it is typed: the first
words, lowercased, runs of anything but letters and digits turned into
one `-`, trimmed, cut at forty characters on a word boundary. It is a
proposal. A generated name is submitted as such, and the host makes it
unique under the repository lock, where the branches and worktrees are
known and two forms cannot race: the first free of `<name>`, `<name>-2`,
`<name>-3` against the local and remote branches and the registered
worktrees, and the add's first progress line and the result carry the
branch used. A name the user edited is explicit and behaves as `add
<branch>` does today, reusing a branch that exists, which is what `a` on
a worktree row without a session wants. `add`'s validation applies at
submit, so a name git would refuse is refused in the form with the same
message.

## The form

One overlay in the view package, next to the pickers, the prompt and
the log, filling the popup:

```
 add a task
 ┌ repository ─────────┐ ┌ host ────┐ ┌ agent ───────┐
 │ laatmux             │ │ vm       │ │ claude       │
 └─────────────────────┘ └──────────┘ └──────────────┘
 ┌ prompt ─────────────────────────────────────────────┐
 │ Make the sidebar follow the current row when the    │
 │ sort moves it, and select nothing when no row is    │
 │ this session.█                                      │
 └─────────────────────────────────────────────────────┘
 branch  make-the-sidebar-follow-the-current-row
 tab next field  enter submit  ctrl-j newline  esc cancel
```

The fields in tab order: repository, host, agent, prompt, branch. `Tab`
and `Shift-Tab` move between them; on a chip, `Left` and `Right` cycle
its candidates and `Enter` opens the existing picker with its filter;
in the prompt, typing edits, with `Left`, `Right`, `Home`, `End`,
`Backspace` and `Delete` within the line, `Ctrl-J` inserts a newline,
since `Enter` is `\r` and a newline is `\n` in raw mode and the two are
told apart without any terminal extension, and `Enter` submits when the
prompt is not empty; on the branch line, typing edits it, and an edited
line stops following the prompt, `Enter` submits. `Esc` cancels the
whole form. A field with one candidate is shown, not skipped, so the
form reads the same every time.

Pasting is part of the contract: the view turns bracketed paste on with
raw mode and off with it, the decoder recognises the start and end
sequences, and everything between them is text inserted where the
cursor is, `\r` and `\n` included as newlines, never a submit, never a
tab. A tmux popup passes the sequences through once the application
has asked for them. The prompt box wraps and scrolls; the renderer
stays a pure function of the fields, the cursor and the size, so the
layouts are tested against golden strings, and the decoder is tested
with pasted and split sequences.

Submitting sends the add to the local daemon with `relay` set to the
host and waits for `accepted`, writes `last.json` for the repository,
and ends the view. From the dashboard, `a` opens the form in place of
today's picker sequence, and `a` on a worktree row without a session
pre-fills it as today, with the branch explicit. The popup binding is
`laatmux compose`, the form alone: the dashboard's model without the
list, exiting on submit or cancel, so a binding like
`display-popup -E -d '#{pane_current_path}' 'laatmux compose'` is the
whole of it and the repository defaults to the directory the popup was
opened from, as `a` does.

## Pending rows

A pending record is a row in the main group, sorted first, since it is
what the user just asked for. The mark is the spinner while the add
runs, `!` when it needs the user. The name is `<repo>/<branch>` with the
host tag, the branch the host allocated once known. The second line in
tiles, or the activity column in compact, is `adding: <stage>` from the
progress, `host unreachable, retrying` while the relay cannot reach the
host, `failed at <stage>` after a failure, `prompt not delivered` after
a success without delivery, `outcome unknown` when the host no longer
knows the add; the title line is the detail or the reason. The row is
not dim while running and dim once it needs the user, as a stale row is.
`Enter` on a running one does nothing; on one with a session it jumps;
`p` on a `not delivered` one delivers the prompt; `x` on one that needs
the user dismisses it, with a confirm line.

A pending record and the worktree row it will become are joined by
identity, not by name: the record carries the host's environment id
from the hello of the connection that carried the add, the repository
source, and the root from the moment the host reports it, which the
`resolve` stage's progress does today in its detail and will do in a
field of its own. While a pending record exists and is not done and
delivered, the worktree row for the same environment and root is not
drawn: the pending row stands for it, with more to say. Once the record
is done and delivered, and the worktree row is there with its session,
the record is removed, its file with it, and the worktree row is drawn.
The view carries the selection across: a rows build reports the ids it
replaced, the pending record's id by the worktree row's, and the model
moves its anchor before it looks for it, so `Enter` after the change
lands on the agent, whichever message arrived first and however the
sort moved the row.

A pending record that needs the user stays until dismissed, through
laptop daemon restarts, with its outcome and reason in the file. One
whose host is gone from the config stays too, with `host removed`, so
nothing the user asked for disappears without them.

## Protocol

New capability on the laptop's daemon, `relay`, next to `merged`:

```
-> {type: add, id, relay: <host>, repo, branch, generated, agent_name, cmd, prompt}
<- {type: result, id, ok}                          accepted: on disk, host dialled after
-> {type: dismiss, id}                             drop a pending record that needs the user
<- {type: result, id, ok}
-> {type: prompt, id}                              deliver a pending record's prompt now
<- {type: result, id, ok, prompt: delivered | not delivered, error}
```

The merged stream carries the pending records with the host, agent and
session records:

```
<- {type: snapshot, seq, hosts, agents, worktrees, sessions, pendings: [...]}
<- {type: upsert, seq, pending: {...}}
<- {type: remove, seq, pending_id}
```

The pending record, without the prompt:

```
{id, host, environment_id, source, repo, branch, generated, agent,
 submitted_at, taken, reachable, stage, state, detail, root, session,
 done, error, prompt}
```

`taken` is that the host has the add; `reachable` is the relay's
connection, the connectivity axis kept apart from the outcome as issue
#1 wants; `stage`, `state` and `detail` are the last progress message's;
`done` with `error` empty and `prompt: delivered` is the success the
worktree row takes over from; `done` with `error`, or without
`delivered`, is what needs the user. Ids are the client's,
`add-<pid>-<nanos>` as today, so a relay resent after a lost laptop
daemon is the same id on the host and attaches rather than starts
again.

On the host, under a new capability `prompt`:

```
-> {type: add, ..., prompt, generated}
<- {type: progress, id, n, stage: resolve, state: done, detail, branch, root}
<- {type: result, id, ok, root, session, pane_id, branch, prompt: delivered | not delivered | none, error}
-> {type: prompt, session, prompt}
<- {type: result, ok, prompt: delivered | not delivered, error}
```

`generated` asks the host to make the branch unique; `branch` on the
resolve progress and on the result is the branch used. `prompt` on a
result is the delivery state. A host without the capability refuses an
add that carries a prompt, on the client's side, before it is sent.

## Order of work

1. This note.
2. The prompt on the host: capability `prompt`, the field in the add
   message, `{prompt}` in an agent's `cmd` with the argument redacted in
   any tmux error, the typed-in delivery through the detector's
   readiness with a `Paste` on the managed server, the delivery state
   in the result, the `prompt` message, `generated` branches, `add -p`
   in the CLI printing the state. Verified on the VM with `claude`
   taking the prompt positionally, with a `cmd` without the placeholder,
   with a retry after the daemon was restarted between the session and
   the delivery, and with two generated names for one prompt.
3. The relay in the laptop's daemon: `add` with `relay`, the pending
   file before the answer, the pending records in the merged stream,
   the follow with backoff, re-follow on restart, `dismiss`, the
   `prompt` message forwarded, `add --detach`. Verified with the laptop
   daemon restarted mid-add, the host's daemon restarted mid-add, and
   the host unreachable for longer than the client's three attempts.
4. The form overlay with bracketed paste in the decoder and golden
   tests, the branch proposal, `compose`, the dashboard's `a` on it with
   the foreground fallback for an older daemon.
5. The pending rows in the rows package and both views, the join by
   environment and root, the anchor carried across, `p` and `x`;
   verified under the user's tmux config with a submit from the popup
   and the row followed through to the agent working on the prompt.

## Out of scope

Attachments and images in the prompt. Choosing a model or a permission
mode in the form; those are the agent's command line in `agents`. A
second prompt to a running agent beyond delivering the one it was
started for. Background `run`. A queue of tasks per repository.
Templates or history for prompts. Encrypting the pending file; it is
the state directory's own protection, as `last.json` has.
