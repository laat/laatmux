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
truth, detection never crosses the network, and records are forwarded
unchanged by the merging daemon. What is new is state the laptop's
daemon keeps for itself: what the user has seen, and the PR and check
state of each branch. The git state of a worktree is computed where the
worktree lives, by its host's daemon.

## Two views

The sidebar and the dashboard both have two views, with a tab header
above the list:

- **Agents**, the default: one tile per agent, the most pressing first.
  This is where the user looks to see what wants them.
- **Tree**: the repositories, their worktrees, and what runs in each.
  This is where the user looks to see what exists and where it runs.

`Tab`, or a click on a tab header, switches between them. The view is
kept in `sidebar.json` (see Persistence), so every sidebar pane on the
machine shows the same one and a restarted pane comes back on it. The
dashboard keeps its own choice in the same file.

### The agent view

- **Rows:** every agent, one tile each, in `sort` order. The primary
  label is the worktree's branch, the secondary the repository and host;
  an agent in no worktree is labelled by its session.
- **Tasks:** the relay's pending tasks come first, as today, the newest
  first.
- **Left out:** shells, servers, runs, and worktrees without an agent.
  They are the tree's.
- **Several agents in one session** are told apart by a `(1)`, `(2)`
  suffix on the primary label, in pane order.
- **Stale:** stale and settled agents fold into `▸ N stale` at the end.
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
    scratch (vm)             💬 claude
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
  the merged stream carries, which it drops today.
- **Tasks:** a pending task sits under its repository as a worktree line
  of its own, with the task's state where the git stats would be, until
  it hands over to the worktree line at its root.
- **Other sessions** is the last group. It holds agents in no worktree,
  a session `new` made or one observed on a default server, each tagged
  with its host.
- **Order:** repositories by name, worktrees by branch. Status never
  reorders the tree, so it stays put while agents work.
- **Folds:** repositories and worktrees fold. A worktree whose agents
  are all idle or unknown, or that is stale or settled, starts folded;
  one with a blocked, working or done agent starts open. A fold the
  user toggles stays as they left it.
- **Jumps:** `Enter` on a worktree line jumps to its workspace session,
  as the row does today. On an agent or a pane it jumps to the
  workspace session and selects that pane: `select-pane` through the
  attach on the host's managed server for a managed pane, `switch-client`
  and `select-pane` on this machine's default server. A pane on a remote
  host's default server is refused as `jump` refuses it. On a
  repository line `Enter` folds and unfolds.
- **Empty state:** `No worktrees`.

With both views, the rule #56 needed for which agent a mixed worktree
row shows goes away: the tree shows every agent under its worktree, and
the agent view lists every agent.

### Switching

The selection follows the user across a switch:

- from an agent, the tree lands on that agent inside its worktree,
  unfolding the worktree and the repository;
- from a worktree line or one of its panes or runs, the agent view
  lands on the worktree's first agent in the tree's order; with none,
  on the first row;
- from a repository line, on the first agent of its first worktree;
- from a task, on the same task, which both views show.

A selection that follows the viewer's own session keeps following it in
the new view.

### The rows package

`rows.Build` makes one mixed list today. It splits into two builders
over the same input: `Agents`, a flat list of agent and task rows, and
`Tree`, a list of nodes with a depth, a kind (repository, worktree,
task, agent, pane, run, group), a fold state and an id. The view draws
either from the same model; the selection anchors on node ids, which
are the records' ids, so a rebuild and a switch keep it.

## Status

laatmux's detector already gives `working`, `blocked`, `idle` and
`unknown`. workmux's statuses map onto them:

| workmux | laatmux | icon | colour |
|---|---|---|---|
| Working | `working` | 2-cell braille spinner, 250 ms a frame | info (cyan) |
| Waiting | `blocked` | 💬 | accent (purple) |
| Done | `idle` after `working`, not seen since | ✅ | success (green) |
| (none) | `idle` and seen, `unknown`, no agent | blank | — |
| Stale | idle for more than an hour, or settled | 💤 | dimmed |

### Done and seen

An agent that went from working to idle is *done* until the user has
been in its session since that change.

- **The transition.** The laptop's daemon sees every agent upsert of
  every host in the merged stream. When an agent's activity goes from
  `working` to `idle`, it records the idle record's `activity_at` as the
  agent's finish.
- **Seen.** The daemon learns which session each tmux client of this
  machine's default server is on through `list-clients`, polled with the
  local sessions it already lists, and poked by a `client-session-changed`
  hook the sidebar's `on` sets, as it sets `window-resized`. A client on
  the agent's workspace session, its plain attachment, or its own
  session on this machine's default server has seen the agent: the seen
  time moves to now.
- **The record.** A new record type in the merged stream, from a
  merging daemon with the capability `attention`:
  `{agent_id, finished_at, seen_at}`. A view shows ✅ for an idle agent
  whose `finished_at` is after its `seen_at`.
- **Kept across restarts** in `attention.json` under the state
  directory, so a restart does not bring back ✅ on everything. An entry
  goes with its agent's remove. A finish while the laptop's daemon was
  down, or before a view subscribed to the host, is not seen as one:
  the daemon only records transitions it watched, and a daemon without
  a record for an agent shows no ✅ for it.

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
`TestRealFixtures`. An interrupt is the user's own key in the agent's
session, so the agent is seen at that moment and is not marked done.
The same capture shows the title not spinning while the turn ran; the
footer rule, not the title, caught the working state, which is how the
detector is built.

### Stale and settled

A row whose agent has been idle for more than `stale_after`, an hour by
default, is stale: dim, with 💤, sorted after the live ones, and folded
in the agent view. This reverses milestone three's "age is never a
reason" rule; `dim_stale: false` restores it.

workmux's sleep is laatmux's settle, and moves to workmux's key: `z`
settles or unsettles the selected workspace, which then shows 💤 and
folds with the stale rows.

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

## Diff stats, on the host

The worktree lives on its host, so git is read there, by the daemon
that lists the worktrees. The worktree record gets a `git` object,
behind a new capability `git-status`:

```json
"git": {"base": "origin/main", "committed": [46, 11], "uncommitted": [28, 3],
        "ahead": 2, "behind": 0, "dirty": true, "conflict": false, "rebasing": false,
        "checked_at": "…"}
```

- **committed:** `git diff --numstat <base>...HEAD`, the branch against
  its merge base.
- **uncommitted:** `git diff --numstat HEAD`, staged and unstaged, plus
  the line counts of untracked files that are not ignored. A binary
  file counts 0.
- **ahead, behind, dirty:** from `git status --porcelain=v2 --branch`.
- **conflict:** `git merge-tree --write-tree <base> HEAD` exits 1. It
  needs git 2.38; with an older git the field is left out.
- **rebasing:** a `rebase-merge` or `rebase-apply` directory in the
  worktree's git dir.
- **base:** the first that exists of `branch.<b>.laatmux-base` in the
  repository's config, which `add` sets from what it branched off;
  `origin/HEAD`; `main`; `master`. On the base branch itself only the
  uncommitted stats are shown.

Every call runs with `--no-optional-locks`, `--no-ext-diff` and
`--no-textconv`, so the poll never takes the index lock a user's git
needs and never runs a user's diff driver.

**Refresh.** A worktree with a session is polled every 5 s, one without
every 30 s. `add`, `rm`, a run ending, and a change in the mtime of the
worktree's `HEAD`, index or refs trigger a poll at once; a worktree gets
at most one poll every 2 s. The git object changes the worktree record,
so the record is upserted only when a field other than `checked_at`
changed. Polling comes first; kqueue or inotify through
`golang.org/x/sys` only if a measurement with twenty worktrees on the VM
shows the cost.

**Rendering.** In order on the line: the rebase mark `R`, dim `+N -M` in
green and red, then `✎` and bright `+X -Y`. When the uncommitted counts
are the whole diff, the committed part is dropped. On a narrow line the
committed part goes first, then everything but the rebase mark.

## PR and checks, on the laptop

A pushed branch is the same on every host, and `gh` is logged in on the
laptop, so the laptop's merging daemon fetches PR and check state for
every worktree in the merged stream, keyed by source and branch.

- **The query.** One `gh api graphql` call per GitHub host, in chunks of
  at most 32 branches, for the newest PR on each head ref, open, merged
  or closed, with its last commit's `statusCheckRollup`. A branch with
  no PR gets its own ref's rollup.
- **When.** Every 30 s while a merged subscriber is there, and at once
  when the set of branches changes. The last answer is cached under the
  state directory, so a restarted sidebar has it at once.
- **Non-GitHub sources and a missing or logged-out `gh`** show nothing,
  and `laatmux hosts` says why.
- **The record,** from a merging daemon with the capability `branches`:

  ```json
  "branch": {"source": "…", "branch": "…",
             "pr": {"number": 52, "state": "open", "draft": false, "url": "…"},
             "checks": {"state": "failure", "passed": 3, "total": 5, "failing": "test (ubuntu)"}}
  ```

- **Aggregation.** Failure is FAILURE, CANCELLED, TIMED_OUT,
  STARTUP_FAILURE, ACTION_REQUIRED or ERROR, and beats pending. Pending
  is IN_PROGRESS, QUEUED, PENDING, REQUESTED or WAITING. NEUTRAL and
  SKIPPED do not count, and a rollup of only skipped checks is success.
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
- **The selection:** a background band, not reverse video.
- **Dividers:** `─` between tiles. The compact layout is line 1 alone.

### Colour and theme

A `Span` has an SGR code or dim today. It gets a foreground and a
background in 256 colours and 24-bit, and bold. A palette of named
colours (accent, info, success, warning, error, dim, border,
current_worktree_fg, selection_bg) has a dark and a light theme; `theme:
auto` picks one from the terminal's background, asked with OSC 11 when
the view starts, dark when there is no answer. Colours can be set per
name. The row for the viewer's own session has its primary label in
bold `current_worktree_fg`, which replaces the `>` gutter. `NO_COLOR`
falls back to today's attributes, and the golden tests' `Debug` form
names colours by palette name.

### Labels and the pane title

The branch is the primary label and the repository the secondary, with
the host tag after it. A detached worktree, a `new` session and an
observed agent fall back to the session name. `main` and `master` are
never primary when there is a better name. The pane title is cleaned
before it is shown: leading braille and `✳ ● ○ ◌ ✓ ✗` characters are
stripped, and so is an `OC |` prefix; a title that starts with `Claude
Code`, is a shell's name, repeats the primary or secondary label, or is
the host name is dropped.

## Sorting and folding

The tree's structure replaces workmux's `group_by`: grouping by project
is the tree, and the host is on every worktree line.

- **`sort`,** in the agent view:
  - `priority`, today's order with the new statuses: blocked, done,
    working, idle and unknown, stale, settled;
  - `recency`, by the time since the status last changed;
  - `window`, by session, then window.
- **Folds:** in the agent view, stale rows fold into `▸ N stale`, `▾`
  when open; `collapse_stale: false` keeps it open. In the tree,
  repositories and worktrees fold, as described there.
- **Pinned header:** in the tree, the repository line of the row at the
  top stays pinned while the list scrolls.
- **More below:** a `↓ N more` line counts rows scrolled out of sight.

## Keys

Settled here, for both views; the dashboard's actions are unchanged
but for settle.

| key | today | now |
|---|---|---|
| `j` `k`, arrows | move | move |
| `g` `G` | first, last | first, last |
| `1`..`9` | jump to the nth row | jump to the nth row of the view |
| `Enter`, click | jump | jump; on a repository line or a fold row, fold |
| `/` | filter | filter |
| `Esc` | clear the filter | clear the filter; nothing else |
| `v` | tiles or compact | tiles or compact |
| `f` | show the settled and stale groups | toggle every fold |
| `Tab` | — | switch view |
| `h`, `Left` | — | fold; on a child, go to its worktree |
| `l`, `Right` | — | unfold |
| `s` | settle (dashboard) | toggle the fold at the selection |
| `z` | — | settle or unsettle (dashboard) |
| `S` | shell (dashboard) | shell (dashboard), unchanged |
| `F` | — | filter to the rows of the viewer's session |
| `?` | — | help overlay listing the keys |
| `o` `O` | — | open the PR, its checks (dashboard) |
| `q`, `Ctrl-c` | quit | quit the dashboard; in the sidebar, ask "Quit sidebar? y/n" |
| `a` `x` `X` `p` | dashboard actions | unchanged |

Three settlements differ from the plan in #52:

- **`S` stays the shell.** workmux uses `S` for toggling every fold; in
  laatmux `S` has opened a shell since milestone three, and `f` already
  toggles what becomes the folds.
- **`Esc` does not ask to quit.** In a sidebar pane an Esc meant for
  another pane is common; it clears a filter and otherwise does nothing.
  `q` and `Ctrl-c` ask.
- **`M-1`..`M-9` are opt-in.** Bound in tmux's root table they take the
  keys from every pane, where shells and editors use them.
  `sidebar.jump_keys: true` has `on` bind them to `sidebar jump N`, and
  the `{jump_key}` token then shows them; by default they are unbound
  and the token is empty.

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
- **Scope:** `laatmux sidebar on --session` limits the rows to the
  current session, which reverses the rule against a per-session scope;
  `F` in the view does the same for the moment.
- **CLI:** `laatmux sidebar next | prev | jump N | filter
  none|all|session|project | view agents|tree`, so tmux bindings can
  drive every sidebar pane on the machine, through the same file the
  panes watch.
- **Persistence:** the view, layout, filter and folds persist in
  `sidebar.json` under the state directory, shared by every sidebar pane
  on the machine and read again when it changes. Folds are kept by node
  id, and ids of records gone for a day are dropped.
- **Other states:** both views show `⠋ Loading` before the first
  snapshot; the empty states are each view's own.

## Templates

Every line of every layout is a template, so the tiles above are only
the defaults.

- **Tokens:**
  - labels: `{primary}`, `{secondary}`, `{branch}`, `{repo}`, `{host}`,
    `{session}`, `{window}`, `{window_index}`, `{pane_title}`,
    `{pane_suffix}`;
  - status: `{status_icon}`, `{status_label}`, `{agent_icon}`,
    `{agent_label}`, `{elapsed}`;
  - git: `{git_stats}`, `{git_committed}`, `{git_uncommitted}`,
    `{git_ahead}` (`↑N`), `{git_behind}` (`↓N`), `{git_dirty}`,
    `{git_conflict}`, `{git_rebase}`, `{git_branch}`;
  - PR: `{pr_number}`, `{pr_checks}`;
  - position: `{idx}`, `{jump_key}`;
  - tree lines only: `{repo_count}` on a repository line; `{fold}`,
    `{worst_status}` and `{child_count}` on a worktree line; `{command}`
    and `{indent}` on a pane or run line.
- **`{fill}`** splits a line into a left and a right part; the right part
  sits against the right edge.
- **Overflow:** fields on the right are dropped last first; then the
  leftmost flexible token, a label or the title, is cut with `…`.
  `{git_stats}` and `{pr_checks}` shrink themselves before either.
- **Empty tokens:** an empty token takes one adjacent space with it; an
  empty field keeps its line, so tiles keep their height; a blank
  template removes the line.
- **Styles:** `#[fg=…,bg=…,bold]` in tmux's syntax, with palette names or
  colours.
- **Config:** `sidebar.templates.{compact, tiles, top, tree.{repo,
  worktree, agent, pane, run}}`. An unknown token is shown in the view
  as `template error: unknown token … at column N in tiles[0]` instead
  of failing the pane.

## The dashboard

The dashboard renders with the same code, so it gets all of the above,
the two views included. On top of that:

- a git column with `→base` when the base is not `main` or `master`, the
  conflict mark, `↑A` and `↓B`;
- the PR state icon, the elapsed time of pending checks, and the name of
  the first failing check;
- `o` opens the selected row's PR in the laptop's browser, `O` its
  checks.

## Protocol

All additive, behind capabilities; the protocol version stays 1.

- **`git-status`,** from a host daemon: the `git` object on worktree
  records. A client without it shows no stats.
- **`attention`,** from a merging daemon: `attention` records in the
  merged stream, `{agent_id, finished_at, seen_at}`, in the snapshot as
  `attentions` and in upserts and removes as `attention` and
  `attention_id`. Without it no row is done.
- **`branches`,** from a merging daemon: `branch` records, keyed by
  source and branch, as `branches`, `branch` and `branch_key`. Without
  it no PR state is shown.

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
  width: 10%              # columns or N%
  height: 3               # top only
  layout: tiles           # tiles | compact
  horizontal: {item_width: 24}
  sort: priority          # priority | recency | window
  dim_stale: true
  collapse_stale: true
  stale_after: 1h
  jump_keys: false        # M-1..M-9 in tmux's root table
  templates: {tiles: ["…", "…", "…"]}
```

## Order of work

Each step is one issue and one PR, reviewed as the others were.

1. **This note**, with the key assignments settled and the interrupted
   check done, the capture kept as a detection fixture.
2. **Colour and chrome, client only.** The span model, palette and
   theme, the stripe, the selection band, the dividers, the icon sets,
   the agent icons, `m:ss`, the labels and title cleaning, `↓ N more`,
   Loading and the empty states. No protocol change.
3. **Done, seen and stale.** The `attention` capability and record, the
   `client-session-changed` hook, `attention.json`, ✅, stale by age
   with `dim_stale` and `stale_after`, `z` for settle with 💤, and the
   sort modes.
4. **Git stats on the host daemon.** `git-status`, the `git` object,
   `branch.<b>.laatmux-base` set by `add`, the poll policy and the
   rendering, measured on the VM with twenty worktrees.
5. **PR and checks in the laptop's daemon.** `branches`, the batched
   GraphQL call through `gh`, the cache, the rendering, and the
   dashboard's `o` and `O`.
6. **Two views.** `rows` split into the agent and tree builders; the
   tree with repository nodes by source, worktree lines with the host in
   parentheses, children from the pane and run records the client now
   keeps, tasks, other sessions, folds and the pinned repository line;
   `Tab`, the tab header, selection across a switch, the stale fold in
   the agent view; the keys `f`, `s`, `h`, `l`; jumps to a pane; both
   views in the dashboard.
7. **Templates.** The tokens, `{fill}`, overflow, styles, template
   errors, and the tree templates.
8. **Placement and controls.** `top`, `%` widths, `--session`, `F`, the
   `sidebar` subcommands, `jump_keys`, `?`, the quit question, and
   `sidebar.json`.
9. **Dashboard columns.** The git and PR columns and the failing check.

Steps 2, 3, 4 and 6 are independent of each other after this note. Step
5 needs step 4's base only for the base branch. Step 7 goes after 2
through 6, so its tokens have something to show; step 8's persistence
takes the view and folds step 6 keeps in memory.

## Tests

- **Rendering:** golden tests for every layout and both views, with
  colours, stale, done, a narrow width with fields dropped in order, a
  repository with worktrees on two hosts, a worktree with two agents, a
  shell and a run, other sessions, and folds.
- **Switching:** selection across `Tab` from an agent, a worktree, a
  pane, a repository line, a task, and an empty worktree.
- **Git:** temporary repositories for base resolution, committed and
  uncommitted counts with untracked and binary files, conflict, rebase,
  the base branch itself, and no upsert when only `checked_at` changed.
- **GitHub:** a fake `gh` for chunking, the aggregation table, the
  cache, and a missing or logged-out `gh`.
- **Attention:** a working-to-idle transition with the user in the
  session, one with the user elsewhere, a daemon restart between, and
  an agent removed.
- **Templates:** a parser table with unknown tokens, styles and `{fill}`.
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
