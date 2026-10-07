# Architecture

What runs where, what flows between the parts, and where each thing in
the tree lives. The README says what the commands do and how to run
them; the milestone notes under `docs/` are the design history. This
is the current shape, as of the #73 refactor pass.

## Processes

One binary, three roles:

- **A daemon** (`laatmux serve`) on every machine that has worktrees or
  agents: the laptop and each configured host. It polls that machine's
  tmux servers and git, derives agent state, runs `add`, `rm`, `run` and
  prompt deliveries there, and serves one JSON-lines stream over a local
  socket. Detection never crosses the network: a daemon only ever looks
  at its own host's tmux. Every daemon `serve` starts has the merge, the
  relay, attention and branches; the worktree listing, git status and
  `run` need the host's directories (its own entry in its config, the
  one without `ssh`), and `add`, `rm` and the journal the managed
  `laatmux` server too. What makes the laptop's daemon the merging one
  is that the laptop's config lists the hosts and the laptop's clients
  subscribe to the merged stream; a host's config lists only the host
  itself, as a rule, and then its merged stream holds its own records
  alone. A daemon's own records enter its merged stream only through an
  entry for itself.
- **The merge**, on the laptop: the daemon dials every configured remote
  host (`ssh -T host laatmux bridge`, which relays stdio to that host's
  daemon, starting it on demand) while a merged client is connected and
  for a minute after, keeps each host's records beside its own (given an
  entry for itself), and publishes one merged stream to local clients
  with a host record per host for connectivity. The relay's pending
  tasks (`add --detach` and the task form), the attention records and
  the PR and check state from GitHub are kept there too.
- **Clients** are the other commands. The listings, the views and the
  commands that find a host through the stream (`ls`, `watch`, the
  sidebar pane, the dashboard popup, `compose`, `tasks`, `jump`, `path`;
  `hosts` for its GitHub line, dialling every host itself for the rest)
  dial the local daemon, starting it on demand, and read the merged
  stream; `add`, `rm`, `run` and `new` go through the local daemon's
  relay or dial the host themselves through the bridge (a `jump` to the
  user's default server switches through tmux alone). `sidebar
  on|off|attach|reap|fit`, `split`, `settle`, `unsettle`, `shell` (a
  shell window, local or over ssh) and `explain` work tmux directly; the
  sidebar's controls (`next`, `prev`, `jump`, `view`, `scope`) find the
  pane through tmux and write to its socket; `upgrade` builds or takes a
  binary, installs it locally or over ssh, stops the old daemon, then
  dials the host, which starts the new one; `repos` reads the config and
  `last.json`; `version` reads nothing; `stop` and `sidebar seen` dial a
  running daemon without starting one.

## Streams

A daemon's own stream is a snapshot then upserts and removes, numbered
in one sequence under the daemon's lock, so a snapshot and the changes
after it never interleave; a subscriber that falls behind (its buffer
full) is dropped and resnapshots. The record kinds: agents (a pane with
an identified agent: activity from the screen and the pane's title,
liveness of the process, the worktree it belongs to), worktrees (from
`git worktree list` under the configured directories), panes and runs
(the tree's children beside the agents, on hosts with attribution), and
the listing stamp.

The merged stream is the same shape with every host's records, host
records, the local workspace sessions, the pending tasks and their
handoffs, the attention records and the branch records. A client's copy
of it is `internal/merged`: `State` applies it, `Status` is one copy for
a reporter, and `ls`, `watch`, the sidebar and the dashboard are
functions of that.

Commands are streams too: `add`, `rm`, `run` and `prompt` are numbered
progress messages then a result, kept for a while so a client that lost
its connection can `follow` it: by id, or for a prompt by the task's id
and the attempt number. The relay's own add (`add --detach` and the task
form) and its prompt without an attempt number are answered with one
result instead; for the add the file is the acceptance.

## The daemon's parts

`internal/daemon` is one `Daemon` with most of its state under one
lock, `mu`; the parts with locks of their own (the relay's records, the
journal, the command table, a command, a run, the path cache) and the
two fields under other locks are named on its doc. The parts, each in
its file:

| Part | File | What |
|---|---|---|
| poll and observe | daemon.go (`poll`, `observe`) | the targets' panes every interval; identity from `procs`, activity from `detect`; the records published |
| worktrees | worktrees.go | the git listing, the managed sessions by root, the published join |
| attribution | attribution.go, resolve.go | which worktree a pane is in, by its path under the listed roots, with a cache of resolved paths |
| git status | gitstatus.go | diff stats per worktree, refreshed when due |
| the task runner | runner.go, commands.go, task.go, runs.go, trust.go | `add`, `rm`, `run`, prompt delivery, the trust watchers; the command table and the keyed locks |
| the journal | journal.go | one file per task on the host, what a restart recovers |
| the merge | merge.go | the hosts dialled, their records kept, the merged stream |
| the relay | relay.go, gone.go | the pending tasks, their delivery to a host, their handoff to the worktree they became, and what is left of a task once that worktree goes (gone.go) |
| branches | branches.go | the PR and check state of every branch with a worktree in the merged stream whose source is on GitHub (github.com or a configured host), from `gh`, while a merged client is connected |
| attention | attention.go | what each agent did last and whether it was seen |
| connections | daemon.go (`HandleConn`) | one handler per message type |

The locks and their order are documented once, on `Daemon`'s doc
comment; a method with the `Locked` suffix is called with its
receiver's lock held, with the exceptions named there.

## Packages

By what each imports from the tree (leaves first):

| Package | Imports | What |
|---|---|---|
| `protocol`, `source`, `peer`, `palette`, `home`, `procs`, `detect`, `tmux` | nothing | the wire format and the records; a source's key and forge split; a host as a value; the colours; the state directory, the environment id, the runtime file, the startup lock, `last.json` and `sidebar.json`, atomic writes; processes; the detection rules; tmux |
| `term` | palette | the terminal: raw mode, the frame, the keys |
| `github` | protocol | the GraphQL queries for PRs and checks |
| `rows` | protocol, source | the rows every listing shares: the tree rooted at repositories, the agent view's tiles |
| `config` | palette, peer, source, tmux | the config file, and `.laatmux.yaml` per repository |
| `client` | home, peer, protocol | dial a daemon, local or through the bridge; requests and streamed commands |
| `worktree` | config, protocol, source, tmux | checkouts, worktrees, the git stages of `add` |
| `workspace` | client, peer, protocol, source, tmux | the local workspace sessions on the default server |
| `view` | palette, protocol, rows, term | the list view the sidebar pane and the dashboard share |
| `merged` | client, config, peer, protocol, rows, source | the client's copy of the merged stream |
| `command` | client, config, home, peer, protocol, tmux, workspace | the client side of `add`, `rm`, `run` and `shell` |
| `daemon` | client, command, config, detect, github, home, peer, procs, protocol, source, tmux, worktree | above |
| `cmd/laatmux` | all of them | the commands, and the sidebar's tmux side, over the above |

`rows` and `view` depend on nothing that touches a socket or a
process: the views are tested as functions of records.

## On disk

Under `home.Dir()` (`LAATMUX_HOME`, else `$XDG_STATE_HOME/laatmux`, else
`~/.local/state/laatmux`): the daemon's socket (`laatmux.sock`), runtime
file (`runtime.json`) and startup lock (`daemon.lock`), its log
(`daemon.log`, appended), `environment-id`, the command journal
(`commands/`), the relay's pending files (`pending/`), `attention.json`,
`branches.json`, `last.json` (per repository, the host a successful or
accepted `add` last used, and the named agent when one was named: the
form's and `add`'s defaults), `sidebar.json` (the views' start settings
and folds), `sidebar.lock` (the sidebar's check-and-create) and
`sidebar/`, one socket per sidebar pane for its controls. The record
files are written whole through a temporary name (`home.WriteAtomic`);
the log is appended to, the locks hold a pid or nothing, and the
environment id is made through a link.

## tmux

The daemon configures one server, `laatmux`, where it creates sessions:
its global options are set, the session-level overrides of the isolation
options unset, both key tables emptied and every global hook removed, on
discovery once per server pid and before every session it creates. Every
other server it polls is the user's and is read only. Workspace sessions
live on the user's default server, tagged with the worktree's key, and
the sidebar pane is a pane of the user's window running `laatmux sidebar
pane`.
