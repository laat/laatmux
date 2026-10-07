# Milestone five: two views, and the sidebar at workmux's level

Design note for issue #52. The sidebar shows the right rows but little
about them: an ASCII mark, `repo/branch`, the host tag, the agent with
its activity and age, and the pane title, in the terminal's default
colour. workmux's sidebar, on the same kind of rows, shows at a glance
which agents want the user, which are done, how big each branch is
against its base, what is still uncommitted, and whether its PR's checks
pass. This milestone brings all of it to laatmux, read from workmux
v0.1.266's source, and splits the sidebar into two views.

laatmux is at its core a worktree manager across hosts, with the agents
as a status layer on top. Since #55 the host attributes every pane and
run to its worktree, so a worktree can have any number of agents,
shells and jobs. One flat list cannot show both sides: it has to pick
one agent per worktree row and give the rest rows of their own. So the
sidebar and the dashboard get two views over the same state: one for
the agents, one for the worktrees.

This milestone reverses five rules of milestone three, each on purpose:
ASCII marks by default, "age is never a reason" to dim, no diff or PR
state, no top-edge sidebar, and no per-session scope. Each reversal is
named where it happens, with the option that restores the old rule
where there is one.

Nothing here changes the model. Git and the daemons stay the sources of
truth, and detection never crosses the network. The merging daemon
forwards records as it decodes them, so a new field on a host's record
reaches a view only through a merging daemon that knows it, as #55's
`attribution` does. What is new is state the laptop's daemon keeps for
itself: what the user has seen, and the PR and check state of each
branch. The git state of a worktree is computed where the worktree
lives, by its host's daemon.

**Words.** *Stale* is workmux's word for an agent idle for long, and
this note uses it so. What laatmux has called a stale row until now, a
local workspace session whose worktree is gone from a host that can say
so, is renamed *orphaned*: `Row.Stale`, the `stale` group and its header
become `Orphaned`, `orphaned` and `orphaned N`, in step 2, before stale
by age arrives in step 3.

## Two views

The sidebar and the dashboard both have two views, with a tab header
above the list:

- **Agents**, the default: one tile per agent, the most pressing first.
  This is where the user looks to see what wants them.
- **Tree**: the repositories, their worktrees, and what runs in each.
  This is where the user looks to see what exists and where it runs.

`Tab`, or a click on a tab header, switches the view of the pane it is
pressed in; other panes keep theirs. The last view chosen is written to
`sidebar.json` (see Persistence) as the default a new or restarted pane
starts in, which no running pane reads. The dashboard keeps its own
view and layout defaults in the same file, under keys of its own, since
it opens in a wide popup where compact suits. A `--layout` given on its
command line wins, told apart from the flag's default with `fs.Visit`;
without one the stored dashboard default applies, then compact. Its
view is its own stored default, else `agents`; neither `sidebar.view`
nor the CLI touches it. The
dashboard starts at scope `all`, whatever the sidebar's default; `F`
narrows it to the `session` scope (see Placement, scope and controls)
of the client the popup opened on. `--all`
does not reach it, since it has no socket.

### The agent view

- **Rows:** every agent, one tile each, in `sort` order. The primary
  label is the worktree's branch, the secondary the repository and host;
  an agent in no worktree is labelled by its session.
- **Tasks:** the relay's pending tasks come first, as today, the newest
  first.
- **Left out:** shells, servers, runs, and worktrees without an agent.
  They are the tree's.
- **Agents that share a primary label,** several in one worktree, or
  several in one session outside any worktree, are told apart by a
  `(1)`, `(2)` suffix, in the tree's child order, start time, so the
  tiles and the tree agree. `{pane_suffix}` is that suffix.
- **Stale:** stale agents and those of settled workspaces fold into
  `▸ N stale` at the end, unless blocked or done, or the viewer's own
  (see Precedence).
- **Orphaned sessions** have no agent and are not in this view; they
  are the tree's.
- **Enter** on a tile goes to the agent's pane, as on an agent in the
  tree (see Jumps).
- **Empty state:** `No agents running`.

### The tree view

```
laatmux
  ▾ agents-config (vm)       +46 -11 ✎ +28 -3   #52 ✓
      ⠋ claude               Adding per-agent config…
      $ zsh
      ▶ make test            0:42
  ▸ auto-layout (vm)         +318 -87            #49 × 3/5   ✅
  ▸ fix-sidebar (mac)        ✎ +4 -1
anki-llm
  ▾ batch-processing (mac)
      💬 claude              Refactoring queue handl…
other sessions
    scratch (vm/default)     💬 claude
```

- **Repositories at the top**, one node per source. #53 made the ssh and
  https forms of one repository the same source, so a repository with
  worktrees on several hosts is one node. It is named with this
  machine's label for the source, else the host's.
- **Worktrees under their repository**, named by branch, or by the
  root's base name when detached. The host is shown in parentheses
  after the name, never as a level of its own. The worktree line
  carries the git stats and the PR and check state, and, when folded,
  the status icon of its most pressing agent.
- **Children of a worktree**, from #55's records:
  - its agents, with status icon, agent name and pane title;
  - its other panes, with the command: `$ zsh`, `npm run dev`;
  - its running jobs, `▶` with the command and the elapsed time.

  Agents come first, by start time, then panes by session and window,
  then runs by start time. The client keeps the pane and run records
  the merged stream carries, which it drops today. laatmux's own panes,
  the sidebar panes and the attach panes of workspace sessions, are not
  children: the host daemon reads their tags, `@laatmux_sidebar` and
  `@laatmux_attach_pane`, in its pane listing and publishes no pane
  record for them.
- **An older host,** without `attribution`, has no worktree ids on its
  agents and no pane or run records. Its agents are placed by the join
  `rows.Build` makes today, the worktree's session, and the rest go to
  other sessions.
- **Orphaned sessions** sit under their repository, by the source in
  their `@laatmux_repo` tag, as a worktree line marked `worktree gone`,
  or in other sessions when the tag is missing. `x` removes them, as
  today.
- **Tasks:** a pending task sits under its repository as a worktree line,
  with the task's state where the git stats would be, until it hands
  over to the worktree line at its root. While a task stands for a
  worktree the host lists, one the add made, a new worktree whose
  prompt was not delivered say, or one that already existed, an add of
  an existing branch, the task's line takes the worktree line's place,
  with the worktree's children under it, as its row does today. A task
  at a root that no longer stands, one that failed after the worktree
  was made, whose host left the config or answers as another machine,
  or whose worktree went and was made again, is a line of its own
  beside the worktree line, as today. When several tasks stand for one worktree, two adds of one branch
  say, the newest submitted owns the children, so each record is one
  node; the others follow it as task lines without children. When the
  owner hands over or stops standing, the next newest takes the
  children, and when none is left the worktree line has them again.
  Following, a switch, a handoff and `session` go to the owner's line.
  Before the host lists the worktree, a standing task's line holds the
  agent in the task's session whose pane starts at the task's root, as
  its row does today, so that agent is not in other sessions meanwhile;
  with several such tasks the newest owns it.
- **Other sessions** is the last group. It holds agents in no worktree,
  a session `new` made or one observed on a default server, each tagged
  with its host.
- **Order:** repositories by name, worktrees by branch. Status never
  reorders the tree, so it stays put while agents work.
- **Folds:** repositories and worktrees fold. A worktree gets its fold
  once, the first time the pane shows it holding a child: open when an
  agent in it is blocked, working or done, folded otherwise. A line
  hidden behind a task line is not shown, so not yet seen. After that it stays as it
  is until the user toggles it, so the tree does not open and close as
  agents work; a folded worktree line shows the icon of its most
  pressing agent, so a blocked or done agent inside is not missed.
  Repositories start open. A task line that holds children folds as a
  worktree line does, by its own node id and the same rule: it gets its
  fold when first shown holding a child, the add's agent say, not when
  it appears empty at submit. The node that takes its children, the
  worktree line at handoff or the next newest task, takes the task
  line's fold, unless the user has toggled that node's own fold, so a
  fold the user set survives the handoff. A toggled fold carried so is
  written to `sidebar.json` under the node's id, as a toggle is (see
  Persistence).
- **Jumps:** `Enter` on a worktree line jumps to its workspace session,
  as the row does today. On a repository line it folds and unfolds. On
  an agent or a pane it goes to that pane, routed by the record's
  server and session, not by the worktree's:
  - **a pane in the worktree's home session** on the host's managed
    server: the jump to the workspace session, then a `select` command
    to the host's daemon (below), which runs `select-window` and
    `select-pane` on its managed server. The attach shows the managed
    session's current window and pane, so the selection is what it
    shows, whether the attach was just made or had been there. On the
    laptop the jump also selects the attach pane's window and the attach
    pane in the workspace session, found by its `@laatmux_attach_pane`
    tag, since the user may have left that session on a shell window;
  - **a pane in another managed session:** the jump an agent row makes
    today, through the plain attachment `<host>/<session>`, then the
    same `select` and the same selection of the attach pane;
  - **a pane on this machine's default server:** `switch-client` to its
    session, then `select-window` and `select-pane` there, all local;
  - **a pane on a remote host's default server or another observed
    server** is refused as `jump` refuses it: laatmux attaches to managed
    sessions only.

  `select` is a new command on the host's daemon, `{type: select, id,
  pane_id}`, capability `select`. It reaches the host over the same
  connection every command does, the ssh bridge for a remote host, never
  through the attach, which is an interactive terminal. It acts on the
  managed server only and answers with a result; a pane gone answers an
  error, and the jump then stays on the session. Against a daemon
  without it the jump goes to the session and leaves the pane as it is.
- **Empty state:** `No worktrees`.

With both views, most of the rule #56 needed for which agent a mixed
worktree row shows goes away: the tree shows every agent under its
worktree, and the agent view lists every agent. What remains is the
worktree line's own jump when the worktree has no home session: it goes
through the agent laatmux made at the root, or one on this machine's
default server, as `rowSpec` does now.

### Switching

The selection follows the user across a switch, resolved from the
selected node to a node of the other view:

- **from an agent,** the tree lands on that agent inside its worktree,
  unfolding the worktree and the repository;
- **from a worktree line or one of its panes or runs,** the agent view
  lands on the worktree's first agent in the tree's order;
- **from a repository line,** on the first agent of its first worktree;
- **from a task,** on the same task, which both views show. A task that
  hands over while selected moves to the node its handoff names, in the
  tree the worktree line, or while another task stands for that
  worktree the owner's line; in the agent view that worktree's first agent,
  as from a worktree line.

A target the stale fold hides opens the fold; a target in a folded
worktree or task line opens it and its repository. Those opens are the
pane's own and are not written to `sidebar.json`. A target the filter
hides, or none at all (a worktree with no agent, an empty repository),
leaves the selection on no row, as a selected row that goes with
nothing to hand over to does today; the next move puts it on the first
row.

**Following.** A selection that follows the viewer's own session keeps
following it in the new view. In the agent view it follows the first
tile in sort order among the agents of the viewer's session or
worktree; in the tree, the worktree line of the viewer's worktree, or
the task line standing for it or at its session's root, the session a
task's add or jump made before the host listed the worktree; or, with
no worktree and no task, the viewer's session's own line, wherever it
sits: under its repository for an orphaned session with a repository
tag, in other sessions otherwise.

### The rows package

`rows.Build` makes one mixed list today. It splits into two builders
over the same input: `Agents`, a flat list of agent and task rows, and
`Tree`, a list of nodes with a depth, a kind (repository, worktree,
task, agent, pane, run, group), a fold state and an id. The view draws
either from the same model. The selection anchors on node ids. A
record's node has the record's id; a repository node has
`repo/<source key>`, the source normalised as #53 does; the
other-sessions group `group/other`; the agent view's stale fold
`fold/stale`; an orphaned session `session/<name>`, as today. A switch
resolves as above, and a task follows its handoff. `ls` prints the
tree, as text, from the tree builder: one line per repository, one per
worktree with its host, and one per agent under it.

## Status

laatmux's detector already gives `working`, `blocked`, `idle` and
`unknown`. workmux's statuses map onto them:

| workmux | laatmux | icon | colour |
|---|---|---|---|
| Working | `working` | 2-cell braille spinner, 250 ms a frame, where the view's spinner is one cell at 100 ms today | info (cyan) |
| Waiting | `blocked` | 💬 | accent (purple) |
| Done | `idle` after `working`, not seen since | ✅ | success (green) |
| (none) | `idle` and seen, `unknown`, no agent | blank | — |
| Stale | idle for more than an hour, or settled | 💤 | dimmed |

### Precedence

- **An agent's status**, for its icon and its place in `priority`
  order: blocked, then done, then working, then idle and unknown, then
  stale. A done agent is never stale: an unseen finish is what the user
  most needs, however old. A blocked agent is never stale either.
- **Settled** is the workspace's, not the agent's: the agents of a
  settled workspace fold with the stale ones and show 💤, unless blocked
  or done, which stay in place with their own icon. An agent observed
  on this machine's default server in a window of the workspace
  session, where no worktree line takes it as a child (the session's
  worktree on another host, say, or the agent's directory in no
  worktree), is one of its agents too: it stands in other sessions
  with the session's state, and `z` on it changes its row as well as
  the session's line, when a line holds the session. One that a line
  takes as a child, by its directory, is that line's. A managed agent
  whose directory is in no worktree, in a worktree's home session (or,
  with the home lost, in the session of the agent laatmux made at its
  root), stands in other sessions with the settled state of that
  worktree's workspace session, where `Enter` on it lands; `z` and `S`
  on it act on that session, and with the viewer there it is the
  viewer's. Under a task standing for the worktree it shows no state,
  and `z` refuses it as the task's. When a worktree's root agent is
  moved by hand into another worktree's session, the session is no
  longer either worktree's home, its panes not all in one root, and it
  stays the worktree's it is named after, as add names it, by the
  host's label and the branch: this agent goes there. When the name
  fits neither (the label or the branch renamed since, or a detached
  worktree), the first of the two in the tree's order has it. `Enter`
  on a worktree's own agent or pane goes to its own workspace session
  whenever that attaches the agent's session, as `z` and `S` on it do.
  The viewer's own row never folds: settled or stale, it stays in
  sight, sorted with the stale ones, so `z` in the sidebar can undo
  itself.
- **A worktree's status**, on its folded line and for `{worst_status}`:
  its most pressing agent's, in the order above; with no agent, none.

### Done and seen

An agent that went from working to idle is *done* until a client has
shown it (see Seen) since that change.

Every time here is the laptop's: hosts' clocks are never compared with
it.

- **What is tracked.** Per agent, keyed by the agent's id and its
  identity (pid and start, from the record), since tmux reuses pane ids
  after a server restart: the last activity seen and that record's
  `activity_at`, which is the host's and used only as an opaque mark of
  the transition, and two laptop times, `finished_at` and `seen_at`.
- **When.** The daemon's own host's agents are tracked always, from the
  records its poll makes, subscriber or not. A remote host's agents are
  tracked while the daemon follows the host, which is while a merged
  subscriber is there and for the idle minute after; a finish while it
  was not following is seen in the next snapshot, as below.
- **A finish.** An upsert, or a host's snapshot, that shows the agent
  `idle` with the same identity and a new `activity_at`, where the last
  activity kept was `working`, is a finish: `finished_at` is the laptop's
  clock at that moment. A snapshot counts as well as an upsert, so an
  agent that finished while the laptop slept, its daemon was down or
  the host was not followed is done when the host's snapshot comes,
  against the state kept on disk. The cached records the merged stream
  keeps for a host while it reconnects are not a source: only the
  host's own upserts and snapshots are. A record with another identity,
  a new agent in the same pane, starts over: no finish, and the old
  times go.
- **Not tracked:** an agent on a remote host's default server or on
  another observed server. No view can take the user there, so it can
  never be seen; it never shows ✅.
- **Seen.** The daemon learns what each tmux client of this machine's
  default server shows through one `list-clients -F` with
  `#{client_name} #{window_id} #{pane_id} #{pane_dead}
  #{@laatmux_sidebar} #{@laatmux_attach_pane} #{@laatmux_attach_target}
  #{@laatmux_host} #{@laatmux_attach} #{@laatmux_workspace}`, options
  resolving through the client's current pane and session,
  every second while an entry is unseen and a view is open or closed
  less than the idle minute ago, a view being any merged subscriber: a
  sidebar, the dashboard, or a one-shot client such as `ls` or `jump`.
  It lists once, right away, when it records a finish, view or not, and
  on a poke. The idle minute covers a jump from the dashboard, whose
  subscriber leaves as the jump lands, so a dashboard-only user's jump
  is seen. A host's daemon has no merged subscriber, the laptop
  following it through a plain subscription, so it lists only when one
  of its own agents finishes, not once a second for as long as one
  stays done. The laptop follows the same rule: a visit made while no
  view is open, and none closed in the last minute, is recorded only by
  a finish or a poke, or by a view opened later if a client still shows
  the agent then. This reverses the first version of this note, which
  listed every second while any entry was unseen, subscriber or not, to
  record such visits, and accepted that cost; #124 found it paid on
  every host's daemon, for agents only the laptop's views show. A
  client *sees* an agent by the pane it shows:
  - a live attach pane, tagged `@laatmux_attach_pane` and not dead,
    whose target is the agent's managed session and whose session's
    `@laatmux_host` is the agent's host, in a workspace session or a
    plain attachment alike. A dead attach pane, kept by
    `remain-on-exit` after its ssh ended, shows old output and sees
    nothing. The target is `@laatmux_attach_target`; an attach pane from
    before #56 has none, and its target is then the session named in a
    plain attachment's `@laatmux_attach`, `<host>/<session>`, or for a
    workspace session the home session of the worktree its key names;
  - on this machine's default server, the agent's own pane.

  The pane a client shows is its window's active pane, except that a
  focused sidebar pane stands for the pane beside it: the user reading
  the sidebar is looking at the attach or the agent next to it. When the
  active pane is a sidebar pane, one `list-panes -t <window>` finds the
  window's live attach pane, or its one other pane, and that is the
  pane shown.

  A client on a workspace session's shell window sees nothing. The
  laptop does not know which window of a managed session the attach
  shows, so every agent in that managed session counts as seen: that is
  the one coarseness left. When a seen agent's `finished_at` is after
  its `seen_at`, `seen_at` moves to now, and a finish observed while a
  client shows the agent is seen at once. `seen_at` moves only then, so
  a client sitting on a session writes nothing. Every attached client
  counts, one left open in another terminal too.
- **The poke.** The sidebar's `on` sets three hooks running `laatmux
  sidebar seen`, which sends the local daemon `{type: poke}` so it lists
  clients at once: `client-session-changed[9106]`,
  `session-window-changed[9107]` and `window-pane-changed[9108]`, since
  seen depends on the pane a client shows and a jump's `select-window`
  onto the attach comes after its `switch-client`. They are global,
  with `--session` too, since the sessions a client moves into are the
  workspace sessions and attachments, not the one holding the sidebar;
  `off` removes them only when no sidebar pane is left. The poll covers
  a missing hook; the hooks only make it quicker.
- **The record.** A new record type in the merged stream, from a
  merging daemon with the capability `attention`:
  `{agent_id, finished_at, seen_at}`. A view shows ✅ for an idle agent
  whose `finished_at` is after its `seen_at`.
- **Kept across restarts** in `attention.json` under the state
  directory, written when an entry changes, so a restart does not bring
  back ✅ on everything. An entry goes with its agent's remove, one
  whose agent a listed host's snapshot no longer has goes with that
  snapshot, and the entries of a host no longer in the config go when
  the daemon starts and when a subscription reads the config.
- **Limits.** A visit made while the laptop was not following the
  agent's host, or before the snapshot that shows the finish arrived,
  is not recorded, since the finish is dated when it is seen: the agent
  shows ✅ though the user was there. Nor is a visit made while no view
  is open and none closed in the last minute, a move by tmux's own keys
  after the dashboard closed or before the first view after a daemon
  restart say: only a finish or a poke lists the clients then. Only
  this machine's clients count: a session attached directly on a host,
  not through laatmux, is never seen, so an agent the user works with
  that way shows ✅ until they visit it through laatmux or it starts
  working again. An agent the laptop has never seen working shows no
  ✅.

This is the laptop's knowledge, not a host's, so no host daemon changes.

### Interrupted: nothing to add

workmux marks an agent interrupted when its pane is unchanged for 10 s
while its hooks still say working. laatmux reads the screen instead of
hooks. The check for this milestone was a real capture: Claude Code
2.1.284 on the VM, 100x40, a turn interrupted with Esc and captured four
seconds later. The screen shows `⎿ Interrupted · What should Claude do
instead?` above an empty prompt box, and the footer no longer says
`esc to interrupt`. It reads as `idle` through `live_prompt_box`, with
the prompt box visible, so the working-to-idle debounce does not hold
it. The capture is a detection fixture, `claude-interrupted`, in
`TestRealFixtures`. What the capture proves is the detection: idle, at
once. Whether the agent then shows ✅ is the seen rule's: the interrupt
is the user's key typed into the attach pane that shows the agent, so a
client shows that pane when the idle arrives, within a poll, and the
finish is seen at once. A user who interrupts and leaves before the poll, or who works in
the session through a direct attach on the host, gets a ✅ they did not
need; the tests for step 3 cover the first case.

The fixture is one screen, taken after the interrupt, and proves the
detector's half. The daemon's half, that `nextActivity` passes a visible
idle through without the debounce, gets its own test in step 3. While
capturing, the title read `✳ Essay about terminals` six seconds into
the turn, with no spinner. Step 3's working capture of 2.1.284,
`claude-working-2.1.284`, confirms it: seven seconds into a turn the
title is `✳` and the topic, `osc_title_working` misses it, and
`live_turn_working`, the footer's `esc to interrupt`, carries the
working state.

### Stale and settled

A row whose agent has been idle for more than `stale_after`, an hour by
default, measured from the host's `activity_at` as the age on a row is
today, is stale: dim, with 💤, sorted after the live ones, and folded
in the agent view. This reverses milestone three's "age is never a
reason" rule; `dim_stale: false` stops the dimming and
`collapse_stale: false` the fold, while the icon and the place in
`priority` order stay. The viewer's own row is never folded away, stale
or settled, so `z` in the sidebar can undo itself.

workmux's sleep is laatmux's settle, and moves to workmux's key: `z`
settles or unsettles the selected workspace in the sidebar and the
dashboard, whose agents then show 💤 and fold with the stale rows (see
Precedence). #52 had `z` in the view; settle was the dashboard's only,
and the sidebar gets it now too.

### Icons

Icons are emoji by default, which reverses milestone three's ASCII
default. Two other sets can be chosen:

- `icons: nerdfont`: `\u{f075}` waiting, `\u{f0134}` done, `\u{f04b2}`
  stale;
- `icons: ascii`: the `!` `*` `-` marks `ls` prints.

Each status icon can be overridden in config. Agent icons are a template
token, off in the default templates: `CC` for claude in #d97757, `CX`
for codex in #10a37f, and the rest of workmux's table for the agents
laatmux detects, each overridable.

### Times from the hosts

Done and seen use the laptop's clock only. The other times a view shows
are the hosts', as today: the age since `activity_at`, stale by age,
the `recency` sort, and a run's elapsed time from `started_at`, each
against the laptop's clock. A host whose clock is off by minutes shifts
them by as much; a time in the future shows as `0:00`. Hosts are
expected to keep their clocks set; laatmux does not correct them.

## Diff stats, on the host

The worktree lives on its host, so git is read there, by the daemon
that lists the worktrees. The worktree record gets a `git` object,
behind a new capability `git-status`:

```json
"git": {"base": "origin/main", "committed": [46, 11], "uncommitted": [28, 3],
        "uncommitted_partial": false, "ahead": 2, "behind": 0, "dirty": true,
        "conflict": false, "rebasing": false, "stale": false, "changed_at": "…"}
```

`changed_at` is when a value last changed, so an upsert and a snapshot
carry the same; the time of the last refresh is not in the record.

- **committed:** `git diff --numstat <base>...HEAD`, the branch against
  its merge base.
- **uncommitted:** `git diff --numstat HEAD`, staged and unstaged, plus
  the line counts of untracked files that are not ignored, from `git
  status`'s untracked entries. A binary file counts 0. At most
  200 untracked files are read, each only up to 1 MB; past either the
  count is marked `+` as a lower bound.
- **ahead, behind:** `git rev-list --left-right --count <base>...HEAD`,
  against the base, as the dashboard shows them beside `→base`. A branch
  `add` makes has no upstream, so `git status`'s counts, which are
  against the upstream, are not used.
- **dirty:** from `git status --porcelain=v2 -z --untracked-files=all`,
  whose untracked entries are the files counted, so no `ls-files` call is
  needed.
- **conflict:** `git merge-tree --write-tree <base> HEAD` exits 1. It
  needs git 2.38; with an older git, found once from `git merge-tree -h`,
  the field is left out and merge-tree is not run again.
- **rebasing:** a `rebase-merge` or `rebase-apply` directory in the
  worktree's git dir.
- **base:** the first that exists of `branch.<b>.laatmux-base` in the
  repository's config; `origin/HEAD`; `main`; `master`. `add` writes the
  key when it makes a branch, with the resolved name, `origin/main` and
  not `origin/HEAD`, so a later change of the default branch does not
  move it: for a new branch from `origin/HEAD`, the branch that points
  to; for a new branch tracking `origin/<b>`, the same, since that is
  where the remote branch is meant to land. It writes nothing for an
  existing local branch, nor over a key already there. On the base
  branch itself only the uncommitted stats are shown.

Every call runs as `git --no-optional-locks`, and the diff against
`HEAD` with `-c diff.autoRefreshIndex=false`, since a porcelain `git
diff` refreshes and rewrites the index under its lock whatever the
option: the poll never takes the index lock a user's git needs. The two
`git diff` calls also take `--no-ext-diff` and `--no-textconv`, which
are diff options, so they never run a user's diff driver. `merge-tree
--write-tree --quiet` stops at the first conflict and writes no objects;
it runs the repository's merge drivers, as any merge does, and a git
before 2.50, without `--quiet`, gets the plain call, which writes the
merge's objects once per commit pair. Every call runs with
`GIT_NO_LAZY_FETCH=1`, which git reads from 2.45, and, for an older
git, `GIT_ALLOW_PROTOCOL=none`, so a partial clone never fetches from
its remote during a refresh. What needs a missing blob is left out: the
committed diff or the conflict, read again after a minute, or the
uncommitted diff, then a lower bound. A branch with no
merge base with its base, an orphan or a shallow history, gets ahead
and behind alone.

**Where it runs.** Not in the worktree listing: that poll is serialized
and stamps the listings that retire tasks, and a slow repository must
not hold it. A separate worker pool, two at a time, takes the worktrees
due for a refresh. Each git call has a 10 s timeout; one that times out
leaves the last object and sets `stale: true` in it. A result is dropped
when the worktree is gone from the listing, has another branch, or its
`HEAD` has moved, since the refresh began.

**What is cached.** `committed`, `ahead`, `behind` and `conflict`
depend only on the base's and `HEAD`'s commits and the shallow
boundary, so they are computed again only when one changes; a fetch
that moves the base, or one that deepens a shallow history, is such a
change. `merge-tree --write-tree` does a real merge and writes objects,
so it runs once per pair and never on the base branch. `dirty` and
`uncommitted` are read on every refresh; the line counts of untracked
files are kept by path, size and mtime, so a refresh reads only the
files that changed.

**Refresh.** A worktree with a session is due every 5 s, one without
every 30 s. `add`, `rm` and a run ending make it due at once, and so does
a change in an mtime the daemon stats every second: `HEAD` and `index`
in the worktree's git dir (`.git/worktrees/<name>`), and in the common
dir `packed-refs`, `shallow` and the loose refs of the branch and of its
base. A worktree gets at most one refresh every 2 s. Polling comes
first; kqueue or inotify through `golang.org/x/sys` only if a measurement with
twenty worktrees on the VM shows the cost.

Step 4 measured it: twenty worktrees of a repository with 2000 files, each
with a commit, a changed file and an untracked one, on the VM (8 vCPU,
Xeon 2.2 GHz, git 2.47). A refresh is four git calls, about 37 ms. The
daemon with its git calls used 2.6% of a core in a minute before step 4,
5.6% with the twenty worktrees on the 30 s cadence, and 12.3% with all
twenty on the 5 s cadence of a worktree with a session. That is about
half a percent of a core per worktree with a session, which is not
worth an event watcher; polling stays. The base's name is kept for a
minute, so a refresh resolves it again only then.

**In the record.** The git object is part of the worktree record. The
listing rebuilds each record from git's listing and compares four
fields to decide an upsert; the git object is carried across that
rebuild and compared with them, so a listing neither drops it nor
upserts for nothing. A refresh upserts only when a value changed.
`git-status` needs the merging daemon to know the field too: a view
shows stats when the host's capabilities have `git-status` and the
merging daemon's hello has it, as for `attribution`.

**Rendering.** In order on the line: the rebase mark `R`, dim `+N -M` in
green and red, then `✎` and bright `+X -Y`. When the uncommitted counts
are the whole diff, the committed part is dropped. On a narrow line the
committed part goes first, then everything but the rebase mark.

## PR and checks, on the laptop

A pushed branch is the same on every host, and `gh` is logged in on the
laptop, so the laptop's merging daemon fetches PR and check state for
every worktree in the merged stream, keyed by source and branch.

- **The hosts.** github.com, and the GitHub Enterprise hosts the config
  lists in `github_hosts`. A source on any other host is never asked
  about: gh would send it the token it keeps for that host.
- **The query.** `gh api graphql` calls per GitHub host, each for at
  most 32 branches, with the owner, repository and branch names passed
  as GraphQL variables, never put into the query text. For each branch
  it asks for the PRs on that head ref, the open ones apart, so newer
  closed ones do not hide one, and the five newest of any state, and
  keeps those that are not cross-repository, since a fork's PR can have
  a head branch of the same name. An open one is taken first, else the newest merged or
  closed, while the branch is where that PR left it or is gone; a
  branch that moved on, `main` after an old PR from it say, has no PR.
  The PR's last commit gives the oid and `statusCheckRollup`; a branch
  with no PR gets its own ref's. When `origin` is itself a fork, the PR
  lives on the upstream repository and is not found: the row shows the
  branch's own checks and no PR. That is a limit of this milestone. The
  rollup's contexts are paginated, so the counts come from the
  connection's aggregates, `checkRunCountsByState` and
  `statusContextCountsByState`, which count every context whatever the
  page. The name of the first failing check is a second, small query,
  only for rollups that fail, paging until one is found, and kept by
  the rollup's id and counts for five minutes, since a rerun on the same commit can
  move the failure to another check; the key is the rollup and every
  count by state. When forks' PRs of the same name fill a connection's
  page, the next pages are asked for, up to five, with the number and
  the fork mark alone, and the repository's own PR, once found, in full:
  the open ones while the branch is there, any when no own one is in
  sight, since a closed one counts once the branch is gone or while it
  is at that PR's last commit; not on the repository's default branch,
  `main` or `master`, whose forks' PRs are many and whose own PR, on
  `main` or `master`, the views never show; and, once the pages held none of the repository's own,
  not again for an hour, whether the branch is on GitHub or not, since
  on a crowded name, `patch-1` say, they never change; a PR found ends
  that.
  A query of 32 branches costs about seven of GitHub's rate-limit
  points; a light page or a PR by number one each.
- **When.** Every 30 s while a merged subscriber is there, and at once
  when the set of branches changes. A round runs beside the loop that
  ages the answers, bounded to two minutes: every host's status first,
  each host within its share of what is left, then the failing checks'
  names, so a slow host starves no other.
- **The cache.** The last answer per branch is kept under the state
  directory with the laptop's wall-clock time it was fetched, so a
  restarted sidebar has it at once, stale when it is old, and a laptop
  that slept ages its answers by the sleep. An answer older than five
  minutes is stale: the view draws it dim with `?` after the checks. A
  failed query, whole, for one chunk, or with an error under one
  branch, keeps the last answers of the branches it covered and marks
  them stale; a branch the answer says has no ref, and no PR of its
  own, drops its entry, while one deleted after its PR merged keeps the
  PR, with that PR's last commit's checks. An entry whose branch has had
  no worktree in the merged stream for a day is dropped, once every host
  has listed successfully since the daemon started.
- **Non-GitHub sources and a missing or logged-out `gh`** show nothing.
  The reason is the daemon's own, not a host's: it travels in the merged
  stream as `github_error` in the snapshot and in an upsert, as
  `sessions_error` does, and `laatmux hosts` prints it on a `github:`
  line after the hosts, never in a host's connectivity. A trusted
  enterprise host that is logged out is named in it too, each host's
  failure kept until that host answers or no worktree is on it; with no error
  from the daemon, `hosts` asks github.com itself with the smallest
  query, through the same code, so it tells the same failures apart.
- **The record,** from a merging daemon with the capability `branches`.
  `branch` is a string in the envelope already (the branch of an add or
  rm), so the record has a key of its own, `branch_status`:

  ```json
  "branch_status": {"source": "…", "branch": "…", "fetched_at": "…", "stale": false,
                    "head_oid": "…", "checks_url": "…",
                    "pr": {"number": 52, "state": "open", "draft": false, "url": "…"},
                    "checks": {"state": "pending", "passed": 3, "total": 5,
                               "failing": "", "pending_since": "…"}}
  ```

  `checks_url` is the PR's checks page, or the commit's for a branch
  with no PR. `pending_since` is the laptop time the checks of this
  `head_oid` were first seen pending, which the dashboard shows as the
  elapsed time. In the stream: `branch_statuses` in a snapshot,
  `branch_status` in an upsert, `branch_status_key` in a remove, an
  object `{source, branch}` whose source is the source key, normalised
  as #53 does, the same as in `repo/<source key>`. `github_error` is
  cleared by an upsert with `github_ok: true`, as `sessions_listed`
  clears `sessions_error`.

- **Aggregation.** Failure is FAILURE, CANCELLED, TIMED_OUT,
  STARTUP_FAILURE, ACTION_REQUIRED or ERROR, and beats pending. Pending
  is IN_PROGRESS, QUEUED, PENDING, REQUESTED, WAITING or EXPECTED.
  NEUTRAL, SKIPPED and STALE are left out of `passed` and `total`
  alike, and a rollup of only those is success with `total` 0.
- **Rendering.** `#N` is green when open, purple when merged, red when
  closed, dim when a draft. Checks are `✓` in green, `× 3/5` in red, or a
  spinner and `3/5` in purple; when narrow the counts go first. On
  `main` or `master` the PR is not shown, and the checks only when they
  fail.

## The agent tile

```
▌ ⠋⠙ agents-config                 1:50
▌    laatmux @vm  +46 -11 ✎ +28 -3
▌    Adding per-agent config…    #52 ✓
──────────────────────────────────────
▌ 💬 batch-processing              6:10
▌    anki-llm @mac
▌    Refactoring queue handl…
──────────────────────────────────────
▌ 💤 history-filter                  1h      (whole tile dim)
▌    consult-llm-mcp @vm +153 -41
▌    Adding date range filter
```

- **Line 1:** the status icon, the primary label, and the time since the
  status last changed: `m:ss` under an hour, then `Nh`, then `Nd`.
- **Line 2:** the secondary label, then the worktree's diff stats
  against the right edge.
- **Line 3:** the pane title, then the PR number and checks against the
  right edge.
- **The stripe:** `▌` on every line, in the status colour.
- **The selection:** a background band when the background is known,
  reverse video otherwise (below).
- **Dividers:** `─` between tiles, in `border`. The compact layout is
  line 1 with the secondary label and host tag after the primary, and
  the dashboard draws line 3 under it.

### Colour and theme

A `Span` has an SGR code or dim today. It gets a foreground, a palette
name or a colour as the config writes one, and bold; the background is
the line's, the selection band, in 256 colours and 24-bit. The
palette is #52's:
`info`, `accent`, `success`, `warning`, `danger`, `dimmed`, `text`,
`border`, `header`, `highlight_row_bg` and `current_worktree_fg`, with a
dark and a light default. `theme.mode: auto` picks one from the
terminal's background, asked with OSC 11 when the view starts, then
from `COLORFGBG`; with neither, a tmux popup say, the dark defaults
colour the accents while plain text keeps the terminal's foreground
and the selection is reverse video with no colours on it, unless
`theme.custom` sets both `highlight_row_bg` and `text`;
`theme.custom` overrides any colour. A stale row is drawn in `dimmed`
with the dim attribute throughout, the stripe and the diff and PR
colours included, and so is any other dim row but for the viewer's own
row's label. A selected dim row is not faint: under the band its plain
text is in `text` and its coloured spans keep their colours; under
reverse video it has no colours. A task that needs the user is not
dim, its waiting icon saying so. Plain text keeps the terminal's
foreground, in `text` under the selection band; a host that is down is
drawn in `danger`, one connecting in `warning`, and a group's header in
`header`. The row for
the viewer's own session has its primary label in bold
`current_worktree_fg`, which replaces the `>` gutter. `NO_COLOR`
falls back to today's attributes, and the golden tests' `Debug` form
names colours by palette name.

### Labels and the pane title

The branch is the primary label and the repository the secondary, with
the host tag after it. A detached worktree is named by its root's last
element, with the repository and `detached` as the secondary; a `new`
session and an observed agent fall back to the session name. `main`
and `master` are never primary when there is a better name: on them
the repository is primary and the branch secondary. The pane title is cleaned
before it is shown: leading braille, the half-circle spinner `◐ ◑ ◒ ◓`
and `✳ ● ○ ◌ ✓ ✗` characters are stripped, and so is an `OC |` prefix;
a title that starts with `Claude Code`, is a shell's name, repeats the
primary or secondary label, or is the host's name or this machine's
name, which tmux titles a pane with until its program sets one, is
dropped. A remote machine's own name is not known to the laptop, so a
remote pane titled with it keeps the title.

## Sorting and folding

The tree's structure replaces workmux's `group_by`: grouping by project
is the tree, and the host is on every worktree line.

- **`sort`,** in the agent view:
  - `priority`, today's order with the new statuses: blocked, done,
    working, idle and unknown, stale, settled;
  - `recency`, by the time since the status last changed;
  - `window`, by session, then window.
- **Folds:** in the agent view, stale rows fold into `▸ N stale`, `▾`
  when open; `collapse_stale: false` keeps stale rows out of it, in
  the list, while settled ones still fold. In the tree,
  repositories and worktrees fold, as described there.
- **Pinned header:** in the tree, the repository line of the row at the
  top stays pinned while the list scrolls.
- **More below:** a `↓ N more` line counts rows scrolled out of sight.

## Keys

Settled here, for both views and for the sidebar and the dashboard
alike unless the row says otherwise.

| key | today | now |
|---|---|---|
| `j` `k`, arrows | move | move |
| `g` `G` | first, last | first, last |
| `1`..`9` | jump to the nth row of the selection's group | jump to the nth tile of the agent view, the nth worktree line of the tree |
| `Enter`, click | jump | jump; on a repository line or a fold row, fold; a click on a fold mark folds |
| `/` | filter | filter |
| `Esc` | clear the filter | clear the filter; nothing else |
| `v` | tiles or compact | tiles or compact |
| `f` | show the settled group and today's stale (orphaned) group | open every fold when any is closed, else close every one |
| `Tab` | — | switch view |
| `h`, `Left` | — | fold; on a child, go to its worktree or task line |
| `l`, `Right` | — | unfold |
| `s` | settle (dashboard) | toggle the fold at the selection |
| `z` | — | settle or unsettle, sidebar and dashboard |
| `S` | shell (dashboard) | shell (dashboard), unchanged |
| `F` | — | scope to the viewer's session, and back |
| `?` | — | help overlay listing the keys |
| `o` `O` | — | open the PR, its checks (dashboard) |
| `q`, `Ctrl-c` | quit | quit the dashboard; in the sidebar, ask "Quit sidebar? y/n"; while filtering `q` is a letter and `Ctrl-c` asks |
| `a` `x` `X` `p` | dashboard actions | unchanged, on the rows below |

Fold rows and repository lines are selectable, since `Enter` acts on
them, and `j` and `k` stop on them; the numbers skip them.

**Actions on the new rows.**
- `x` and `X` on an agent tile or an agent in the tree remove the
  agent's worktree, as on its worktree line, and the question names the
  worktree and says how many agents go with it. On an agent in no
  worktree they do nothing, as on an observed agent today.
- On a pane or run line, `x` and `X` say what they remove, as on a
  repository line or the stale fold; stopping a run from the view is
  not in this milestone.
- `a` preselects the repository and host of the selected row's
  worktree, from a tile or any tree line under a worktree, as from a
  worktree row today, and a repository line's repository when this
  machine knows its source, by a host's label alone for an older host.
- `x` on an orphaned session's line sends `rm` by the root from the
  session's key, with the repository and branch when its tags have
  them, as on a stale row today.

Five settlements differ from the plan in #52:

- **`S` stays the shell.** workmux uses `S` for toggling every fold; in
  laatmux `S` has opened a shell since milestone three, and `f` already
  toggles what becomes the folds.
- **`Esc` does not ask to quit.** In a sidebar pane an Esc meant for
  another pane is common; it clears a filter and otherwise does nothing.
  `q` and `Ctrl-c` ask.
- **Working worktrees start open in the tree.** A worktree with a
  working agent starts unfolded, so its spinner is in sight; #52 folded
  whatever did not need the user.
- **View, layout and scope are start defaults.** #52 shared them live
  between every pane; here a change in one pane stays in it, the view
  and layout last chosen and the scope last set by the CLI are what a
  new pane starts with, and `--all` changes every running pane.
- **`M-1`..`M-9` are opt-in.** Bound in tmux's root table they take the
  keys from every pane, where shells and editors use them.
  `sidebar.jump_keys: true` has `on` bind them to `run-shell "laatmux
  sidebar jump N -t '#{window_id}' -c '#{client_name}'"`, so the window
  and the client are the key's own with any number of clients
  attached, and the `{jump_key}` token then shows them in the sidebar
  panes, where the keys land, not in the dashboard; by default they
  are unbound and the token is empty.

## Placement, scope and controls

- **Position:** `sidebar.position: left | top`. `top` is a strip of chips
  `item_width` wide, 24 by default, separated by ` │ `, showing as many
  template lines as its height allows. This reverses milestone three's
  "no top-edge sidebar". The strip shows the agent view only; `Tab`
  there does nothing.
- **Size:** `sidebar.width` takes columns or `N%`; the default becomes
  10% of the window, clamped to 25..50 columns, and an explicit width
  is not clamped. `sidebar.height` applies to `top`. `sidebar fit` keeps
  its job of restoring the width after a resize.
- **Scope:** `laatmux sidebar on --session` puts sidebar panes in the
  current session's windows only, so other sessions get none. This
  reverses the rule against a per-session scope, and is #52's meaning.
  The hooks stay global, since a hook set on the session would shadow
  the user's global hooks of that name there: `on --session` names the
  session in the server option `@laatmux_sidebar_sessions`, the hooks
  pass the window's session, and `attach` adds no pane to a window in
  a session not named; a plain `on` clears the option, `off` unsets
  it. The current session is the default server's: from a shell nested
  on the laatmux server `on --session` refuses, since the default
  server would take the other server's pane id for one of its own, or
  fall back to its latest session, and kill the panes elsewhere.
- **`F`** switches the pane's scope to `session`, and pressed again back
  to the scope the pane had before, whatever set it; a pane already on
  `session` goes to `all`. It acts on that pane only and is not
  persisted. `F` and the scope are one setting.
- **CLI:** `laatmux sidebar next | prev | jump N | view agents|tree |
  scope all|session|project` act on one sidebar pane: the one in the
  window the command runs for, `-t` a window or the current one on the
  default server (from a shell nested on the laatmux server there is
  none, and the command does nothing), or on every pane with `--all`,
  which only `view` and `scope` take. Each
  sidebar pane listens on a unix socket of its own under the state
  directory, named by the tmux server's pid and the pane id, since pane
  ids restart after a server restart; it writes the path to the pane
  option `@laatmux_sidebar_socket`, and the CLI reads the path from the
  tagged pane rather than building it. A pane unlinks a leftover socket
  of its name before it listens and removes its socket on exit, and
  `sidebar reap` removes a socket whose server pid is the default
  server's and whose pane is gone, or one that refuses a connection; a
  socket of another server, which it cannot list, goes only when it
  refuses. The command is
  one message to the socket, handled as a navigation event, not as
  typed keys: it moves the selection or switches the view whether the
  pane is filtering or not, and is ignored while an overlay or a
  question is open. `jump N` is the digit key N: it counts the tiles,
  or the worktree lines in the tree, in the list as drawn, after the
  scope, the filter and the folds, skipping fold rows and repository
  lines, and switches the client the
  command names with `-c`, or the one it ran from when that is the
  default server's, with `switch-client -c`; a shell nested on the
  laatmux server names none, and the pane switches a client of its own. For `view` and `scope` the CLI writes the new default to
  `sidebar.json` once, whether or not a pane answered; the panes only
  apply the change. Nothing is replayed. A window with no sidebar pane,
  or a socket that refuses the connection, makes the command exit
  quietly, since a binding's error flashes in the status line.
  The scope is what a pane shows, defined by the viewer's row, the one
  Following picks:
  - `all`: every row;
  - `session`: the viewer's worktree, in the agent view every agent of
    it, whatever session each runs in, and every task at its root, and
    in the tree its worktree line, or the task line standing for it,
    with its children, and any other task at its root as a line of its
    own beside it, under its repository line; with no worktree, the
    viewer's line as Following finds it;
  - `project`: every line under the viewer's worktree's repository line,
    its worktrees, its pending tasks and its orphaned sessions, and in
    the agent view that repository's agents and tasks; with no worktree,
    the same as `session`.

  A task at the viewer's worktree's root is the viewer's under
  `session`: an add of an existing branch lands on the worktree the user
  may be in, and one whose prompt was not delivered because the session
  existed needs them there.

  Every row that is the viewer's, as Following marks it, is in the
  scope under `session` and `project` whatever its worktree. A managed
  agent of no worktree in the home session of the viewer's line, `cd ~
  && claude` in a split, and an agent observed in a window of the
  viewer's workspace session stand in other sessions with no worktree,
  yet they are the viewer's: `z`, `S` and enter on them act on the
  session the viewer is in. The tree shows such a row under the
  other-sessions header, after the viewer's line. With a task standing
  for the line, the managed one is the task's, as `z` refuses it, and
  not the viewer's. Another worktree's line is the viewer's when one of
  its agents is observed in a window of the viewer's session with its
  directory in that worktree, or runs in the managed session a plain
  attachment the viewer is in shows: the scope keeps that line with its
  children, and in the agent view its agents, beside the viewer's
  worktree.

  A pane in a session that is no row's, the user's own shell session
  say, shows the view's empty state under `session` and `project`. The
  dashboard's `F` uses the same rules through the client the popup
  opened on, and starts at `all` whatever the file says, since a scope
  the CLI set for the panes would empty a popup opened from an
  unrelated shell. #52 called it `filter none|all|…`; `none` was the same as
  `all`, and the word `filter` is the view's `/` text filter, which
  stays the pane's own and is not persisted, as is `F`.
- **Persistence:** `sidebar.json` under the state directory holds two
  kinds of thing. The view, layout and scope are *defaults*: the view
  and layout last chosen by a key or the CLI, and the scope last set by
  the CLI, are written there, and a pane reads them only when it
  starts; `F` is not written. A change in one pane never moves another; the CLI
  with `--all` is how to change every pane. The folds the user toggled
  are *shared*: every pane reads them again when the file's mtime
  changes, checked every second. Panes write the file read-modify-write
  under a lock file and replace it by rename, so two panes toggling
  folds at once lose neither change; the CLI and the dashboard, which
  write it too, do the same. The dashboard shares the folds with the
  sidebar panes. At a handoff, a pane that finds a toggled fold under
  the task's id and none under the node taking its children writes it
  under that node's id, under the same lock; every running pane writes
  the same value, and with none running the fold is lost, which is
  accepted. The selection is each
  pane's own and is not kept. A fold is kept by node id with the time
  its node was last seen, and one not seen for a day is dropped. At
  start a pane takes the file's values and the config's for what the
  file lacks; the config is the default, the file the last choice.
  The view tells its host of a setting changed, the view, layout,
  scope or a fold, once after the key or command that changed it; the
  host writes the view after `Tab`, the layout after `v` and the
  folds set here then, and the file's folds reach the view as a
  command on its own goroutine when the poll sees the mtime move; a
  value the file held the last time is not applied again, so a fold a
  pane opened to reveal a selection stays open; a fold carried at a
  handoff is written only where the file has none, and the value
  carried is the user's, the file's as last seen, not a reveal over
  it; a fold the file dropped is forgotten by the panes too, unless
  set there since; each pane refreshes its folds' sightings once an
  hour. Under a scope, a folded repository line is a closed fold
  shown: `f` opens it as a reveal, not written, and the lines under it
  with it. The dashboard's keys are `dashboard_view` and
  `dashboard_layout`.
- **Other states:** both views show `⠋ Loading` before the first
  snapshot; the empty states are each view's own.

## Templates

Every line of every layout is a template, so the tiles above are only
the defaults.

- **Tokens:**
  - labels: `{primary}`, `{secondary}`, `{branch}`, `{repo}`, `{host}`
    (the name alone, dim off this machine, `?` when no host claims the
    record, `/server` after it on a tile or an agent line for an agent
    observed off the managed server; the templates write the `@` and
    the parentheses), `{session}`, `{window}` (tmux's
    `session:index`, since the records carry no window name),
    `{window_index}`, `{pane_title}` (the cleaned title; on a tile,
    what the row is instead), `{pane_suffix}`;
  - status: `{stripe}` (the bar in the status colour, on a tile),
    `{status_icon}`, `{status_label}` (the agent's status as a word, a
    task's state, `worktree gone` on an orphaned line; "" on a
    worktree line, whose folded icon says it), `{agent_icon}`,
    `{agent_label}`, `{elapsed}` (a run's running time on its line);
  - git: `{git_stats}`, `{git_committed}`, `{git_uncommitted}`,
    `{git_ahead}` (`↑N`), `{git_behind}` (`↓N`), `{git_dirty}`,
    `{git_conflict}`, `{git_rebase}`, `{git_branch}` (the base),
    `{git_sync}` (step 9: `→base` off `main`, `master` and the branch
    itself, `origin/` taken off, at most twelve cells; the conflict
    mark; `↑A ↓B`; shrinks the base first, cut then gone, then `↓B`,
    then `↑A`, never to nothing);
  - PR: `{pr_number}`, `{pr_checks}`, `{pr_state}` (step 9: the state
    icon), `{pr_detail}` (step 9: the pending time or the failing
    check's name);
  - position: `{idx}`, `{jump_key}`;
  - tree lines: `{indent}` on every line; `{repo_count}` on a
    repository line; `{fold}` (`▾ `, `▸ `, or two spaces; on a
    repository line `▸ ` or nothing), `{worst_status}` and
    `{child_count}` on a worktree line; `{command}` on a pane or run
    line. A token with nothing on a row is empty.
- **`{fill}`** splits a line into a left and a right part; the right part
  sits against the right edge.
- **Overflow:** the flexible tokens, the labels and the title on
  either side, are cut with `…` down to a floor of a third of the
  width, at most twelve cells, the rightmost first (so the compact
  line's secondary label goes before its primary); `{git_stats}`,
  `{git_sync}` and `{pr_checks}` shrink themselves, the rightmost
  first, never to nothing (the stats keep
  their smallest form, the rebase mark or one part, until the field is
  dropped); then fields on the right are dropped, the widest first and
  a folded worktree line's `{worst_status}` last, so a blocked or done
  agent inside is not missed; then the flexible tokens are cut
  further; then the tokens on the left are dropped, the last first,
  whole; then the line is clipped. A dropped token takes its
  separator, the literal before it (else the one after) up to a
  bracket, which is a neighbour's, and a bracket pair around it alone,
  `({host})`, goes with it. What dropping leaves over goes
  back to the cut labels, then to the shrunk stats, sync and checks. A stale
  branch's `?` sits on `{pr_checks}` when they are drawn, else on
  `{pr_number}`, counted in the fitting: a number drawn on a stale row
  always has it; of a one-digit number and the marked check, equally
  wide, the number goes first, so the checks keep the mark.
- **Empty tokens:** an empty token takes the adjacent run of spaces
  with it, the one after it, else the one before, so `{a}  {b}  {c}`
  with `{b}` empty is `{a}  {c}`; an empty field keeps its line, so
  tiles keep their height; an empty entry in `tiles` removes that
  line, while the compact row and a tree node always keep a line, so
  they can be selected, and their empty template is the default.
- **Styles:** `#[fg=…,bg=…,bold,dim]`, `nobold`, `nodim` and `default`
  in tmux's syntax, with palette names or colours; a style holds until
  the next, through the fill's padding, and leaves a token's own
  colours alone; a background gives way to the selection's band.
- **Config:** `sidebar.templates.{compact, tiles, top, tree.{repo,
  worktree, agent, pane, run}}`; `top`, like `tiles`, is a list of
  lines, three by default. An unknown token is shown in the view
  as `template error: unknown token … at column N in tiles[0]` instead
  of failing the pane. With `Titles` the compact layout draws the
  tiles' third line under each row. The stale fold row, the `other
  sessions` header and the session lines under it are fixed, not
  templates.

## The dashboard

The dashboard renders with the same code, so it gets all of the above,
the two views included. On top of that:

- a git column with `→base` when the base is not `main`, `master` or
  the branch itself, the conflict mark, `↑A` and `↓B`;
- the PR state icon, the elapsed time of pending checks, and the name of
  the first failing check;
- `o` opens the selected row's PR in the laptop's browser, `O` its
  checks.

The columns are tokens, `{git_sync}`, `{pr_state}` and `{pr_detail}`,
so a sidebar wide enough can have them too. What differs is the
defaults: the dashboard's second tile line and its compact line put
`{git_sync}` after `{git_stats}`, where the engine shrinks it first,
its third tile line reads `{pr_state} {pr_number} {pr_checks}
{pr_detail}`, and its worktree line has both before `{worst_status}`;
the sidebar's defaults stay as step 7 left them, since at 25 to 50
columns the engine would only drop the new fields again. A line the
config sets applies to both hosts. A base is a branch name, which can
be long and says less than the stats and checks beside it, so
`→base` is at most twelve cells from the start and is left out when
it names the row's own branch; `{git_sync}` shrinks before it is
dropped, the base cut then gone, then `↓B`, then `↑A`, so the
conflict mark is the last to go. `{pr_detail}`'s failing name is a
label, cut like the title; its pending time is shown whole or
dropped. A draft closed as a draft is closed, in `{pr_state}` and in
`{pr_number}`'s colour. The pending time is the laptop's
`pending_since` against the laptop's clock, so it never runs
backwards across hosts; a stale answer leaves the columns dim and
plain, the pending time then as of the last answer, standing still as
the spinner does, and the `?` stays on the number and the checks.

## Protocol

All additive, behind capabilities; the protocol version stays 1.
Subscribers do not choose which records they get, so every new record
travels under an envelope key no older client knows, and an older
client passes over the message. None reuses an existing key with
another type: `branch` is a string in the envelope, which is why the PR
record is `branch_status`.

- **`git-status`,** from a host daemon, and from the merging daemon that
  forwards it: the `git` object on worktree records. A client without
  it shows no stats.
- **`select`,** from a host daemon: the `select` command, `{type:
  select, id, pane_id}`, which selects the pane's window and the pane on
  the managed server. A tree jump to a pane without it stops at the
  session.
- **`attention`,** from a merging daemon: `attention` records in the
  merged stream, `{agent_id, finished_at, seen_at}`, in the snapshot as
  `attentions` and in upserts and removes as `attention` and
  `attention_id`. Without it no row is done.
- **`branches`,** from a merging daemon: `branch_status` records, keyed
  by source key and branch, as `branch_statuses`, `branch_status` and
  `branch_status_key`, and `github_error` beside them. Without it no PR
  state is shown.
- **`poke`,** `{type: poke}` to a merging daemon with `attention`: list
  the local clients now. Answered with nothing.
- **Pane records** of laatmux's own panes stop, in step 6's host change:
  no capability, since a record that is not there needs none.

Each step that adds a record adds a test that decodes its messages with
the envelope as it was before the step, as an older client would.

The pane and run records of #55 are already in the stream; the tree is
the first view that shows them.

## Config, collected

```yaml
icons: emoji              # emoji | nerdfont | ascii
status_icons: {working: "", waiting: "💬", done: "✅", stale: "💤"}   # "" keeps the default
agent_icons: {claude: {icon: CC, color: "#d97757"}}
theme: {mode: auto, custom: {accent: "#b48ead"}}
sidebar:
  view: agents            # agents | tree
  position: left          # left | top
  # width: 40             # columns or N%; unset: 10%, clamped to 25..50
  height: 3               # top only
  layout: tiles           # tiles | compact
  horizontal: {item_width: 24}
  sort: priority          # priority | recency | window
  dim_stale: true
  collapse_stale: true
  stale_after: 1h
  scope: all              # all | session | project
  jump_keys: false        # M-1..M-9 in tmux's root table
  templates: {tiles: ["…", "…", "…"]}
```

## Order of work

Each step is one issue and one PR, reviewed as the others were.

1. **This note**, with the key assignments settled and the interrupted
   check done, the capture kept as a detection fixture.
2. **Colour and chrome, client only.** The span model, palette and
   theme, the stripe, the selection band, the dividers, the icon sets,
   the agent icons, `m:ss`, the spinner, the labels and title cleaning,
   `↓ N more`, Loading and the empty states, and the rename of today's
   stale rows to orphaned. No protocol change.
3. **Done, seen and stale.** The `attention` capability and record,
   `poke`, the three poke hooks and `sidebar seen`, `attention.json`, ✅, the
   precedence, stale by age with `dim_stale` and `stale_after`, `z` for
   settle with 💤, and the sort modes. Stale rows fold with the settled
   ones in today's collapsed group, with `collapse_stale`, until step 6
   turns that into the agent view's fold. `s` does nothing from here
   until step 6 gives it the folds. A working capture of Claude Code
   2.1.284 and a `nextActivity` test for the interrupted fixture.
4. **Git stats on the host daemon.** `git-status`, the `git` object,
   the worker pool and its cache, `branch.<b>.laatmux-base` set by `add`,
   the refresh policy and the rendering, measured on the VM with twenty
   worktrees. Both daemons need the build: the host for the field, the
   laptop to forward it.
5. **PR and checks in the laptop's daemon.** `branches`, the batched
   GraphQL calls through `gh`, the failing name query, the cache and its
   stale mark, `github_error` and the `hosts` line, the rendering, and
   the dashboard's `o` and `O`.
6. **Two views.** `rows` split into the agent and tree builders; the
   tree with repository nodes by source, worktree lines with the host in
   parentheses, children from the pane and run records the client now
   keeps, tasks, other sessions, folds and the pinned repository line;
   `Tab`, the tab header, selection across a switch and following, the
   stale fold in the agent view; the keys `f`, `s`, `h`, `l`, the
   numbers, and the actions on the new rows; jumps to a pane with the
   host's `select` command; orphaned sessions in the tree; laatmux's own
   panes left out of the pane records; `ls` printing the tree; both
   views in the dashboard. Hosts need the build for `select` and for
   leaving laatmux's own panes out; an older host still shows `$ tmux`
   and `$ laatmux` children under a worktree whose sessions are local,
   until it is upgraded.
7. **Templates.** The tokens, `{fill}`, overflow, styles, template
   errors, and the tree templates.
8. **Placement and controls.** `top`, `%` widths, `--session` with its
   session hooks, `F`, the `sidebar` subcommands over each pane's
   socket, `scope`, `jump_keys`, `?`, the quit question, and
   `sidebar.json` with its defaults, its shared folds, its lock and the
   folds' last-seen times.
9. **Dashboard columns.** The git and PR columns and the failing check:
   the `{git_sync}`, `{pr_state}` and `{pr_detail}` tokens, and the
   dashboard's own default templates.

Steps 2, 3 and 4 are independent of each other after this note, but for
step 2's rename, which step 3 needs first. Step 6 comes after step 3,
which moves settle from `s` to `z` before the tree takes `s` for folds,
so no build is without a settle key, and which gives the agent view's
stale fold something to hold. Step 7 goes after 2 through 6,
so its tokens have something to show; step 8's persistence takes the
view and folds step 6 keeps in memory.

## Tests

- **Rendering:** golden tests for every layout and both views, with
  colours, stale, done, a narrow width with fields dropped in order, a
  repository with worktrees on two hosts, a worktree with two agents, a
  shell and a run, other sessions, and folds.
- **Switching:** selection across `Tab` from an agent, a worktree, a
  pane, a repository line, a task, a task handing over, a task handing
  over while another stands for its worktree, a target in the
  stale fold or a folded worktree, and an empty worktree; following
  with two agents in the viewer's worktree.
- **Tree contents:** laatmux's own panes left out, an older host's
  agents placed by session, an orphaned session under its repository, a
  new worktree whose task needs the user holding the worktree's
  children, two tasks for one worktree with each child once, the add's
  agent under its task line before the listing, a fold set on a task
  line kept across its handoff.
- **Git:** temporary repositories for base resolution, committed and
  uncommitted counts with untracked and binary files, conflict, rebase,
  the base branch itself, no upsert when a refresh changes nothing, a
  slow repository not holding the listing, a result dropped after the
  worktree's `HEAD` moved, and the cache by commit pair.
- **GitHub:** a fake `gh` for chunking, the aggregation from the count
  connections with a failure past the first page of contexts, the
  failing name query, the cache with its stale mark, a failed chunk, and
  a missing or logged-out `gh`.
- **Attention:** a working-to-idle transition with the user in the
  session, one with the user elsewhere, an interrupt followed at once by
  a switch away, a finish across a daemon restart seen in the next
  snapshot, a finish watched with no view open and a view opened after
  the user left, the listing each second only with an agent done and a
  view open or closed within the idle minute, a cached record replayed
  on reconnect not counted, a local agent's finish with no subscriber,
  a new identity in the same pane, a reused pane id after a server
  restart, an agent on a remote default server never done, a client on
  a workspace session's shell window seeing nothing, a dead attach pane
  seeing nothing after its host reconnects, an attach pane from before
  #56 with no target, an agent on vm not seen from an attach to mac's
  session of the same name, two workspace attach panes from before #56
  on one host told apart by their keys, a focused sidebar pane beside
  the attach, a host taken out of the config, a host clock hours ahead
  of the laptop's, and an agent removed.
- **Precedence:** done beats stale, blocked is never stale, a settled
  workspace's blocked agent stays in place.
- **Jumps:** a tree jump to a pane in the home session, in another
  managed session, in another window, and on this machine's default
  server, against a fake daemon with and without `select`, and into an
  existing workspace session left on a shell window.
- **Sidebar control:** a pane's socket found by the window and by
  `--all`, each command reaching the pane, `view` and `scope` written
  whether or not a pane answers; `next` while filtering and while a
  question is open; `jump N` with its client, switched by the pane's
  jump once; a leftover socket reaped and a live one kept; a window
  with no sidebar and a socket refusing; a pane starting from the
  file's view, layout and scope unless fixed, writing the view, the
  layout and the folds and never the scope, and another pane's write
  reaching it through the poll; `F` from `all`, from `session`, and
  after a `scope` from the CLI; the dashboard with and without
  `--layout`.
- **Scopes:** `session` with the viewer's worktree's agents in two
  managed sessions, with a task at the viewer's worktree's root whose
  prompt was not delivered, with a failed task at that root, the viewer
  in a task's session before the listing, a session with no worktree,
  an orphaned session under its repository, a session with no row, and
  `project` with a pending task and an orphaned session; `f` under
  `session` with the repository line open and folded.
- **Old envelope:** each new record decoded by the envelope before its
  step.
- **Templates:** a parser table with unknown tokens, styles and `{fill}`.
- **Columns:** `{git_sync}` with the base off main and on it, each
  shrink step with the base cut, a long base, the branch as its own
  base, nothing to say, the smallest form beside a field, stale with
  a conflict; `{pr_state}` per set against every state, a draft open
  and closed, stale, no PR; `{pr_detail}` failing, cut, pending under
  and over an hour whole or dropped, stale under an hour as of the
  answer, since never, on success, on `main`; the dashboard's
  defaults against the sidebar's, a configured line in both, the
  host's options picking the set, and the dashboard's compact and
  worktree lines rendered at widths going down.
- **Detection:** `claude-interrupted` in `TestRealFixtures`, added with
  this note.

## Out of scope

- The `0: workmux` line above the list in workmux's screenshot is not
  drawn by its sidebar; it is tmux's `pane-border-status`, which stays
  the user's tmux config.
- workmux has no notifications, sounds, unread markers or review state,
  and neither does this milestone.
- Agent hooks as a status source stay out: laatmux reads the screen.
- A plain attachment left by an older jump blocking a worktree's jump is
  #57, separate from this milestone.
