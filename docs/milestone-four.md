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
the client that sent it on the laptop's side too, that a prompt is
part of starting an agent, with a delivery of its own that is never
inferred from anything else, and that the host writes down what it
did for a command before it does it, since a prompt delivered and a
branch allocated are not things a retry can see by looking.

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
   prompt did not reach the agent, or whose outcome the daemons can no
   longer say, stays as a row saying so until the user acts on it: `p`
   delivers the prompt to the agent now, `x` dismisses the row.

`laatmux add <branch> -p <prompt>` is the same from the CLI, in the
foreground as today; `--detach` hands the add to the daemon and returns.

## The host remembers what it did

Every step of `add` today is skipped by inspection on a retry: a
checkout, a branch, a worktree, a copied file, a setup marker, a
session in the root are all there to be looked at, so a resend after a
lost connection or a daemon restart finishes what was started and
never does a step twice. Two things this milestone adds cannot be
inspected. A branch allocated for a generated name looks like any
other branch; a resend that allocates again makes `task-2` beside
`task` and a second worktree for one task. A prompt delivered into a
pane leaves nothing a daemon can see; a resend that finds the session
cannot tell whether the prompt is in it, and delivering again is the
same task twice, not delivering is a silent loss.

So the host's daemon keeps a journal: one file per command id under
`$LAATMUX_HOME/commands/`, written atomically before each side effect
that inspection cannot see and rewritten after it, and read before
anything is done for an id it has seen. It holds the command's
identity, `source`, `agent`, whether the branch was generated, the
allocated branch once it is, the root, and the session, the pane and
the server instance the agent stage made, the agent's identity once
observed, and the delivery state with its attempts. The in-memory
command with its replay stays as it is for streaming; the journal is
what a resend consults. Entries are removed when the worktree is
removed by `rm`, and swept when their root is gone from git and their
age is past a day, so the directory holds what is live and recent.

With the journal, a resend under a known id reuses the allocated branch
and takes the recorded delivery state as the truth; a resend under an
id the journal has never seen is a new add, as today. A daemon that
dies between writing `attempting` and writing `delivered` leaves the
state `unknown`, which is reported as such and never resolved by
guessing.

## Where the background add lives

Today `add` runs in the client: it dials the host, streams the stages,
writes `last.json`, makes the local session, jumps. A form that closes
on submit needs a process that stays. The daemon on the host runs the
add to completion whatever happens to the connection, and `follow`
reattaches by id, so the host side needs nothing for that. What is
missing is a place on the laptop that keeps the add's progress and
outcome for the views, and it is the laptop's own daemon: the one
process per machine, started on demand, and the thing every sidebar
pane and the dashboard subscribe to.

So the laptop's daemon gains a `relay` capability: a client sends it an
`add` naming the host, the daemon dials that host as the client would
have, sends the add under the client's id, and follows it to the result.
While it does, it publishes a pending record in the merged stream, and
the views draw it as a row. The relay is the client's `command.Add`
moved into the daemon, not a second implementation: the same `stream`
with its follow and its environment pin, the same result handling, with
the differences below. What the client did after the result splits: the
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
after its five-minute retention or a restart, is resent as the client
resends, and the host's journal makes the resend safe: the branch is
the one allocated, the delivery state is the one recorded. Every
connection of a relay, the first and each reconnect, is pinned to the
environment id the pending record carries, with the check `stream`
already has, so a host alias moved to another machine gets nothing.

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
  that, and the README says so where the config is documented.

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
  typed into; and the pane is the one the journal names, on the server
  instance it names, with the agent identity it recorded at the first
  observation. The daemon loads the prompt into a tmux buffer named for
  the attempt, pastes it with bracketed paste, sends Enter, deletes the
  buffer, and does this once. A pane that is not ready within a minute
  gets nothing.

Delivery is a state of its own, `prompt` in the result and in the
pending record, and its attempts are journaled on the host:

- `none`: the add carried no prompt. Complete.
- `delivered`: the journal says an attempt reached Enter. Complete.
- `not delivered: <reason>`: the pane was not ready in time, the paste
  failed before anything reached the pane, or the session existed
  already and no attempt was ever made. Needs the user.
- `unknown`: an attempt was written as `attempting` and no outcome
  followed, the daemon died between the paste and the record, or the
  journal for a session the resend found is missing. Needs the user,
  and no daemon delivers again on its own.

The rules a resend follows are the journal's: a session the agent
stage creates gets the prompt and the attempt is journaled around the
paste, `attempting` before, `delivered` or `not delivered` after; a
session found already in the root, the skip path, takes the journal's
delivery state as it stands, `none` when the journal has no entry for
the id, since then the session is the user's own or an older add's,
and `unknown` when the entry says `attempting`. Nothing is inferred
from the session's existence, and a session that has exited since is
not recreated by a resend for delivery's sake: the agent stage skips by
root only when a managed session is there, as today, and when none is
there it makes one and delivers, which is the same task's first
delivery unless the journal says otherwise.

A result whose delivery needs the user keeps the pending row, with the
reason, and the prompt stays in the pending file until the user acts.
`p` on the row starts a delivery attempt: `{type: prompt, id, attempt}`
to the host, where `id` is the add's and `attempt` a number the relay
increments, so the host journals it, serializes attempts per session,
answers a repeat of the same attempt with its recorded outcome rather
than pasting again, and lets the relay `follow` an attempt whose reply
was lost. The host checks the target against the journal: the root
must still be the worktree, the session's single pane the one recorded,
the server instance the same, the agent identity the recorded one or
none yet; a replacement session or agent is refused with `not
delivered: session replaced`, and the user decides. `x` dismisses the
row and deletes the file. `jump` works on the row meanwhile, since the
agent is up. `laatmux add -p` in the foreground prints the delivery
state as its last line and exits 0 when the add succeeded whatever the
delivery, since the worktree and the agent are there; the state is what
the user reads.

The prompt is sensitive. It travels in the `add` message and the
`prompt` message, over the same ssh as everything. On the laptop it is
in the pending file, mode 0600 under the state directory, from accept
until the delivery state is `delivered` or `none`, when the file is
rewritten without it at once, or until dismissed; and in the daemon's
memory meanwhile. On the host it is in the command's memory until the
agent stage is done and in a paste buffer for the length of a paste,
never in the journal, never in the replay cache, never in a progress
line: the `agent` stage's `start` line names the session and the
command with the placeholder in it, and a tmux invocation that fails
with the prompt in its arguments has the prompt replaced by the
placeholder before the error reaches the result, the log or anyone.
The paste buffer is deleted whether the paste worked or not. Nothing
logs it.

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
unique in an `allocate` step of its own after `fetch`, when the local
and remote branches are current and the repository lock is held so two
forms cannot race: the first free of `<name>`, `<name>-2`, `<name>-3`
against the local branches, the remote branches and the registered
worktrees, written to the journal before the branch is made, so a
resend takes the allocated name and never allocates again. The
`allocate` progress line and the result carry the branch used, and the
pending record shows it from then on. A name the user edited is
explicit and behaves as `add <branch>` does today, reusing a branch
that exists, which is what `a` on a worktree row without a session
wants. `add`'s validation applies at submit, so a name git would refuse
is refused in the form with the same message.

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
host, `failed at <stage>` after a failure, `prompt not delivered` or
`prompt delivery unknown` after a success without a complete delivery,
`outcome unknown` when neither daemon can say what became of the add,
`done, worktree gone` when the add completed and the worktree has since
been removed; the title line is the detail or the reason. The row is
not dim while running and dim once it needs the user, as a stale row is.
`Enter` on a running one does nothing; on one with a session it jumps;
`p` on one whose prompt is retained delivers it; `x` on one that needs
the user dismisses it, with a confirm line.

Complete is one predicate, used by the daemon to retire a record, by
the rows to hide the worktree row behind the pending one, and by the
views to offer `p` and `x`: the add succeeded and the delivery state is
`delivered` or `none`. A record that is complete is retired, its file
removed, once the host is listed in the merged stream and the worktree
record for its environment and root is there, which hands the row over
to the worktree row, with or without a session; or once the host is
listed and the root is not among its worktrees, which is `done,
worktree gone`, a row that needs the user only to be dismissed. Until
then the pending row stands and the worktree row for the same
environment and root is not drawn.

A pending record and the worktree row it will become are joined by
identity, not by name: the record carries the host's environment id
from the hello of the connection that carried the add, the repository
source, and the root from the moment the host reports it, in a field
of the `allocate` progress line. From that moment the pending row's id
is the worktree row's, `<environment>/worktree/<root>`, so the view's
selection anchor, which is the row id, needs no transfer: whichever
message arrives first, and across a resnapshot, the same id names the
task, and `Enter` after the change lands on the agent. Before the root
is known the id is the command's.

A pending record that needs the user stays until dismissed, through
laptop daemon restarts, with its outcome and reason in the file; a
daemon that starts and finds a record whose result is recorded does
not resubmit it. One whose host is gone from the config stays too, with
`host removed`, so nothing the user asked for disappears without them.

## Protocol

New capability on the laptop's daemon, `relay`, next to `merged`:

```
-> {type: add, id, relay: <host>, repo, branch, generated, agent_name, cmd, prompt}
<- {type: result, id, ok}                          accepted: on disk, host dialled after
-> {type: dismiss, id}                             drop a pending record that needs the user
<- {type: result, id, ok}
-> {type: prompt, id}                              deliver a pending record's prompt now
<- {type: result, id, ok, prompt: <state>, error}
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
 done, error, prompt, attempt}
```

`taken` is that the host has the add; `reachable` is the relay's
connection, the connectivity axis kept apart from the outcome as issue
#1 wants; `stage`, `state` and `detail` are the last progress message's;
`prompt` is the delivery state and `attempt` the number of the last
attempt. Ids are the client's, `add-<pid>-<nanos>` as today, so a relay
resent after a lost laptop daemon is the same id on the host and
attaches rather than starts again.

On the host, under a new capability `prompt`:

```
-> {type: add, ..., prompt, generated}
<- {type: progress, id, n, stage: allocate, state: done, detail, branch, root}
<- {type: result, id, ok, root, session, pane_id, branch, prompt: <state>, error}
-> {type: prompt, id, attempt, prompt}
<- {type: result, id, attempt, ok, prompt: <state>, error}
-> {type: follow, id, attempt, after}              an attempt whose reply was lost
```

The delivery state is `none`, `delivered`, `not delivered` or
`unknown`, with the reason in `error` where there is one. `generated`
asks the host to allocate the branch; `branch` and `root` on the
`allocate` progress line and on the result are the ones used. A `prompt`
message is a command like `add`, with the journal behind it: a repeated
attempt is answered from the record, and `follow` with an attempt
reattaches to one in flight. A host without the capability refuses an
add that carries a prompt, on the client's side, before it is sent.

## Order of work

1. This note.
2. The journal and the prompt on the host: capability `prompt`, the
   journal under the state directory with its sweep, `generated`
   branches in an `allocate` step after `fetch`, the field in the add
   message, `{prompt}` in an agent's `cmd` with the argument redacted in
   any tmux error, the typed-in delivery through the detector's
   readiness with a `Paste` on the managed server, the attempts and
   their states, the `prompt` message with `follow`, `add -p` in the CLI
   printing the state. Verified on the VM with `claude` taking the
   prompt positionally, with a `cmd` without the placeholder, with the
   daemon restarted between the session and the delivery leaving
   `unknown`, with a resend under a known id keeping its branch, and
   with two generated names for one prompt.
3. The relay in the laptop's daemon: `add` with `relay`, the pending
   file before the answer, the pending records in the merged stream,
   the follow with backoff and the environment pin, re-follow on
   restart, the prompt scrubbed on completion, `dismiss`, the `prompt`
   message forwarded with attempts, `add --detach`. Verified with the
   laptop daemon restarted mid-add, the host's daemon restarted mid-add,
   and the host unreachable for longer than the client's three attempts.
4. The form overlay with bracketed paste in the decoder and golden
   tests, the branch proposal, `compose`, the dashboard's `a` on it with
   the foreground fallback for an older daemon.
5. The pending rows in the rows package and both views, the join by
   environment and root with the shared id, the completion predicate,
   `p` and `x`; verified under the user's tmux config with a submit from
   the popup and the row followed through to the agent working on the
   prompt.

## Out of scope

Attachments and images in the prompt. Choosing a model or a permission
mode in the form; those are the agent's command line in `agents`. A
second prompt to a running agent beyond delivering the one it was
started for. Background `run`. A queue of tasks per repository.
Templates or history for prompts. Encrypting the pending file; it is
the state directory's own protection, as `last.json` has. Journaling
the steps of `add` that inspection already covers.
