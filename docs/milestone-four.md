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
inferred from anything else, and that the host writes down two things
it decides for a command before it acts on them, since a prompt
delivered and a branch allocated are not things a retry can see by
looking.

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

## The host remembers what it decided

`add`'s steps are retried by inspection: a checkout, a branch, a
worktree, a copied file, a session in the root are there to be looked
at, so a resend after a lost connection or a daemon restart finishes
what was started. Setup commands are at least once, as milestone two
says: a crash between a command's effects and its marker reruns it.
Two things this milestone adds are neither inspectable nor safe to
repeat. A branch allocated for a generated name looks like any other
branch; a resend that allocates again makes `task-2` beside `task` and
a second worktree for one task. A prompt delivered into a pane leaves
nothing a daemon can see; a resend that finds the session cannot tell
whether the prompt is in it, and delivering again is the same task
twice, not delivering is a silent loss.

So the host's daemon keeps a journal for those two decisions: one file
per command id under `$LAATMUX_HOME/commands/`, written atomically
before the decision is acted on and rewritten as it plays out, and read
before anything is done for an id it has seen. An entry holds the
command's identity, `source`, `agent`, whether the branch was
generated, the allocated branch, the root, the launch state with the
session, the pane and the server instance the agent stage made, the
agent identity bound for delivery, the delivery state, and the
attempts. The in-memory command with its replay stays as it is for
streaming; the journal is what a resend and a `follow` consult when the
memory has nothing. The journal does not make `add` exactly once: it
protects the allocation and the delivery, nothing else.

An entry outlives its worktree. `rm` marks it `removed`; a sweep
deletes entries thirty days after they became terminal. Until then a
`follow` for an id the memory has forgotten is answered from the
entry's recorded result, and a resend under an id the journal knows is
never a new add: it resumes the recorded state, and once the entry is
terminal it is answered with the result, `removed` included, and does
nothing. An entry that is not terminal, the daemon died in the middle
of the add, is answered to a `follow` with `interrupted` and the state
recorded, and the relay resends the add under the same id, which the
host resumes: the steps by inspection, the allocation and the launch by
the journal. Only an id the journal has never seen is a new add, and
only while its submission is young, by a contract the two clocks can
keep: the relay owns a lifetime, seven days from submit by the
laptop's clock, carried on every resend as `submitted_at` unchanged,
after which it stops resending and shows `outcome unknown`; the host
owns retention, thirty days by its own clock from the moment it first
saw the id, which the sweep respects; and the host refuses an add
whose `submitted_at` is more than a day in its future or more than the
retention in its past with `submission expired`. A resend inside the
relay's seven days reaches a host whose tombstone is inside its thirty,
unless the clocks disagree by more than three weeks, which the future
check catches from the other side. A daemon that dies between writing
a decision and writing its outcome leaves a state that is reported as
`unknown` and never resolved by guessing.

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

Every connection of a relay is pinned to the host's environment id, the
check `stream` has today, so a host alias moved to another machine gets
nothing. The id comes from the host row of the merged stream when the
host has answered a hello before, and is written into the pending file
at accept. A host never reached has no id yet; then the first hello
binds it, the file is rewritten with it before the add is sent, and
every later connection is held to it. That is the narrower guarantee
for a host the laptop has never talked to: the machine that answers
first is the one the task goes to.

Connectivity is not outcome. The client's `stream` gives up after three
lost connections, which is right for a user watching; the relay follows
with the reconnect backoff the merged stream uses, for as long as the
task is outstanding, and the pending record says `host unreachable,
retrying` meanwhile, never failed. A `follow` the host answers from its
journal is a `follow` like any other; a `follow` answered `interrupted`
makes the relay resend the add under the same id, with the prompt it
still holds, and the host resumes; a `follow` the host does not know at
all, memory and journal, means the host never took the add, and the
relay resends it, which is the one case a resend is a start; a
`follow` refused as `submission expired` is `outcome unknown` for the
user. The same holds for an attempt: a `follow` with an attempt the
host has never seen makes the relay resend the `prompt` message under
that attempt number.

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
  alive and its identity is verified, not the tentative one an
  environment hint gives before the agent's own process is found; and
  the pane is the one the journal names, on the server instance it
  names. That verified identity is bound into the journal at that
  moment, before the first attempt, and every later attempt requires
  the same one. The daemon loads the prompt into a tmux buffer named
  for the attempt, pastes it with bracketed paste, sends Enter, deletes
  the buffer, and does this once. A pane that is not ready within a
  minute gets nothing.

The agent stage is journaled as two transitions, whichever way the
prompt goes. `launching` is written before `new-session`, with the
root, the session name and whether the argv carries the prompt;
`launched` after it returns, with the pane and the server instance. On
the argv path `launched` is `delivered`: the process was started with
the prompt as its argument, and that is the handoff. On the typed path
delivery is a third and fourth transition, `attempting` before the
paste and `delivered` or `not delivered` after it. A daemon that dies
between `launching` and `launched` leaves `unknown`, whether or not a
session is in the root: with one, the agent may have the prompt, or
may not; without one, an agent may have started with the prompt, done
its work and exited, since the managed server does not keep a pane
whose command ended. Nothing about the session says which, so no
resend launches again; the user reads the state and decides, and the
cost of that window, a task submitted twice by hand, is taken over the
same task run twice by a daemon. A daemon that dies between
`attempting` and the outcome leaves `unknown` the same way.

Delivery is a state of its own, `prompt` in the result and in the
pending record:

- `none`: the add carried no prompt. Complete. Nothing else is `none`.
- `delivered`: the journal says the argv launch returned, or an attempt
  reached Enter. Complete.
- `not delivered: <reason>`: the pane was not ready in time, the paste
  failed before anything reached the pane, or the add found a managed
  session already in the root and was not a resend of the launch that
  made it, `session existed`, the case of `a` on a worktree row that
  has a session, or of a branch reused on purpose. Needs the user.
- `unknown`: a crash window above, or an error after a side effect
  may have taken: `new-session` failing after the session was made,
  which the managed server's configuration step can do, or the paste
  done and Enter refused. Needs the user, and no daemon delivers again
  on its own. `not delivered` is reserved for what provably did not
  transfer: the pane never ready, so nothing was pasted, or the paste
  refused before it began.

Nothing is inferred from a session's existence. The skip path takes the
journal's word: a resend under a known id resumes the recorded state; a
new id that finds a session is `session existed`; a known id whose
entry has no launch recorded and finds a session is `session existed`
too, since the launch was not this command's.

A result whose delivery needs the user keeps the pending row, with the
reason, and the prompt stays in the pending file until the user acts.
`p` on the row starts a delivery attempt: the relay writes the attempt
number into the pending file first, allows one unresolved attempt at a
time, and sends `{type: prompt, id, attempt, prompt}` to the host, where
the journal serializes attempts per session, answers a repeat of the
same number with its recorded outcome rather than pasting again, and
lets the relay `follow` an attempt whose reply was lost; a laptop daemon
that restarts with an attempt unresolved follows it before anything
else. The host checks the target against the journal: the root must
still be the worktree, the session's single pane the one recorded, the
server instance the same, the agent identity the bound one; a
replacement session or agent is refused with `not delivered: session
replaced`, and the user decides. A row whose journal has no target,
`session existed` or an `unknown` from a launch that never recorded its
pane, has `p` too, and there it adopts one, since the user pressing it
is the decision the journal lacked: the host takes the managed session
in the root, if there is exactly one and its single pane has a
verified live agent, records it as the target and delivers; no such
session, or an agent not verified, is `not delivered: no agent to
deliver to`. `p` is offered only on a row whose state is `not
delivered` or `unknown` with the prompt retained and no attempt
unresolved. `x` dismisses the row and deletes the file. `jump`
works on the row meanwhile, since the agent is up. `laatmux add -p` in
the foreground prints the delivery state as its last line and exits 0
when the add succeeded whatever the delivery, since the worktree and
the agent are there; the state is what the user reads.

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
The paste buffer is deleted whether the paste worked or not, and a
daemon that starts deletes every buffer named as an attempt's on the
managed server, which a daemon killed between loading and deleting
leaves behind. Nothing logs it.

The host advertises the whole of this, the journal, the allocation,
the prompt and the listing after the result, as one capability,
`task`. Every relayed add requires it, prompt or not, since a relay
counts on the journal and the listing, and every foreground add that
carries a prompt requires it; the check is on the first connection and
on each reconnect, as `follow` and `run` are checked today. A host
without it refuses the submit with a message, it does not take the add
and drop what it does not know; a foreground add without a prompt
works against it as today. The form reads the cached capability from
the host row and says `tasks not supported by <host>'s daemon` in its
footer before submit, and `--detach` to such a host is refused the
same way.

## The branch name

The branch line is filled from the prompt as it is typed: the first
words, lowercased, runs of anything but letters and digits turned into
one `-`, trimmed, cut at forty characters on a word boundary. It is a
proposal. A generated name is submitted as such, and the host makes it
unique in an `allocate` step of its own after `fetch`, when the local
and remote branches are current and the repository lock is held so two
adds cannot race: the first free of `<name>`, `<name>-2`, `<name>-3`
against the local branches, the remote branches, the registered
worktrees, and the names allocated by journal entries that are not
terminal, which is what reserves a name between its allocation and its
branch across a daemon death. The allocation is written to the journal
before the branch is made, so a resend takes the allocated name and
never allocates again, and an entry that ends terminal without its
branch made releases the name. The `allocate` progress line and the
result carry the branch used, and the pending record shows it from
then on. A name the user edited is explicit and behaves as `add
<branch>` does today, reusing a branch that exists, which is what `a`
on a worktree row without a session wants. `add`'s validation applies
at submit, so a name git would refuse is refused in the form with the
same message.

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
`done, awaiting the listing` between a success and the host's worktree
listing that follows it, `done, worktree gone` when that listing has
no worktree at the root; the title line is the detail or the reason.
The row is not dim while running and dim once it needs the user, as a
stale row is. `Enter` on a running one does nothing; on one with a
session it jumps; `p` on one whose prompt is retained delivers it; `x`
on one that needs the user dismisses it, with a confirm line.

Complete is one predicate, used by the daemon to retire a record, by
the rows to hide the worktree row behind the pending one, and by the
views to offer `p` and `x`: the add succeeded and the delivery state is
`delivered` or `none`. A record that is complete is retired on a
worktree listing that is after the result, which the host makes
provable without ever holding the result for it: its worktree listings
are numbered within a daemon generation, `listing` is the generation,
the daemon's start, and a count of the polls that succeeded in it, the
poll and its publication run under one lock so an older observation
never overwrites a newer, a snapshot carries the listing it reflects,
and the result carries the barrier, the count after the last listing
that completed before the add's mutation, in the current generation.
`finish` pokes a poll as today and emits the result at once; a listing
that fails leaves the count where it was and the host row's error
saying why, and the barrier stands. The relay, on its own connection,
takes plain snapshots after the result until one satisfies the barrier:
the same generation at that count or higher, or any later generation
with a successful listing, since a daemon that started after the
mutation lists after it. A worktree at the root in that snapshot hands
the row over to the worktree row, once the merged stream shows it too,
and the file goes; no worktree at the root is `done, worktree gone`, a
row that needs the user only to be dismissed, and the file stays until
then. Until such a snapshot the row says `done, awaiting the listing`,
with the host's listing error when there is one, and the outcome is
already reported: the foreground command returns, the relay's channel
closes, only the retirement waits. While a
pending record exists that is not retired, the worktree row for the
same environment and root is not drawn: the pending row stands for it,
with more to say.

A pending record and the worktree row it will become are joined by
identity, not by name: the record carries the host's environment id,
the repository source, and the root from the moment the host reports
it, in a field of the `allocate` progress line, and the rows package
joins on environment and root. The row's id is the command's, always;
a pending row whose root is known also carries the worktree row's id,
`<environment>/worktree/<root>`, as its alias. The view's anchor is a
pair: when the model anchors on a row it remembers the row's id and its
alias, and after a refresh it takes the row whose id is the anchor's
id, else the row whose id is the anchor's alias, which is the worktree
row once the pending one has gone, else the row whose alias is the
anchor's id, and re-anchors on what it found. The pair lives in the
model, so it survives a refresh that coalesced the intermediate state.
A view may never have seen the alias at all: a row selected during
`clone` has no root yet, and the record may be retired before the next
refresh, or while the view was disconnected. So the handoff is carried
by the stream as well: the `remove` of a retired record names the
worktree id it became, and a merged snapshot carries the recent
handoffs, the last hour's pairs of command id and worktree id, which
the merged client keeps in a map of its own that a resnapshot merges
into rather than replaces. The model's anchor lookup consults that map
last: an anchor whose id is a command the map has retired re-anchors
on the worktree row it names. So a task selected during `clone` stays
selected when the root arrives, when the worktree row takes over, and
through the reorder the sort makes, whether or not the view saw the
steps between; two pending records for one explicit branch are two
rows with two ids and one alias, the anchor follows the one that was
selected, and the worktree row is hidden while either stands.

A pending record that needs the user stays until dismissed, through
laptop daemon restarts, with its outcome and reason in the file; a
daemon that starts and finds a record whose result is recorded does
not resubmit it, and one with an attempt unresolved follows the
attempt. One whose host is gone from the config stays too, with `host
removed`, so nothing the user asked for disappears without them.

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
<- {type: snapshot, seq, hosts, agents, worktrees, sessions, pendings: [...], handoffs: [{id, worktree_id}]}
<- {type: upsert, seq, pending: {...}}
<- {type: remove, seq, pending_id, worktree_id}    worktree_id when retired into a worktree row
```

The pending record, without the prompt:

```
{id, host, environment_id, source, repo, branch, generated, agent,
 submitted_at, taken, reachable, stage, state, detail, root, session,
 done, error, prompt, attempt, attempt_open}
```

`taken` is that the host has the add; `reachable` is the relay's
connection, the connectivity axis kept apart from the outcome as issue
#1 wants; `stage`, `state` and `detail` are the last progress message's;
`prompt` is the delivery state, `attempt` the number of the last
attempt and `attempt_open` that it is unresolved. Ids are the client's,
`add-<pid>-<nanos>` as today, so a relay resent after a lost laptop
daemon is the same id on the host and attaches rather than starts
again.

On the host, under a new capability `task`:

```
-> {type: add, ..., prompt, generated, submitted_at}       submitted_at unchanged on every resend
<- {type: progress, id, n, stage: allocate, state: done, detail, branch, root}
<- {type: result, id, ok, root, session, pane_id, branch, prompt: <state>, listing: {generation, n}, error}
-> {type: prompt, id, attempt, prompt}
<- {type: result, id, attempt, ok, prompt: <state>, error}
-> {type: follow, id, attempt, after}              an attempt whose reply was lost
<- {type: result, id, ok: false, error: interrupted, stage}   a journaled add the daemon died in
<- {type: result, id, ok: false, error: submission expired}   an add older than the journal keeps
<- {type: snapshot, seq, listing: {generation, n}, ...}   the host's own stream, numbered listings
```

The delivery state is `none`, `delivered`, `not delivered` or
`unknown`, with the reason in `error` where there is one. `generated`
asks the host to allocate the branch; `branch` and `root` on the
`allocate` progress line and on the result are the ones used;
`listing` on the result is the barrier, the daemon generation and the
count a successful listing after the add's mutation will reach, and on
a snapshot the generation and count of the listing it reflects; a
later generation with any successful listing satisfies a barrier from
an earlier one. A `prompt` message is a command like `add`, with
the journal behind it: a repeated attempt is answered from the record,
`follow` with an attempt reattaches to one in flight, and `follow` with
an attempt the host never saw is answered `unknown attempt`, on which
the relay resends the message. `follow` without an attempt, for an id
the memory has forgotten, is answered from the journal: the recorded
result, or `interrupted` with the stage reached, on which the relay
resends the add. A host without the capability refuses a relayed add,
and a foreground add with a prompt, on the client's side, before it is
sent.

## Order of work

1. This note.
2. The journal and the prompt on the host: capability `prompt`, the
   journal under the state directory with tombstones, `removed` from
   `rm` and the sweep, the age refusal, `follow` answered from it with
   `interrupted` for an unfinished entry, `generated` branches
   in an `allocate` step after `fetch` reserving against the journal,
   the field in the add message, `{prompt}` in an agent's `cmd` with
   the argument redacted in any tmux error, the launch transitions, the
   typed-in delivery through the detector's readiness with the bound
   identity and a `Paste` on the managed server, the attempts and their
   states, adoption on `p` where no target was recorded, the `prompt`
   message with `follow`, the numbered listings by generation and the
   barrier on the result, the sweep of attempt buffers at start, `add
   -p` in the CLI printing the state. Verified on the
   VM with `claude` taking the prompt positionally, with a `cmd` without
   the placeholder, with the daemon killed between `launching` and
   `launched` leaving `unknown`, with a resend under a known id keeping
   its branch, with two generated names for one prompt, and with a
   `follow` after `rm` answered `removed`.
3. The relay in the laptop's daemon: `add` with `relay`, the pending
   file before the answer, the environment bound at accept or at first
   contact, the pending records in the merged stream, the follow with
   backoff and the resend on `interrupted`, the seven-day lifetime,
   re-follow on restart, the snapshots after the result up to its
   barrier and the retirement rule, the handoffs on `remove` and in
   snapshots, the prompt scrubbed on completion, `dismiss`, the
   `prompt` message with attempts persisted first, `add --detach`.
   Verified with the laptop daemon restarted mid-add and mid-attempt,
   the host's daemon restarted mid-add, and the host unreachable for
   longer than the client's three attempts.
4. The form overlay with bracketed paste in the decoder and golden
   tests, the branch proposal, `compose`, the dashboard's `a` on it with
   the foreground fallback for an older daemon.
5. The pending rows in the rows package and both views, the join by
   environment and root, the anchor pair in the model with the handoff
   map, the completion
   predicate, `p` and `x`; verified under the user's tmux config with a
   submit from the popup and the row followed through to the agent
   working on the prompt.

## Out of scope

Attachments and images in the prompt. Choosing a model or a permission
mode in the form; those are the agent's command line in `agents`. A
second prompt to a running agent beyond delivering the one it was
started for. Background `run`. A queue of tasks per repository.
Templates or history for prompts. Encrypting the pending file; it is
the state directory's own protection, as `last.json` has. Journaling
the steps of `add` that inspection already covers, or making setup
commands exactly once.
