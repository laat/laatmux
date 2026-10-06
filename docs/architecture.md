# Architecture

What runs where, what flows between the parts, and where each thing in
the tree lives. The README says what the commands do and how to run
them; the milestone notes under `docs/` are the design history. This
is the current shape, as of the #73 refactor pass.

## Processes

Three kinds of process, one binary:

- **A host daemon** (`laatmux serve`) on every machine that has
  worktrees or agents: the laptop and each configured host. It polls
  that machine's tmux servers and git, derives agent state, runs `add`,
  `rm`, `run` and prompt deliveries there, and serves one JSON-lines
  stream over a local socket. Detection never crosses the network: a
  daemon only ever looks at its own host's tmux.
- **The merging daemon** is the laptop's host daemon with `hosts` in its
  config. It dials every configured host (`ssh -T host laatmux bridge`,
  which relays the remote socket), keeps each host's records, and
  publishes one merged stream to local clients with a host record per
  host for connectivity. It also keeps what is the laptop's alone: the
  relay's pending tasks (`add --detach`), the attention records, and
  the PR and check state from GitHub.
- **Clients** are every other command: `ls`, `watch`, the sidebar pane,
  the dashboard popup, `add`, `jump` and the rest. A client dials the
  local daemon, starting it on demand, and reads the merged stream; a
  command that acts on a host goes through the local daemon's relay or
  dials the host itself through the bridge.

## Streams

A daemon's own stream is a snapshot then upserts and removes, numbered
in one sequence under the daemon's lock, so a subscriber that falls
behind can be dropped and resnapshot. The record kinds: agents (a pane
with an identified agent: activity from the screen, liveness of the
process, the worktree it belongs to), worktrees (from `git worktree
list` under the configured directories), panes and runs (the tree's
children beside the agents, on hosts with attribution), and the
listing stamp.

The merged stream is the same shape with every host's records, host
records, the local workspace sessions, the pending tasks and their
handoffs, the attention records and the branch records. A client's copy
of it is `internal/merged`: `State` applies it, `Status` is one copy for
a reporter, and `ls`, `watch`, the sidebar and the dashboard are
functions of that.

Commands are streams too: `add`, `rm`, `run` and `prompt` are numbered
progress messages then a result, kept for a while so a client that lost
its connection can `follow` the id.

## The daemon's parts

`internal/daemon` is one `Daemon` with its state under one lock, `mu`,
and these parts, each in its file:

| Part | File | What |
|---|---|---|
| poll and observe | daemon.go, observe | the targets' panes every interval; identity from `procs`, activity from `detect`; the records published |
| worktrees | worktrees.go | the git listing, the managed sessions by root, the published join |
| attribution | attribution.go, resolve.go | which worktree a pane is in, by its path under the listed roots, with a cache of resolved paths |
| git status | gitstatus.go | diff stats per worktree, refreshed when due |
| the task runner | runner.go, commands.go, task.go, runs.go, trust.go | `add`, `rm`, `run`, prompt delivery, the trust watchers; the command table and the keyed locks |
| the journal | journal.go | one file per task on the host, what a restart recovers |
| the merge | merge.go | the hosts dialled, their records kept, the merged stream |
| the relay | relay.go, gone.go | the laptop's pending tasks, their delivery to a host, their handoff to the worktree they became |
| branches | branches.go | the PR and check state of every branch in the stream, from `gh` |
| attention | attention.go | what each agent did last and whether it was seen |
| connections | daemon.go (`HandleConn`) | one handler per message type |

The locks and their order are documented once, on `Daemon`'s doc
comment; a method with the `Locked` suffix is called with its
receiver's lock held, with the exceptions named there.

## Packages

By what each imports from the tree (leaves first):

| Package | Imports | What |
|---|---|---|
| `protocol`, `source`, `peer`, `palette`, `home`, `procs`, `detect`, `tmux` | nothing | the wire format and the records; a source's key and forge split; a host as a value; the colours; the state directory, the environment id, the runtime file, the startup lock, atomic writes; processes; the detection rules; tmux |
| `term` | palette | the terminal: raw mode, the frame, the keys |
| `github` | protocol | the GraphQL queries for PRs and checks |
| `rows` | protocol, source | the rows every listing shares: the tree rooted at repositories, the agent view's tiles |
| `config` | palette, peer, source | the config file |
| `client` | home, peer, protocol | dial a daemon, local or through the bridge; requests and streamed commands |
| `worktree` | config, protocol, source | checkouts, worktrees, the git stages of `add` |
| `workspace` | client, peer, protocol, source, tmux | the local workspace sessions on the default server |
| `view` | palette, protocol, rows, term | the list view the sidebar pane and the dashboard share |
| `merged` | client, config, peer, protocol, rows, source | the client's copy of the merged stream |
| `command` | client, config, home, peer, protocol, tmux, workspace | the client side of `add`, `rm`, `run` and `shell` |
| `daemon` | client, command, config, detect, github, home, peer, procs, protocol, source, tmux, worktree | above |
| `cmd/laatmux` | all of them | the commands, thin over the above |

`rows` and `view` depend on nothing that touches a socket or a
process: the views are tested as functions of records.

## On disk

Under `home.Dir()` (`LAATMUX_HOME`, else the platform state dir): the
daemon's runtime file and startup lock, its log, the command journal
(`commands/`), the relay's pending files (`pending/`), the attention
file, the branches file, `last.json` for the form's last choices. Every
file is written whole through a temporary name (`home.WriteAtomic`).

## tmux

The daemon configures one server, `laatmux`, where it creates
sessions: its global options, hooks and key tables are reconciled on
discovery, once per server pid. Every other server it polls is the
user's and is read only. Workspace sessions live on the user's default
server, tagged with the worktree's key, and the sidebar pane is a pane
of the user's window running `laatmux sidebar pane`.
