# laatmux

Git worktrees and coding agents across hosts, from tmux: a daemon per
host that watches its tmux servers and git, a merging daemon on the
laptop that joins the hosts into one stream, and a sidebar, a dashboard
and a CLI over it. The plan is [issue #1](https://github.com/laat/laatmux/issues/1);
[docs/architecture.md](docs/architecture.md) says what runs where. The
milestone notes under `docs/` are the design history: two, worktrees
and workspaces; three, the sidebar, the dashboard, `split` and `run`;
four, the task form and the background add; five, the two sidebar views
at workmux's level. All of it is built.

## Layout

| Package | What |
|---|---|
| `cmd/laatmux` | CLI: `serve`, `bridge`, `add`, `tasks`, `rm`, `prune`, `run`, `path`, `ls`, `watch`, `sidebar`, `dashboard`, `compose`, `jump`, `shell`, `split`, `settle`, `unsettle`, `new`, `hosts`, `upgrade`, `stop`, `repos`, `explain`, `version` |
| `internal/protocol` | JSON-lines wire format, protocol version 1, capability flags, agent, worktree, pane, run, host and session records |
| `internal/daemon` | polls the configured tmux servers and git, derives agent state, streams snapshot + upserts; runs `add`, `rm` and `run` with numbered progress a client follows by id; merges the configured hosts' streams into one for local clients |
| `internal/worktree` | checkouts found under `repos` by origin, worktrees from `git worktree list`, the git and filesystem stages of `add` |
| `internal/detect` | screen and title rules, ported from herdr's manifests (Apache 2.0, see `manifests/NOTICE`) |
| `internal/procs` | agent instance identity from the tty's foreground process group (sysctl on macOS, /proc on Linux) |
| `internal/tmux` | `list-panes -a -F`, `capture-pane`, managed server config, `new-session` in one invocation, `kill-session`, branch encoding for session names |
| `internal/peer` | a configured host as a value: its label, ssh alias and remote binary |
| `internal/client` | dial local daemon (start on demand) or `ssh -T host laatmux bridge`; request and streamed command |
| `internal/command` | the client side of `add`, `rm`, `run` and `shell`, one implementation each for the CLI and the dashboard, with the reconnect and follow logic |
| `internal/workspace` | the local workspace session on the default tmux server: tags, attach and shell commands, create, switch, kill |
| `internal/merged` | the client's copy of the merged stream: the state applied, one status for a reporter |
| `internal/rows` | the rows the listing, the sidebar and the dashboard share: worktrees joined with agents and local sessions, dim state, groups |
| `internal/term` | the terminal: raw mode and the alternate screen, the frame drawn, the bytes read decoded into keys, mouse events and pastes |
| `internal/view` | the list view: the renderer for the tiles, compact and strip layouts and both views, the model's keys and mouse, the draw loop |
| `internal/home` | state dir, environment id, runtime file, startup lock, `last.json` |
| `internal/config` | `~/.config/laatmux/config.yaml`: hosts with their directories, agents, the repository list, `tmux_servers` for this machine's daemon, `sidebar`; `.laatmux.yaml` per repository |
| `internal/source` | a repository source's key, the same for the forms a forge gives one repository, and the forge split |
| `internal/github` | the GraphQL queries for a branch's PR and checks, through `gh` |
| `internal/palette` | the colours: the named palette, a theme's overrides, what a template's colour name resolves to |

## Run

```sh
go build -o laatmux ./cmd/laatmux
./laatmux ls        # starts the local daemon on demand, lists workspaces and agents
./laatmux watch     # live, redraws on change
./laatmux hosts     # reachability, daemon version, capabilities; marks daemons that differ from this build
./laatmux hosts pause vm    # stop dialling vm from this machine; hosts resume vm dials it again
./laatmux upgrade vm    # build for the host from this checkout, install over ssh, restart its daemon
./laatmux stop      # end this machine's daemon cleanly; the next command starts one again
./laatmux repos     # each known repository's name and where it lands on each host
./laatmux add fix-ls                          # worktree and agent for the repo of the current directory, on the last-used host
./laatmux add fix-ls --repo proj --host vm --agent claude
./laatmux add -p 'make ls sort by host'          # branch proposed from the prompt, made unique on the host; the agent gets the prompt
./laatmux add fix-ls -p 'make ls sort by host'   # the same with the branch given
./laatmux add -p 'make ls sort by host' --detach # hand it to the local daemon and return; laatmux tasks shows it
./laatmux add -p 'try it' --repo git@github.com:nrkno/pin-scripts.git  # a repository not in the config: added to repos once the worktree is made
./laatmux tasks                                  # the background adds and their state; tasks show|dismiss|prompt <id>
./laatmux path proj/fix-ls                    # the worktree root on its host
./laatmux jump vm/proj/fix-ls                 # switch to the workspace session, creating it if missing
./laatmux shell                               # a shell at the worktree root, from inside a workspace session
./laatmux split -h '#{pane_id}'               # from a binding: split the pane; a workspace's new pane is a shell at the root on its host
./laatmux run proj/fix-ls -- go test ./...    # run in the worktree root on its host, output and exit status streamed back
./laatmux run -- make                         # inside a workspace session, that workspace
./laatmux settle                              # collapse this workspace in ls; unsettle brings it back
./laatmux rm proj/fix-ls [--force]            # remove the worktree, its managed session and the local session
./laatmux rm                                  # inside a workspace session: that workspace
./laatmux prune -n                            # the worktrees with no session: which would go, which stay, and why
./laatmux prune --host vm --repo proj --branches  # remove them after one question, their local branches too
./laatmux explain --tmux-socket default %12   # detection inputs and decision for one pane
./laatmux new work --cwd ~/code/foo -- claude # managed session without a worktree
./laatmux jump mac/work                       # a local session attached to it
./laatmux jump --server default mac/notes     # switch to an observed session in this machine's tmux
```

Config, one entry per host; omit `ssh` for this machine:

```yaml
hosts:
  - name: mac
    repos: ~/code             # main checkouts live in <repos>/<name>
    worktrees: ~/worktrees    # worktrees live in <worktrees>/<name>/<branch>
  - name: box
    ssh: box                  # ssh alias, ControlMaster assumed
    bin: laatmux              # remote binary, must be on PATH of a non-interactive shell
    repos: [~/src, ~/src/work]  # or several: a checkout is found in any of them,
                                # a clone for add is made in the first
    worktrees: ~/src/worktrees
    paused: true              # not dialled from this machine until resumed (below)
tmux_servers: [laatmux, default]   # what this machine's daemon watches
agents:
  claude:
    cmd: [claude, "{prompt}"] # {prompt} is replaced by add -p's prompt, or removed
  claude-safe:
    cmd: [claude-safe]        # sandboxing is the launch command's business;
                              # without {prompt} the prompt is typed into the pane
default_agent: claude         # what add starts when --agent and last.json say nothing
repos:                        # the known set
  - git@github.com:laat/laatmux.git
  - source: https://github.com/laat/other.git
    name: notes               # optional; otherwise derived from the source
    copy: ["config/*.local"]  # this machine's own steps for the repository's worktrees,
    setup: ["pnpm install"]   # after the committed .laatmux.yaml's
copy: ["**/.envrc.cache.enc"] # copy rules for every worktree this machine adds or makes
```

`hosts`, `agents` and `repos` are read by clients. `tmux_servers` and the
local host's `repos` and `worktrees` are read by the daemon on the machine
the file lives on, so the laptop's config cannot change what a remote daemon
watches or which directories it uses; each host's own config does that. The
default server list is the managed `laatmux` server alone. The daemon
follows the file's `repos`, `copy`, `agents` and `hosts`: every two
seconds it looks at the file, and reads it again when it has changed
(another file renamed over it, or a new modification time or size), so
a repository added by hand or by the task form shows in the listing
without a restart, a `copy` rule, a repository's `copy` and `setup`, or
an agent added or edited is used by the next add, a host added or
removed reaches the views already open, and the relay retries the
appends a broken file refused (below); a daemon whose own entry has no
directories watches the file for the relay all the same. An add takes
the repository, its steps and the top-level `copy` from one read, the
daemon's last as the add starts, whatever the file says while its clone
runs. `tmux_servers`, `github_hosts` and the local host's directories
are read when the daemon starts; `laatmux stop` makes the next one take
an edit. A file that does not parse keeps what the daemon had, with one
line in its log. A file read empty within two seconds of its last change
is one an editor has truncated to write it again in place: the daemon
and the views keep what they have, the hosts included, and read it again
at the next look; one left empty longer, like a missing file, is the
default config, as at start.

The dashboard, the sidebar panes and `compose` follow the file the same
way, every two seconds. The dashboard and a sidebar pane take the
hosts, agents, `default_agent`, repositories and `copy` their keys and a
task form opened from then on use, the hosts and their `paused` on the
hosts line, this machine's names for the repositories in the rows, the
sidebar's `sort` and stale settings, the line templates, `icons`,
`status_icons`, `kind_icons`, `agent_icons`, the theme, `jump_keys`'
labels and the strip's `item_width`. A task form, the dashboard's or
`compose`'s, reads the file again as a chip's picker
opens: a form left up offers the repositories, hosts and agents listed
then, each chip kept on its choice when that is still there, else on
the default the file gives now, and its submit sends that read's `copy`.
`compose` takes the theme too. What a running view does not take: the
pane's place and size (`position`, `width`, `height`), which `sidebar
on` and the hooks set, a width being put back by the next resize; the
`layout`, `view` and `scope` it started in, which its keys and
`sidebar.json` own from then on; the bindings `jump_keys` makes, which
`sidebar on` sets; and the terminal's background, asked once as the view
starts, so a theme turned to `auto` later takes that answer, else
`COLORFGBG`, else dark. A file that does not parse leaves a view as it
was, with the error in its footer, and in the note of a task form that
is up, until the file is put right.

A host reached over ssh can be paused, for a machine that costs money
while it runs, a cloud workspace that ssh starts say: `paused: true` on
its entry, which `laatmux hosts pause <host>` writes and `laatmux hosts
resume <host>` takes out, as does `H` in the dashboard and the sidebar,
a picker of the hosts with their state (connected, paused, down) where
`Enter` flips the one picked, and a click on the host's entry on the
hosts line below their list. The write keeps the rest of the file as
the task form's append does. This machine does not dial a paused host:
the daemon drops its merged subscription and removes its records, as
for a host removed from the config, within its two-second look at the
file, and takes it up again when it is resumed; the relay does not
follow its tasks, and closes a connection it has open to the host, an
add it follows going on on the host, though a prompt delivery `p` began
before the pause ends first; `hosts` does not probe it, `prune` passes
over it, and no PR or checks are asked for its worktrees, which are out
of the stream. A command aimed at it, `add --host`, `jump`, `rm`,
`run`, `path`, `new`, `prune --host`, `split` and a shell in its
workspace, or a task form's submit, is refused with `host vm is paused; laatmux hosts resume vm connects
it`; `upgrade`, which the user runs by hand, connects it and says so. A
task queued for it before the pause stays in its file, its row saying
`host vm is paused` where it waits on the host and not first on the
user, and is followed again once the host is resumed, from the host's
journal where the add went on; nothing is dismissed, though a task not
sent in seven days is `outcome unknown`, paused or not. The host's own
daemon is not told: pausing is this machine's refusal to dial, nothing
more. This machine's own entry, which nothing dials, cannot be paused.
A local daemon of a build older than pause, capability `pause`, reads
the file without the key and goes on dialling the host: `laatmux hosts
pause` and `H` say so, and `laatmux stop` ends it, the next command
starting the current build.

A repository the config does not list is added from the task form or
from `add`: a source in one of the forge forms below
(`git@github.com:nrkno/pin-scripts.git`, `ssh://…`, `https://…`) pasted
into the repository chip's picker, or given to `--repo`, is the add's
repository, named as the list would derive it (`pin-scripts`), or, when
that name would rename a listed repository or is no label, under a name
of its own written with it (`nrkno-scripts`, `next_js`). A credential in
the source, an https URL's user and token or an ssh URL's password, is
left out, and the form's footer or add's output says so: it never
reaches the config, a pending file or a host, and the host clones
through its own credentials. The add carries it as its `repo_entry`,
and once the host has made the worktree the source is appended to
`repos`: by `add` itself in the foreground, and by the local daemon's
relay for the form and `add --detach`. An add the host refuses, a clone
that fails say, leaves the config as it was. The relay keeps the ask in
the task's pending file until the append is made: an append that fails,
a config that does not parse at that moment say, holds the task, whose
row then says `done, not added to the config` with the reason and needs
the user, who may fix the file or dismiss the task with `x`; the relay
tries again whenever the config file changes and at every start,
handed-over tasks included, and the task hands over to its worktree row
once the append is made. A dismiss, by `x` or `laatmux tasks dismiss`,
drops the append for good: its message names the source, the name it
would have had and why the append failed, so the entry can be added by
hand or the source pasted again, and the daemon's log has it once. A
handed-over task still asking is kept past the day a handoff is kept
for, and one dropped with its worktree, by `rm` or a listing that finds
it gone, has the dropped append in the log. A source the list has in
another form is that repository and is not added again. The repository
picker reads the config again as it opens, so a repository an earlier
add appended is a listed candidate, not a new one. The append keeps the file as it was
around the new line, comments and blank lines included: the line goes
after the list's last item, in its indentation, or a `repos:` list is
made; a file the line cannot go into, a list written `[a, b]` say, is
written again from its parsed YAML, which keeps the content and the
comments but not the layout. A file of more than one YAML document with
content is refused, since laatmux reads the first and a rewrite would
drop the rest; an empty one after it, a `---` at the end or one of
comments alone, is kept as it is, the line going before its marker,
though a file that needs the rewrite and has one is refused too. The
result is parsed before it replaces the file, through a temporary
renamed over it, the link's target when the config is a symlink, made
with its directory when the link's target is not there, with the file's
mode. The appends on one machine take turns under `config.lock` in the
state directory, waiting up to ten seconds for another, and a file
another writer changed between the read and the rename is read again. A host's own daemon needs
no `repos` entry for the add, since `repo_entry` carries the source; its
listing labels the new checkout by its directory's name, the entry's
name, with its origin as the source, until its own config names it.

The repositories are the laptop's to decide. An add carries the repository
as the sending machine's config has it: source, name, `copy` and `setup`,
with the top-level `copy` after the repository's own. A host daemon with
the `repo-entry` capability resolves an add for a repository its own
config does not list against that entry, so a host needs no `repos` list
for it. The host's own top-level `copy` applies after the entry's, as it
does to every worktree the host makes; a file copied already is skipped.
A repository the host's config does list is the host's own: its source,
the transport the host clones with, its name and its steps decide, as
before entries. An entry whose name is the host's name for another
repository is refused. A host with an older daemon ignores the entry and
needs the repository in its own list, as before.

The forms a forge gives one repository are one repository:
`git@host:owner/repo`, `ssh://git@host/owner/repo` and
`https://host/owner/repo`, with or without `.git` and a trailing slash,
the host compared without case. Only the forge convention is unified: ssh
as the user `git` on the default port with a path from the forge's root,
`git+ssh://` and `ssh+git://` being git's other spellings of `ssh://`,
and http or https on the default port, whose user is a credential. Any
other source compares exactly, since there a user, a port or a leading
slash can name another repository. A host finds an existing checkout in
any of the forms and fetches through that checkout's own origin, so each
machine keeps the transport it can use. Two forms of one repository in
the same `repos` list are rejected as a duplicate, so a config that
listed both must drop one before the daemon starts again.
`laatmux serve --tmux-servers laatmux,default` overrides the file.

An agent's `cmd` is an argv: the first word is the program, and each
word is one argument, quoted for the shell tmux starts the agent with.
A first word such as `FOO=1` or `PATH+=:/opt/bin` would be run as a
command, so the config refuses it, and the daemon does not start until
it is changed; `add -- <cmd>` and `new -- <cmd>` refuse it too. A
variable is set through `env`, as in `cmd: [env, FOO=1, claude]`.

An agent's `cmd` may carry `{prompt}` as one argument: `add -p` replaces
it with the prompt, as one argument however many lines it has, and an add
without a prompt removes it. The prompt is then on the host's process
list for as long as the agent runs, visible to another user of that host
with `ps`; choosing the placeholder is choosing that. A `cmd` without it
gets the prompt typed into the pane once the agent is ready, as the
task section below says.

Host names, agent keys and repository names are labels: `A-Z a-z 0-9 _ -`,
nothing else, since they end up in session names, ids and directory names.
A label does not start with `-`, so it never reads as a flag in the
commands laatmux prints, such as the `laatmux add` line a hint gives.
A host named after its ssh alias or the machine's hostname, and a
repository named from its source, must pass the same rule or the config
is rejected asking for an explicit `name`. An `ssh` alias that starts
with `-` is rejected whatever the host's name, without that advice,
since ssh would read it as an option. The `bin` of a host with `ssh`
is rejected, naming the host, when it starts with `-`, since the tools
in `upgrade`'s install script would read the path as options, and so,
unless the path needs quoting, would the host's shell running the
bridge. `repos` and `worktrees` have no defaults, and must be absolute
or start with `~`; a host without them cannot `add`. A host's `repos` is
one directory or a list of them, `[~/code, ~/code/group]` say, for
checkouts kept in a directory that groups related repositories: each
is scanned as one is, its direct children with an `origin`, and no
deeper, so a group is listed by name. A repository found in none of
them is cloned into the first; worktrees go under the one `worktrees`
directory whichever directory has the checkout. A checkout reached
twice, by a directory listed twice or through a symlink, a link in one
directory to a checkout in another say, is listed once: by its own path
where a `repos` directory has it, else by the path scanned first. A repository's
name is derived from its source: the last path
component without `.git`; on a collision each is prefixed with its org
(`laat-laatmux`, `acme-laatmux`); if they still collide, or there is no org
to prefix, the first six hex digits of the source's SHA-256 are appended.
The derivation is deterministic, so every host derives the same name from
the same list, and duplicate sources or duplicate final names are rejected.
Identity is the source, not the name: the name only places new things.
`laatmux repos` shows each name next to its checkout and worktree paths per
host, as configured, so a `~` is the host's own; for a host with several
`repos` directories, a line before the list names them and the first,
where the checkout path each repository's line shows is cloned when no
directory has a checkout of it.

Shared setup lives in `.laatmux.yaml` at the repository root, committed:

```yaml
copy: [.envrc, .env.local]   # from the main checkout, skipped when present
setup: ["pnpm install"]      # each runs at least once; must tolerate a rerun
```

Each `setup` entry runs as `sh -c <string>` in the worktree root. What
is personal to a machine goes in its config rather than the committed
file: a top-level `copy` list applies to every worktree the machine's
daemon makes, and a repository entry's `copy` and `setup` to that
repository's, each run after the committed file's. A `copy` entry is a
path relative to the repository root, or a glob: `*`, `?` and `[...]`
within a path segment, `**` as a whole segment for any number of them.
A glob is matched against what git knows of the main checkout, the
files it tracks or does not ignore plus the ignored files, which is
where an encrypted env cache sits, with ignored directories collapsed,
so `**/.envrc.cache.enc` finds the caches in every package and never
walks `node_modules`. Each match that is a regular file is copied as
a literal entry is, through a temporary file renamed into place,
skipped when present; a symlink, a directory or a submodule a glob
matches is passed over. Nothing is read from outside the checkout or
written outside the worktree: a source is read where it resolves and a
target written where its directory resolves, and either resolving out
through a symlink is an error. The committed commands and a
repository's own each number their own completion markers, so a
committed list that grows does not move a personal command onto
another's marker. The
last-used host and agent per repository are state, not config: they live in
`$LAATMUX_HOME/last.json`, keyed by source, and are updated under a lock
with an atomic rename. The design is in
[docs/milestone-two.md](docs/milestone-two.md); both sides are built, see
the two sections below.

## Worktrees and add, daemon side

The daemon on a host with `repos` and `worktrees` advertises `worktrees`,
and with the managed server also `add`, `rm` and `prune`. Git is the source of
truth; labels only place new things.

- **Git status**, capability `git-status` (milestone five, step 4): each
  worktree record carries a `git` object once read. It holds:
  - `base`: the branch's `laatmux-base` key, which `add` sets to the
    resolved `origin/HEAD` on a new branch; else `origin/HEAD`'s branch,
    `main` or `master`;
  - `committed`: the branch against its merge base with that base;
  - `ahead` and `behind`;
  - `uncommitted`: the diff against HEAD plus the untracked files that
    are not ignored, a lower bound past 200 files or 1 MB a file;
  - `dirty`, `conflict` (from `git merge-tree --write-tree`, git 2.38 and
    later), `rebasing`, and `stale` when the last refresh timed out.

  Two workers beside the listing refresh it: every 5 s for a worktree
  or a main checkout with a home session and for a main checkout with
  an agent in it, every 30 s otherwise, and at once after an add, a
  run ending, or a change to `HEAD`, `index` or the refs of the branch
  and its base. A refresh is at least 2 s after the last one, never waits
  on the listing, and upserts only when a value changed. Git runs with
  `--no-optional-locks`, the diff against HEAD without refreshing the
  index, the diffs with `--no-ext-diff --no-textconv`, and no transport
  allowed: a refresh never takes the index lock, runs a diff driver, or
  fetches into a partial clone. The views show the stats on a tile's
  second line and before the time in the compact line: `R` while
  rebasing, the committed `+N -M`, then `✎` and the uncommitted `+X
  -Y`. A merging daemon that knows the field forwards it.
- **Worktree records** arrive in the subscription stream next to agents:
  `worktrees` in a snapshot, `worktree` in an upsert, `worktree_id` in a
  remove. Every two seconds the daemon scans the main checkouts under
  each `repos` directory, in the order the list has them, each found by
  having an `origin`, asks each that has a linked
  worktree under `worktrees/`, read from the `gitdir` files git keeps,
  for `git worktree list --porcelain`, and publishes the entries under
  `worktrees/`, a root once. Every checkout counts, listed in the config
  or not: one the config lists is labelled with the config's name and
  source, any other with its directory name and its origin. A directory
  name that is not a label, from a clone made by hand say, is made one:
  each character a label cannot have, and a leading `-`, becomes `_`,
  so `next.js` is labelled `next_js`, and a jump target names it by
  that label, as `ls` shows it. Such a label does not take one another
  repository has, the config's name for it or the label of another
  checkout the config does not list, `next_js` or `next:js` say: the
  config's name, or the checkout named so, keeps it, else the first in
  scan order, by name within a directory, and the made label gets a `-`
  and six hex digits of a hash of its origin after it, which the daemon
  logs once. Two checkouts of different repositories named alike in two
  `repos` directories, `~/code/api` and `~/code/group/api` say, are
  told apart the same way: a checkout of the config's repository named
  `api` whose directory is named `api` keeps the label, else the one in
  the directory listed first, and the other gets the hash. Clones of
  one repository share their label. Listing either repository in the
  host's config with a `name` settles it. The laptop's views show the
  laptop's own name for a source it knows. Prunable entries, whose
  directory is gone, are not published; a detached worktree has an
  empty branch. A branch checked out by hand that laatmux cannot carry,
  one with a byte that is not UTF-8 or with U+FFFD (JSON writes U+FFFD
  for such a byte), is published as laatmux prints it, quoted as Go
  quotes it when it has a byte that is not UTF-8 (`"a\xffb"`), with
  `branch_display_only`: no command names the worktree by it, and `rm`
  and `run` take it only with that worktree's `root`. The record's
  `session` is the worktree's home session: the managed session with a
  pane that records the root in `@laatmux_cwd`, all of whose panes are
  inside the root, a main checkout's root as well (below), joined from
  the pane poll, so an agent exiting updates
  the record without a git call. A split for a shell keeps the home; a
  pane gone elsewhere takes it away, and the views then jump to the
  worktree through its agent. For the agent laatmux made at the root,
  the worktree's workspace session attaches to that agent's managed
  session. For an agent on this machine's default server, the jump
  switches to the agent's session; one on a remote host's default
  server, or on another observed server, cannot be jumped to. `rm`
  still kills every managed session with a pane laatmux
  made at the root, home or not, so no agent is left in a removed
  directory. Origin reads are cached by the mtime of
  `.git/config`, and a checkout with no linked worktree under
  `worktrees/` is not asked, so an idle poll spawns one git process per
  checkout that has one. The id is `<environment_id>/worktree/<root>`.
- **Main checkouts**, capability `checkouts` (issue #338): the same
  poll reads each main checkout's branch from its `.git/HEAD`, empty
  when detached, asking git only on the reftable backend, whose HEAD is
  a stub, and publishes a worktree record with `main` for each checkout
  in use: one with a worktree listed, one an agent is attributed to
  (below), or one with a home session; being in the host's config is
  not use, since a config that lists every repository would publish a
  line for each. Its id is `<environment_id>/checkout/<root>`, its root
  the checkout's directory, its branch the one checked out, and its
  `session` its home session as a worktree's is (issue #376): the
  managed session with a pane laatmux made at its root, all of whose
  panes are inside it, which a jump makes with the user's shell (`jump`
  below) and `add` never makes; its `git` object is read as a
  worktree's, every 5 s while it has a home or an agent is in it. The
  rest of the checkouts under `repos`, many on a machine
  that clones there by hand, have no record, line or refresh. A HEAD
  that cannot be read, in a checkout with no worktree whose origin the
  daemon has read, leaves that checkout without a record, logged once,
  and fails no listing; no pane in it is a worktree's around it either.
  git itself finds no repository there, so a checkout with a worktree
  fails the listing as before, and one whose origin is not read yet is
  not scanned. The main worktree git lists first is never a worktree
  record, also where a symlink makes its path another than the
  checkout's as scanned. The records go only to a subscriber
  that asks for them with `checkouts` on `subscribe`, as this build's
  clients and merging daemon do: an older client or merging daemon
  never sees one, and gets an agent attributed to one as an agent of
  no worktree, as before, so none takes a main checkout for a worktree
  it may remove. A named agent (claude, codex) whose pane is not
  laatmux's own, with its path in a main checkout and under no worktree
  root inside it, is attributed to the checkout's record: a live one on
  the host's default server; on the managed server, live or after it
  quit, one in the checkout's home session, or in a pane laatmux made at
  its root in any session, the home's once a pane gone elsewhere took
  the home among them, as a worktree's root agent stays its worktree's.
  The record is published before the agent names it and taken back,
  when no other reason holds it, after the last such agent has left or
  quit (gone from its pane on the managed server), also when the
  checkout leaves the listing; a shell in a plain session, one left
  after its agent quit among them, puts no checkout in use, so a record
  never follows a shell's `cd`. An agent in a managed session made
  below the root, a `new` session's, keeps a row of its own, even where
  the checkout is inside a worktree, and the views take a session of
  the checkout's shell session's name for no session of the checkout's
  where a managed agent in it runs in a pane laatmux made at another
  directory, below the root too (with no pane record of the home's
  shell, a pane moved there by hand from a session `new` made below the
  root reads the same once the home is lost); a pane with no agent in
  a checkout has no pane record, its home's shell among them. `rm`
  refuses a main checkout's root, also where the repos directory is
  under the worktrees one, and `run` takes registered worktrees only,
  as before.
- **Attribution**, capability `attribution` (issue #55): every polled
  pane, on every server in `tmux_servers`, belongs to the worktree whose
  root contains its path, the recorded `@laatmux_cwd` of a pane laatmux
  made and `pane_current_path` otherwise. Paths are compared with
  symlinks resolved, on path separators (`/w/foo-2` is not inside
  `/w/foo`), the deepest root winning; a pane under no root belongs to
  none, and a pane in a main checkout to its record only as said
  above. An agent record carries
  `worktree_id`, so a worktree has any number of agents. A pane with no
  agent inside a root is a pane record, `{id, environment_id, server,
  session, window, pane_id, command, pid, cwd, worktree_id, managed}`,
  `managed` for a pane laatmux made, whose `cwd` is the directory it was
  made at, upserted
  when its command, path or place changes and removed when it leaves
  every root, goes, or has an agent identified in it: `panes` in a
  snapshot, `pane` in an upsert, `pane_record_id` in a remove. A `run`
  job is a run record `{id, environment_id, root, worktree_id, cmd,
  started_at}` while its process runs: `runs`, `run`, `run_id`. Panes
  are polled far more often than git lists worktrees; a listing that
  changes the roots attributes every pane again, so a worktree listed
  after its pane was seen gains it at once. The views pair a worktree
  with its agents by `worktree_id` against a host with the capability,
  reached directly or through a merging daemon that has it too, and by
  `session` otherwise. A worktree row shows an agent in its home
  session, or, with no home session, the first started of the agent
  laatmux made at its root and those on the default server, never chosen
  by activity, so rows do not swap as agents work; an agent in another
  managed session stays that session's, and the others keep rows of
  their own for now. A pane's path is resolved off
  the poll, so a shell on a hung mount never holds detection up.
- **`add`** `{type: add, id, repo, branch, agent_name, cmd}` runs the
  stages in the note, each step skipped by inspection: resolve (a
  checkout in any `repos` directory), clone (into the first `repos`
  directory, refused when `<first>/<name>` exists with another origin),
  fetch,
  worktree (`set-head --auto` and prune; a remote branch is tracked, an
  existing local branch used as is, a new one made with `--no-track` from
  `origin/HEAD`; the root registered on another branch or the branch
  checked out elsewhere fails the stage), copy (through a temporary file
  renamed into place), setup (markers under the worktree's git directory,
  keyed by index and hash of the command), agent (one tmux invocation,
  skipped when a managed session already runs in the root, refused as a
  name in use when the intended name runs elsewhere; one in the root made
  with the user's shell, its pane tagged `@laatmux_nocmd` as `new` with
  no command tags it, in which no agent runs by the daemon's last look
  at the pane on the server instance it is on, none started or one
  exited, is refused too, saying to start the agent in it or exit that
  shell and add again; a shell session made by an older host, untagged,
  is taken up as before). Progress streams as
  `{type: progress, id, stage, state, detail}` with state `start`, `done`,
  `skip` or `output`; the result carries `stage` on failure, and
  `session`, `pane_id` and `root` on success. `repo` is the source, and
  `repo_entry` the repository as the sender's config has it, which a
  daemon with `repo-entry` resolves the add against. Without an entry
  `repo` is the source or the label as the daemon's own config knows it.
  `remember`, on an add relayed through the local daemon, asks a daemon
  with the `remember` capability to append `repo_entry` to its config's
  `repos` once the host's add has succeeded; a client sends it only to a
  daemon with the capability, and an older daemon, which would read the
  add without the field and add nothing, is refused with a hint to stop
  it so the current build starts. A pending record's `remember_error`
  is why that append fails, while it does.
  The key is `agent_name` because `agent` is the upsert's record in the
  same envelope. A branch that is not valid UTF-8, or has U+FFFD, is
  refused: the connection turns such a byte into U+FFFD, so the daemon
  cannot know the branch meant. Clients refuse it before they send, and
  on macOS git could not make its ref or root in any case.
- **`rm`** `{type: rm, id, repo, branch, root, force}` removes the worktree
  through git, which refuses a dirty or locked one without `force` and
  says why, then kills every managed session whose pane records the
  root. Both steps skip when already done, so a repeat is `ok`. `repo`
  names a repository the config lists or any checkout under `repos`, by
  source or by label; a label two repositories answer to is refused. With
  `root`, the checkout that registers the root is the one git removes
  from, and a repository with no checkout here still reaches the
  session by the root. Only a
  worktree under `worktrees/` is removed, by branch or by root; one the
  user made elsewhere is left alone, as is the branch. Send `root` from
  the record whenever it is known: it is what reaches a session whose
  worktree is already gone, since a branch alone maps to no root then.
  Without `root`, a `branch` that is not valid UTF-8 or has U+FFFD is
  refused, as add refuses it, and so is one with a `\`, which git takes
  in no branch: it can only be a record's quoted form, which names its
  worktree with the root alone. With `root`, `branch` must be the
  root's: its name, or for a record with `branch_display_only`, the
  quoted form the record carries.
  From a daemon with `prune`, `rm` also takes `head`, the commit the
  worktree's HEAD must be at: one at another commit is refused before
  git is asked, so a commit made since the client read the worktree
  keeps it. A root whose directory is gone has nothing to lose and
  passes. With `unused`, it is refused when something runs in the
  worktree, checked under the same locks as the removal: an add at the
  root with no outcome yet in the journal, a run, or a pane of any
  watched server, each listed then rather than as the last poll saw it,
  that laatmux made at the root, as `rm` kills it, or whose path is
  under it, as tmux reports it or resolved by the daemon's resolver,
  which never waits on a hung mount; a server whose panes cannot be
  listed within ten seconds refuses it too, one not running has none.
  Before it looks, `rm` closes the root until it is done: the root's
  removal generation is bumped, so a run that resolved before is
  refused with `worktree removed; retry`, also when the removal then
  does not happen; no run registers in a closed root (`worktree being
  removed; retry`); and `new`, which makes a session in a worktree at
  its directory with the links resolved, refuses one in a closed root,
  and kills one it made while a root of it was closed or removed, by
  the removal generation, also one reopened since, or once its
  directory went, which tmux would otherwise start in the home
  directory; a session whose pane went into another session meanwhile
  is left, and said. An add's session is there by the look, as `rm`
  waits for every add in flight. A session elsewhere, and one in
  another worktree, is made as ever, without waiting. With `delete_branch` too, once this `rm` has had git remove
  the worktree, the branch is deleted when it is still at `head`, with
  its reflog and its config, as `git branch -D` deletes them; the
  deletion is `git update-ref --no-deref -d` with `head` as the old
  value, so a commit made on the branch in between is refused rather
  than deleted. A branch at another commit, one made a symbolic ref,
  or one a worktree has checked out, stays. That is a
  progress line of stage `branch`, `done` or `skip` with why, and the
  result stays the removal's. An `rm` that removes no worktree, one
  already gone when it runs, leaves the branch, since which clone it
  was in is not known.
- **`facts`** `{type: facts, id, roots}`, capability `prune`, answers
  `{type: result, id, ok, facts}` with one record per root, in order:
  `root`; `branch` and `head` as git has them at the root, the branch
  empty when detached; `changed`, the paths git status lists as the git
  status refresh reads it, untracked ones included and ignored ones not,
  0 when clean; `ignored` and `ignored_dirs`, the ignored files and
  directories git status lists with `--ignored=matching`, a directory
  counted once whatever is in it, which a removal deletes with the
  worktree; `locked` with `lock_reason`, and `submodules`, which git
  worktree remove refuses without force, read as git checks them;
  `base`, the repository's default branch, `origin/HEAD`'s branch, else
  `origin/main`, `origin/master`, `main`, `master`, the first that is a
  commit, and `ahead`, the commits HEAD has that it does not;
  `on_origin`, that `refs/remotes/origin/<branch>` is there as the last
  fetch left it, and `pushed`, that HEAD is in it; `in_use`, what
  `unused` would find running there, an add with no outcome yet that
  no client resends among it, without the removal's locks, so the plan
  says it. Unlike the git status
  object's base, a branch's `laatmux-base` key does not count: what a
  branch was made from need not be where its work lands. Only a root
  git lists as a worktree under `worktrees/` is read, a main checkout's
  not; any other has `error`, as does a root whose read failed. The
  listing is the worktree listing's; every read after it is
  `--no-optional-locks` with the refresh's timeout, and none goes to the
  network. Four roots are read at once, those of every connection
  together, and the reads end with the connection that asked. The
  index is read for submodules only in a worktree with a
  `.gitmodules`: one nested without it, which git refuses to remove
  all the same, is a removal that fails.
- **`run`** `{type: run, id, repo, branch, root, cmd}`, capability `run`,
  runs `cmd` as a subprocess of the daemon in `root`, which must be a
  registered worktree of a known repository under `worktrees/` and, when
  `repo` and `branch` are given, theirs, `branch` as `rm` takes it with a
  `root`. No shell, no tty, stdin at
  `/dev/null`, the daemon's environment, its own process group. When
  the process exits, whatever it left in its group is laatmux's own and
  is stopped the way a cancel stops it, so a background child of a run
  never outlives the result and `rm` never meets one; the process
  exiting while such a child holds the pipes costs a second before
  that. Output
  streams as `{type: progress, id, n, stage: run, state: output, fd,
  detail}` one line per message, `fd` 1 or 2, a partial last line at
  exit; the result is `ok` with `exit` when the process exited at all,
  `ok: false` with `error` for laatmux's own failures. The two streams
  are read as two pipes, so the order between a stdout line and a
  stderr line is not kept, as with any pipe pair; within one it is.
  Output is text: binary is mangled by the line split, and a line past
  64 KiB is cut there with `...` appended, as setup output is, so one
  very long JSON line arrives cut. `{type: cancel,
  id}` sends `SIGTERM` to the process group, `SIGKILL` five seconds
  later unless every member of the group has gone, and the result says
  `cancelled` once it has; the group is watched, not the child, since a
  descendant that ignores the signal outlives its parent. A follower
  whose connection ends is let go at once, not at the next event. A cancel that
  lands before the process has started means it never starts. A clean
  daemon shutdown, on a signal or a failure, closes the registry so no
  run starts after it, cancels its runs the same way and waits for
  them. Runs take no
  repository lock. `rm`, once git has removed the worktree and before it
  kills the sessions, cancels every run in that root and waits, so its
  `ok` means nothing of laatmux's is left there. The two interlock on a
  removal generation per root: a run reads it before it asks git, and
  registers only if it is unchanged; `rm` bumps it under the same mutex
  it takes the root's runs under, so a run that resolved before the
  removal is refused with `worktree removed; retry` whether or not a
  worktree is back at that root.
- **Follow and numbered progress**, capability `follow`: every progress
  message carries `n`, from 1 per command, and a client that lost its
  connection sends `{type: follow, id, after}` in place of the command;
  the daemon replays from `after + 1` and keeps sending. An unknown id
  gets `{type: result, ok: false, error: "unknown command"}`; `add` and
  `rm` clients then resend the command as a new execution, `run` reports
  the outcome unknown. Retention differs per command: `add` and `rm`
  drop output past 1 MiB for good after one line saying so; a run keeps
  streaming past it and forgets its oldest lines for replay, and a
  follow from before the retained tail gets one `{state: gap, n,
  detail: "<count> lines dropped"}` numbered as the last dropped line.
  The ring is the one buffer, so a connected follower that falls more
  than the budget behind, on a slow link, gets a gap the same way: the
  process is never stalled by a reader, as a slow subscriber of the
  status stream is dropped rather than throttling the daemon. Each line
  is charged its bytes plus 64 for the message around it, so a stream
  of empty lines is bounded too. Every command with progress needs the
  capability: a client refuses a daemon without it before sending.
- **Retry and serialization**: commands run under the daemon's context
  and outlive the connection that sent them. Ids are kept for five
  minutes. `add` is serialized per repository, the forms of one source
  sharing a lock, so adds for different repositories run in parallel;
  `rm` holds every repository while it resolves and removes, listed or
  not, since its root checks ask every checkout, and so waits for any add
  in flight; adds that arrive while it waits wait for it. Two sources
  sent under one name also share the name's lock, so they never clone
  into one directory at once.
- **Tasks**, capability `task`, the host's side of
  [milestone four](docs/milestone-four.md): a command journal, one file
  per add under `<state>/commands/`, for the two decisions a retry
  cannot inspect. `add` takes `prompt`, `generated` and `submitted_at`.
  The journal is read before anything is done for an id it has seen: a
  terminal entry is answered with its recorded result and nothing runs,
  one the daemon died in is resumed, the steps by inspection, the
  allocation and the launch by the record, and `follow` for an id the
  memory has let go is answered from it: the result, `removed` after an
  `rm`, or `interrupted` with the stage reached, on which the client
  resends the add under the same id. An id the journal never saw is
  `unknown command` as before. An add whose `submitted_at` is more than
  a day in the host's future or more than thirty days in its past is
  refused as `submission expired`; entries are swept thirty days after
  they became terminal. The client's side of the contract is in one
  send path: an add is neither sent nor resent more than seven days
  after its submission.
  With `generated`, `branch` is a proposal and an `allocate` stage after
  `fetch` picks the first free of `<name>`, `<name>-2`, `<name>-3`
  against the local and remote branches, the registered worktrees and
  the names other unfinished entries hold, writes it to the journal
  before the branch is made, and reports `{stage: allocate, state, detail,
  branch, root}`; a given branch gets the same line as `skip`. The
  result carries `branch`.
  The agent stage is journaled as `launching` before `new-session` and
  `launched` after, with the pane and the tmux server instance. The
  prompt reaches the agent on the argv when the `cmd` has `{prompt}`,
  and `launched` is then `delivered`; otherwise it is typed in once the
  pane is ready: a fresh observation by the detector, after the startup
  grace, with the prompt box on screen (`VisibleIdle`, not the idle
  fallback), a verified live agent identified in the pane, the pane and
  server instance the journal names. That identity is bound into the
  journal before the paste and required by every later one. The paste
  is `load-buffer` from stdin into a buffer named for the attempt,
  `paste-buffer -p`, `send-keys Enter`, `delete-buffer`; a daemon that
  starts deletes every `laatmux-attempt-*` buffer. A pane not ready
  within a minute gets nothing. Claude Code asks, the first time it
  starts in a folder, whether the folder is trusted, and takes neither
  a typed prompt nor one on its command line until it is answered;
  every worktree can be such a folder. After a launch in a root under
  the host's `worktrees` directory, and again after a restart that
  finds the typed prompt not yet delivered, the daemon watches the pane
  for that question, for up to the same minute. It answers only while
  the pane is still in the session and on the tmux server instance it
  was launched in, with a verified Claude identified in it, and only
  when the bottom of the screen is the whole question, naming exactly
  that root, with its two options, one cursor, and nothing after the
  footer. Each key, two at most, is pressed under the root's
  delivery lock on a capture made under it: the cursor moved onto `Yes,
  I trust this folder`, then Enter with it there. Text that does not
  match is left alone, and the wait times out as before; `StopRuns`
  cancels the watchers. The result carries `prompt`, the
  delivery state: `none` (no prompt), `delivered`, `not delivered` with
  the reason in `error` on an ok result (pane never ready, paste refused
  before it began, a managed session already in the root: `session
  existed`, a launch that failed before anything started), or `unknown`
  (a daemon death between `launching` and `launched` or during a paste,
  `new-session` failing after the session may have been made, Enter
  refused after the paste). Nothing is inferred from a session's
  existence, and no daemon delivers again on its own. The prompt is
  never in the journal, a progress line, or a tmux error: the agent
  stage's `start` line names the command with the placeholder in it,
  and an error that echoes the command line has the prompt replaced.
  `{type: prompt, id, attempt, prompt}` delivers a pending prompt later,
  as attempt `attempt` from 1 in order: the journal serializes attempts
  per add, answers a repeat of a number with its recorded outcome rather
  than pasting again, checks the target is still the recorded pane on
  the recorded server with the bound agent (`not delivered: session
  replaced` otherwise), and for an entry without a target adopts the
  managed session in the root when it is the only one and its pane has
  a verified agent (`not delivered: no agent to deliver to` otherwise).
  An attempt out of order is refused with `next_attempt`, the number
  the journal expects, on the result; the relay takes its number from
  that rather than from the words. `follow` with `attempt` reattaches
  to one in flight or answers from the record; an attempt the journal
  never saw is `unknown attempt`, on which the client resends; an id
  the journal no longer holds is `recovery expired`. `laatmux add -p`
  in the foreground prints the delivery state as its last line and
  exits 0 when the add succeeded, whatever the delivery.
  Listings are stamped: the daemon counts observations owed in a
  `revision`, stepped at the end of every add that succeeded and by
  every `rm` that removed a worktree; a poll reads it before it asks
  git and publishes its listing stamped `{generation, revision}`, the
  generation being the daemon's start, in the snapshot and in an upsert
  of `listing` alone when it changes, with `listing_error` when the last
  listing failed. An add's result carries the barrier its mutation made;
  a listing at that revision or later in the same generation reflects
  it, and so does any successful listing of a later generation. From a
  daemon with `attribution` an `rm` that removed a worktree carries the
  stamp of its removal as `listing` in its result too, and the remove
  of a worktree carries `removed_in`, the stamp of the listing that
  found it gone; both date a removal against an add at the same root.

## Workspaces, client side

A workspace is one worktree, one managed agent session on its host and one
local session on the laptop. The local session lives in the user's default
tmux server, named `<host>/<repo>/<encoded branch>`, with one window
running the attach command (`env -u TMUX tmux -L laatmux attach` locally,
the same through `ssh -t` remotely). It carries `@laatmux_workspace` =
`<environment_id>/<root>`, the workspace key, `@laatmux_host`, and
`@laatmux_repo` and `@laatmux_branch`, the source and branch. The key of
a root with a control character, a byte that is not UTF-8, a U+2063 or a
`%` in it is stored as `<environment_id>%<encoded root>` instead, each
byte of those written as `%` and two hex digits, and read back decoded:
tmux 3.4 and 3.5 print such a byte back escaped, and a newline or the
separator laatmux reads tmux's listings with would split the session's
line. A key an earlier build stored, with the root as given after the
`/`, is read as it is, so it still finds its session where tmux gives it
back as written. The attach pane carries `@laatmux_attach_pane`,
`@laatmux_attach_target`, the managed session it attaches to, and
`remain-on-exit`. Sessions are
matched on the key, never the name, so a renamed host or repository label
still finds its session. The host tag is refreshed on every reuse, and the
source and branch whenever the reuse knows them: the worktree record
carries the source from the daemon, so `jump` never derives it from a
label that may mean another repository on this machine. The session is
created detached and tagged in one tmux command sequence, then the attach
pane is tagged by the id `new-session` printed, since the user's hooks may
split the window at once. The pane runs a placeholder until then, so it
cannot exit before `remain-on-exit failed` is set on it; the attach command
replaces the placeholder in the same sequence as the tags, and an attach
that fails, ssh refused or the managed session not there, leaves a dead
pane, with its message, for the next `jump` to respawn. A connected
attach ends with status 0 whether the managed session ended with its
agent or was killed under it, and then the pane closes and so the
workspace session; the next `jump` makes it again.

- **`add <branch>`** resolves the repository from `--repo`, a listed
  name or source, or a source in a forge form the config does not list,
  which is added to it (see the config above), else from the
  current directory: its git origin is matched against the known sources,
  since identity is the source and a checkout keeps its directory after a
  label change; an origin that is not configured is an error rather than
  a guess from the directory name; only a directory with no origin falls
  back to its place under the local host's `repos` or `worktrees`, the
  more specific first, where the next path component is the label. Host
  and agent come from
  their flags, else `last.json`, else for the agent `default_agent`, else
  the only candidate, else an error naming the candidates. The
  command id is chosen once per invocation; a transport failure mid-way
  dials again and follows the id from the last numbered progress seen.
  Progress prints one line per step. On success `last.json` is updated
  and the workspace session is created, or found by key; inside the
  default tmux server the client switches to it, elsewhere it prints how
  to attach, with the default server selected explicitly, and inside
  another tmux server it says to detach first.
- **`rm <repo>/<branch>`** sends the root along whenever it is known: from
  the host's record, or, when the worktree is already gone, from the key
  of the local session found by its source and branch tags; a session
  found by name is accepted only when it carries no identity tags at all,
  never when they name another workspace. Git's refusal of a dirty worktree comes back as the error
  with everything left in place; `--force` removes it. A branch only a
  main checkout has checked out is refused as the main checkout, with
  nothing sent; a worktree on it in another clone is found first. After an `ok` the
  local session with that key is killed, switching away first if it is the
  current one. `rm --root <path> --host h` removes a detached worktree.
- **`rm`** with no target, inside a workspace session, removes that
  workspace: the root from the session's key, on the host the session's
  tag names, or, when that tag names no configured host any more, the
  host that answers as the key's environment, as the dashboard routes
  a row; the host must answer as the environment the key names, since
  a host entry moved to another machine must not remove that machine's
  worktree at the same path, and every connection of the command is
  held to that. The host's worktree record for the root gives the
  repository and branch, which the session's tags can misname after a
  switch or a detach in the worktree; without a record the tags do,
  when this machine's config knows the source, else the root alone as
  `--root` does; a record of a main checkout, whose shell session a
  jump made, is refused as the main checkout with nothing sent. That is
  how the dashboard's `x` resolves a row, and what a binding needs:
  `bind-key W confirm-before -p "remove this workspace? (y/n)" "run-shell 'laatmux rm'"`.
- **`prune [--host h] [--repo r] [-n|--dry-run] [--branches] [--yes]`**
  removes the worktrees nothing uses whose work is in the repository's
  default branch. Their ignored files, a `.env` say, go with them, as
  with `rm`, and the plan line says how many. It reads the merged stream
  as `ls` does, and looks at the worktree lines `ls` shows with
  `no session` and nothing under them: no agent, no home session on the
  host, no pane and no run, not a main checkout and not a task's line,
  on hosts that are listed; a host down or still connecting is said and
  left. `--host` takes a host's name or its ssh alias; `--repo` a
  repository this machine's config lists, by name or source, else a
  host's label or a source a host lists a worktree of. A worktree with a
  detached HEAD, with a branch laatmux cannot carry, or with a local
  workspace session open (a shell over ssh in it is no pane the host
  sees) stays, and so does every worktree while what runs in it cannot
  be told from here: the local sessions not listed, or the records
  without the worktree each agent is in, from a host's daemon or a local
  one of a build before attribution. For the rest it sends `facts` to
  each host with `prune` and decides on the answer and this machine's
  branch records, the ones `{pr_state}` draws from: a clean worktree
  goes when it has no commit the default branch lacks, or when its
  branch's PR is merged into the default branch and HEAD is the PR's
  last commit, since a squash or rebase merge leaves the branch's
  commits ahead of the default branch, a commit made after the merge is
  in no PR, and a PR merged into another branch, the one under it in a
  stack say, has not put its work in the default branch. One the host
  finds in use stays with what uses it; a dirty one
  with the count of changed files; a locked one with its reason
  and one with submodules, which git removes only by force; one ahead
  without such a PR with the count, whether it is pushed and its PR;
  the default branch's own worktree; and every worktree of a host
  without `prune`. The plan is a line per worktree, `remove` or `keep`,
  with why; `-n` stops there, and without `--yes` the question is asked
  once, for the whole list, on a terminal: no terminal is an error
  naming `--yes`. Each removal is an `rm` with the root, the repository
  and branch, the environment the records are of, `head` from the facts
  and `unused`, so a worktree with a commit made since the plan, or with
  something started in it, a session or an add, stays; the local
  sessions are listed again before each, and one opened for the
  worktree since keeps it too. `--branches` sends `delete_branch` as
  well. A removal that does not happen is said and the rest go on; the
  command then fails with the count.
- **`path <repo>/<branch>`** prints the root from the host's records,
  or with no worktree on the branch the directory of the main checkout
  that has it checked out. Records are matched by source, since the
  host's label for a source may differ from this machine's. `rm` finds its record the same way. A
  branch that is not valid UTF-8 is refused before any daemon is asked,
  by `path`, `rm` and `run` alike, naming the quoted form a host lists a
  worktree on it under. That form, and a branch with U+FFFD, `ls` can
  show for one checked out by hand; neither is a name: they say so,
  with the worktree's root, which `rm --root` takes. A branch with
  U+FFFD that no record has is looked up like any other, and the daemon
  refuses an rm of it without a root.
- **`jump <host>/<repo>/<branch>`** switches to the workspace session,
  creating it from the record when missing, respawning a dead attach pane,
  and opening a new attach window when the pane is gone altogether. An
  attach pane on another managed session than the jump's, the worktree's
  agent having moved to another since, is restarted on the jump's; a
  pane from before the target was tagged is left as it is, and closing
  it has the next jump make a tagged one. A worktree with no managed
  session and no agent gets one first, asked of the host's daemon
  through `new` (capability `new`): named as `add` names the
  worktree's, the host's label and the encoded branch, at the root, with
  no command, so the host's `default-shell` runs in it; the jump prints
  what it made. `new` in a worktree needs the worktree's directory, and
  is refused while `prune` removes that worktree. A session of that name, which `new` refuses as a name in
  use, is attached instead, as one made since the listing by an `add`
  say, unless the host's records place it elsewhere: another worktree's
  home, which two clones' worktrees on one branch can be, or, with no
  record of the managed pane laatmux made at the root, a session an
  agent or a pane of another worktree runs in, or one outside the root;
  that is refused as `add` refuses it, `session <name> runs in <dir>,
  not <root>; name in use`. The view reads the records once `new` has
  answered, those it had at enter when the stream has no listing of the
  worktree's machine by then: the host down, listing again after its
  entry changed, or the entry reaching another machine. A daemon that
  answers as another machine than the worktree's is asked nothing. A
  host whose daemon lacks `new`, a branch only shown, a name
  tmux would not store as given, and a worktree whose agent is
  elsewhere keep the refusal that says how `add` makes one. The shell
  session is the worktree's home from then
  on: an agent started in it by hand is the worktree's, and `add` for
  the worktree refuses the session while no agent runs in it, saying so,
  rather than take it up and start none, on a host of this build. A
  managed session that is no worktree's, one `new` made, is reached the
  same way through a session named `<host>/<session>` tagged
  `@laatmux_attach`. A session name the host's tmux lists escaped, one
  made with a `\` or a tab in it, is decoded first, and in the local
  name each character the branch encoding encodes but `%`, `#`, `;` and
  `:` (a `\`, a control character, DEL, a byte that is not UTF-8, a `$`,
  a `.`) is written as `%` and two hex digits, so tmux keeps the local
  name as given: `a\b` gives `<host>/a%5cb`, and `a.b`, which tmux 3.7
  keeps, gives `<host>/a%2eb`. When that session later
  becomes a worktree's home, `new work --cwd <root>` say, or an older
  build left one named as the worktree's workspace would be, the
  worktree's jump adopts it as the workspace, keyed and tagged, rather
  than refuse its name as in use, also under the name an older build
  gave it before the `$` or `.` in it was encoded; an attachment to another
  managed session is still a name in use. The
  repository in the target is read as this
  machine's label first, then the host's, and the target after the host
  may also be the managed session's name, with the branch encoded, which
  is how a worktree detached in place is still reached. The local
  session's name follows the managed session's. `--server default` still
  switches to an observed session on this machine's tmux. A
  repository's main checkout, `<host>/<repo>/<branch>` for the branch
  it has checked out, gets no session from `add`. With a home session
  the jump goes to its workspace session as a worktree's does. With
  none it goes through its most recently active agent, one working or
  blocked first: to its session on this machine's default server, one
  on a remote host's default server refused as any session there is;
  or, for the agent laatmux made at the root of a home a split gone
  elsewhere took, through the workspace session attached to that
  agent's session, as a worktree's jump does then. With no agent either
  it makes the checkout a home as it makes a worktree with none one,
  through `new`, named as `add` would name a worktree's on the branch
  (`pin-chat/main`), with the host's `default-shell` at the checkout's
  root, and the workspace session keyed by the root, as any root's; a
  session of that name in use is attached or refused as for a worktree,
  and also refused when an agent in it runs in a pane laatmux made at
  another directory, below the root too. A
  host whose daemon lacks `new` says no agent runs there. The home is
  the checkout's from then on, and an agent started in it by hand is
  the checkout's (issue #376). A host on a build from before this has
  `new` but never ties the home to the checkout: each jump attaches to
  the session again, as one of that name in use, and an agent started
  there stays in other sessions. The home keeps the name it was made
  with when the checkout switches branch, as a worktree's does; an
  `add` of that branch then finds the name in use. Two clones of one
  repository on one branch name their checkouts' shell sessions alike,
  so the second's jump is refused as a name in use while the first's
  home is listed. Two clones' main checkouts on
  the branch, which no label tells apart, are named with their roots,
  and, on this machine, `--server default <host>/<session>` reaches an
  agent in any of them.
- **`ls`** prints the tree the sidebar's tree view shows, see below: a
  repository per line, its main checkouts in use and its worktrees with
  their host under it, and under each its agents with mark, state
  (`done` and `stale` among them), name, age and title, its other panes
  and its runs. A worktree with nothing under it shows `no agent` when
  its session has no identified agent and `no session` when it has
  none, a main checkout `no agent`; on a host that
  attributes panes the session's shell is a pane under the line, so
  those notes are for older hosts and worktrees with no pane at all. A
  settled worktree says `settled`, and a local
  workspace session whose worktree is gone from a connected host is
  marked `worktree gone`, from which `rm` still works; a host whose
  snapshot has not arrived, or whose daemon does not publish worktrees,
  says nothing about its workspaces. Agents in no worktree, observed
  ones naming their server, are under `other sessions`.
  `ls`, `watch`, `prune`, `jump`, `path`, `rm` and `run` read the local daemon's
  merged stream, see below, each starting the daemon when it is not
  running. `ls`, `watch` and `prune` fail when it cannot be started or is an
  older build without the stream, saying so; `jump`, `path`, `rm` and
  `run` then dial the host themselves, as they do for a host the
  daemon's config lacks.
- **`shell`** runs inside a workspace session and opens a window at the
  worktree root: started there for a local host, `ssh -t` with `cd` and
  the single-quoted root then `exec "$SHELL" -l` for a remote one. The
  window is tagged `@laatmux_shell`; a second call selects it. Meant to
  be bound in the user's tmux config, as
  `bind-key S run-shell 'laatmux shell'`: a `run-shell` job has `TMUX`
  naming the session the key was pressed in but no `TMUX_PANE`, and the
  session is resolved from either.
- **`split [-h|-v] [<pane-id>]`** is the split binding for every window:
  from the pane, passed in since a `run-shell` job has no `TMUX_PANE`, it
  reads the session's tags on the server `TMUX` names. Not a workspace
  session: `split-window -t <pane> -c '#{pane_current_path}'`, the plain
  split. A workspace on the local host: `-c <root>`. On a remote host:
  the ssh command `shell` builds. The new pane lands at the worktree
  root, not the split pane's directory, and split panes carry no tags:
  the session's decide, so a split of a split resolves the same way.
- **`run [<repo>/<branch>] [--host h] -- <cmd>...`** runs the command in
  the worktree root on its host: inside a workspace session that
  workspace, from the session's key and tags; elsewhere the record found
  as `path` finds it, a main checkout's refused. stdout lines go to
  stdout and stderr lines to stderr, so `laatmux run proj/x -- go test
  ./... | tail` behaves, and
  the exit status is the process's; 255 means the outcome is unknown, a
  lost connection whose follow found the daemon no longer knew the run;
  130 is cancelled. Ctrl-C sends `cancel` and waits for the result; a
  second Ctrl-C gives up waiting, and since the cancel went on the
  connection open at the time, and during a reconnect there was none,
  the message says the run may still be going. A client that just
  disconnects leaves the run going, as an `add` keeps going.
- **`settle`** and **`unsettle`** set and clear `@laatmux_settled` on the
  workspace session they run from, or the one named.

## Upgrading a host

`laatmux upgrade <host>...` puts this checkout's build on a host and
restarts its daemon. It asks the host for `uname -sm`, builds for that
platform with `CGO_ENABLED=0` and the version from `git describe`, once
per platform per run (`--bin` installs a binary built elsewhere, `--src`
names the checkout when the command runs from another directory; flags
go before, between or after the hosts), then streams the binary over the
same ssh alias the client uses into an `sh` script: written to a fresh
temporary name beside the configured `bin`, checked to answer
`version` as laatmux does, one line with the protocol and nothing after
it, exit 0, which a build for the wrong platform, a truncated copy or
an empty file fails (an empty file would run as a shell script that
succeeds) and then leaves the working binary as it was, and renamed
over it, so the install is atomic, the running daemon keeps its own
inode and two installs at once do not share a file; then `laatmux stop`
with the new binary. The `bin` is the same shell word the bridge runs:
a path under `~` is the remote home, anything else one quoted word, a
bare name found on the remote PATH, so a bare name cannot be
reinstalled once the binary is gone, there being no path to put it at;
set `bin` to a path for that. `stop` reaches the daemon over its
socket, without starting one, and sends `shutdown`, capability
`shutdown`, which the daemon answers and then exits on as it does on
`SIGTERM`, cancelling its runs and waiting for them; the process that
ends is the one that answered the hello, never a pid a file remembers,
which a crash can leave for another process to inherit. The runtime
record is read once and its address dialled, and the daemon's hello
carries its pid, so a replacement that took the same socket path
between the read and the dial is seen and the record read again. A
daemon from before the message has no pid in its hello and gets
`SIGTERM` at the record's pid when the record still stands. A daemon
that holds the startup lock but answers on no socket is starting or
shutting down, and `stop` keeps trying to reach it while that holder has
the lock. Then `stop` waits for the lock to leave that daemon's hands,
released by the kernel when it exits, reaped or not, or taken by a
replacement a client started meanwhile. The next connection starts the new build, and
`upgrade` makes that connection last and prints the version. Nothing
before that connection needs a daemon on the host, so a host whose
daemon is stopped or whose binary is gone is upgraded too; the script
says what the old binary was. A paused host is upgraded too, the one
command that dials it, and upgrade says it connects it; it stays
paused for everything else. Each remote step is bounded, so a host
that stops answering is reported and skipped rather than holding the
others. The local host is upgraded in place of the running executable
the same way. `hosts` marks every
daemon whose build is not this client's, since versions are `git
describe` strings, equal or not, never ordered.

## Sidebar and dashboard

The sidebar and the dashboard have two views over one join, built in
`internal/rows` (milestone five, step 6), with a tab line above the
list that `Tab` or a click switches:

- **Agents**, the default: the relay's tasks first, then one tile per
  agent in sort order. The primary label is the worktree's branch, the
  secondary the repository and host; an agent in no worktree is
  labelled by its session, and agents that share a label are numbered
  `(1)`, `(2)` in the tree's order. Shells, runs and worktrees without
  an agent are not here. Stale agents and those of settled workspaces,
  unless blocked or done or the viewer's own, fold into `▸ N stale` at
  the end, a row of its own that `Enter` or `s` opens.
- **Tree**: one node per repository, by this machine's label for the
  source, else the host's; its worktrees under it by branch, the host in
  parentheses, with the git stats and the PR on the line, and before the
  name `⎇`, a worktree, or `⌂`, the main checkout (`{kind_icon}`,
  below); under each its agents by start time, its other panes
  (`$ zsh`) and its runs (`▶ make test 0:42`), from the pane and run
  records the client now keeps.
  A main checkout in use is a line under its repository like a
  worktree's, before them, with its git stats and its agents in it, in
  plain sessions on its host's default server and in its home, and the
  workspace session keyed by its root as its own. The line shows its
  most recently active agent wherever that runs. With a home it is a
  worktree's line with one, `Enter` going there; with none, `Enter`
  goes through the most recently active agent, the line is the
  viewer's when the viewer sits in the session of any agent in a plain
  session, and with no agent either `Enter` makes it a home with a
  shell (below). With the home lost to a split gone elsewhere, the
  session of the agent laatmux made at its root stays the line's,
  whatever its name, and so does one named as that shell session is,
  as a session named after a worktree stays the worktree's, unless an
  agent in it runs in a pane laatmux made at another directory, below
  the root too. Its agents'
  tiles are titled by the repository, with the branch under it.
  A pending task sits where its worktree will be, holding the worktree's
  children while it stands for it, the newest of several owning them.
  An orphaned session sits under its repository by its source tag,
  marked `worktree gone`, else in `other sessions`, the last group,
  with agents in no worktree. Repositories and worktrees fold: a
  worktree's fold is decided the first time it is shown holding a
  child, open when an agent in it is blocked, working or done, and
  stays as the user leaves it; a folded line shows its most pressing
  agent's icon. The repository line of the node at the top stays pinned
  while the list scrolls. Empty, the views say `No agents running` and
  `No worktrees`.

`laatmux ls` prints the tree. The selection follows a switch: an agent
to its tile or node, a worktree line or what is under it to the
worktree's first agent, a repository line to its first worktree's, a
task to itself, opening the folds over the target; a target the view
has none of leaves the selection on no row. A following selection keeps
following: in the tree the first row in the tree's order that is the
viewer's by its own session, a worktree, task or orphaned session's
line before an agent in other sessions, usually the viewer's worktree
line; another worktree's line that is the viewer's only through one of
its agents in the viewer's session, started there from a split after a
`cd` into that worktree say, is followed only when no row is the
viewer's by its own session; in the agent view the
first of the viewer's tiles of what the tree follows, that line's
worktree's agents and tasks or the tile of the agent the tree follows
in other sessions, and of a line followed through a visitor the
visitor's; when none of those is shown, the first of the viewer's
tiles in the viewer's session, else the first of the viewer's tiles.
A tile in other sessions that sorts first is not
followed for that reason. Such a tile can be the viewer's beside the
worktree's own, an agent started outside any worktree from a split of
the viewer's session, say, and `o`, `O`, `x` and `a` on it would find
no worktree.
Local sessions are joined in by key, or by the attach tag for a `new`
session's attachment, so a row knows its local session, whether it is
settled, and whether it is the one the viewer is in. An idle agent that went from working to idle since a tmux client of
this machine last showed it is *done*; one idle for longer than
`sidebar.stale_after`, an hour by default, is *stale* (milestone five,
step 3). A done or blocked agent is never stale. A row is dim when it
has no identified agent, its agent is gone, its host is down, it is
orphaned, its workspace is settled and its agent does not want the
user, or it is stale and `sidebar.dim_stale` is not false. The order,
`sidebar.sort: priority`, is pending tasks, blocked, done, working, idle
and unknown, stale or settled, then gone agents whatever they last did,
most recent activity first within a group; an idle agent whose branch's
PR has checks pending ranks with the working ones for the first hour
since they went pending, since waiting on CI is work in flight, and as
idle after that, a job that never finishes not holding the row up; `recency` is most recent
activity first, and `window` by session and window, tasks first in
both. Stale agents, unless `sidebar.collapse_stale` is false, and
settled workspaces' agents fold into `▸ N stale` at the end of the
agent view; a settled workspace's live blocked or done agent stays in
place, and so does the viewer's own row, whatever it is. Orphaned
sessions are lines of the tree.

**Done and seen.** The local daemon keeps, per agent and its process
identity, the last activity it saw and two times on this machine's
clock: when the agent finished, and when a client last showed it after
that; a host's clock is never compared with it. It tracks its own agents
from its poll and a remote host's from its upserts and snapshots while
it follows the host, so a finish while the laptop slept is found in the
host's next snapshot against the state kept in `attention.json` under
`$LAATMUX_HOME`. A client shows an agent through a live attach pane to
the agent's managed session on the agent's host, or as the agent's own
pane on this machine's default server; a focused sidebar pane stands for
the pane beside it. The daemon lists the clients once a second while an
agent is done and a view is open or closed less than a minute ago, at
once when an agent finishes, and at once on `laatmux sidebar seen`,
which the sidebar's hooks on `client-session-changed`,
`session-window-changed` and `window-pane-changed` run. An agent on a
remote host's default server or another observed server is never done:
no view can take the user there.

The view is a tmux pane's worth of terminal, no TUI library: raw mode
through termios, the alternate screen and the SGR mouse reports decoded
into keys, clicks and the wheel are `internal/term`; ANSI for cursor,
colours and attributes and the renderer are `internal/view`. The renderer
is a function from rows, size and selection to lines that name their
colours from a palette, keeping what the next frame, click or refresh
needs, and is tested against golden files for every layout; only the
terminal
encoding looks the colours up
([milestone five](docs/milestone-five.md), step 2). `tiles` is three
lines per row and a divider: the status icon, the primary label (the
branch; the repository on `main` or `master`; a session's name for a
row that is no worktree's) and the time since the status changed, `m:ss`
under an hour, then `Nh`, then `Nd`, against the right edge; the
secondary label, the repository, with the host tag, dim for every host
but the local one; and the pane title, cleaned of spinner and status
characters and dropped when it only repeats a label, a shell's name or
the host, or what the row is instead, `no session`, `no agent`, `no
worktree`, a task's state. A stripe `▌` runs down the left of every
line in the status colour. The viewer's own row has its primary label
in bold `current_worktree_fg`. `compact` is the first line with the
secondary label and host tag after the primary, and in the dashboard
the third line under it. The icon is the status's: a two-cell braille
spinner at 250 ms for working, 💬 for blocked or a task that needs the
user, ✅ for done, 💤 for stale or a settled workspace's agent; `icons: nerdfont` and `icons: ascii` choose other sets, and
`status_icons` sets single ones. The set also has the glyphs a tree
line says what it is by, `⎇` a worktree and `⌂` the main checkout,
nf-oct-git_branch (U+F418) and nf-fa-home (U+F015) in nerdfont, `+` and
`=` in ascii; `kind_icons` sets single ones. Plain text keeps the
terminal's own foreground. `theme.mode: auto` asks the terminal for its
background with OSC 11 when the view starts, then reads `COLORFGBG`;
with neither, a tmux popup or a sidebar pane started by a hook say,
it takes the dark defaults. The selection is a band in
`highlight_row_bg` with the text in `text` either way: the band sets
both its colours, so it reads whatever the background is, and every
pane draws it the same. With `NO_COLOR` set, the attributes alone,
the selection reverse video. `theme.custom` sets palette colours. Before the first snapshot the list
says `Loading`, an empty one says so, and rows below the window are
counted on its last line, `↓ N more`. Keys in both: `j` `k` and arrows
move, `g` `G` first and last, `Tab` switches the view, `Enter` jumps,
on a repository line or the stale fold folds, `1`..`9` jump to the nth
tile of the agent view or the nth worktree line of the tree, `s` folds
and unfolds the selected line, `h` and `Left` fold it or go from a
child to its line, `l` and `Right` unfold, `f` opens every fold when
any is closed, else closes every one (the stale fold in the agent view),
`v` toggles the layout, `/` filters by name or host and `Esc` clears
it (with no filter, `Esc` closes the dashboard as `q` does; a sidebar
pane ignores it),
`z` settles or unsettles the selected workspace, `q` quits. A click
jumps to the row under it, on a fold mark or a repository line folds;
the wheel moves the selection. Hosts that
are not connected and listed, and a local daemon that is down, are
lines above the list, but for a paused host. Below the list, above the
footer, the hosts line has an entry for each host the config reaches
over ssh: `[x] vm` when the host is connected, connecting or down, the
name in danger when down, a warning while connecting and plain once it
is connected and listed, and `[ ] vm` dimmed when it is paused, whose
rows are gone until it is resumed. The entries wrap onto a second row
where the width is short. The line is drawn from the config the view
follows, so a pause made elsewhere shows within two seconds. A click on
an entry pauses the host, or resumes it; `H` opens the picker of the
same hosts, where `Enter` does (see the config above). The write, or
its refusal, a config file that does not parse say, is said in the
footer.

A jump from a tile, or from an agent or a pane in the tree, goes to the
pane, routed by the pane's server and session: a pane in a managed
session through the workspace session, or the plain attachment to
another managed session, then the host daemon's `select` command
(capability `select`), which runs `select-window` and `select-pane` on
its managed server, and the workspace session's attach pane made current
here; a pane on this machine's default server by `switch-client` and
the same selection locally; a pane on a remote host's default server is
refused as `jump` refuses it. A run's line jumps to its worktree's
session. `x` and `X` on a tile or an agent remove the agent's worktree,
the question saying how many agents go with it, and say what they
remove on a pane, a run, a repository line or the stale fold; on a
main checkout's line or one of its agents they refuse it as the main
checkout, and `z` and `S` act on its workspace session as on a
worktree's, or say it has none; `a`
preselects the repository and host of the selected row's worktree, a
main checkout's without its branch, and a repository line's repository
when this machine knows its source. The
host daemon leaves laatmux's own panes, the sidebar
panes and the attach panes of workspace sessions, out of its pane
records by their tags.

Jump from the view is `jump`'s logic in-process against the merged
records: a workspace row switches to its local session, creating it from
the record when missing; a `new` session's row does the same through a
plain attachment; an observed agent on this machine's default server is
a `switch-client`; one on a remote host's default server is refused with
`jump`'s message; a worktree or a main checkout with no session and no
agent gets a managed session with a shell at its root as `jump` makes
one, on a host whose daemon has `new` by the capabilities the merged
stream has cached for it. The view does not wait on the host: the
footer says `making a session on <host>…`, a jump or an `S` meanwhile
is refused with it, and when the host answers the jump ends as any
does, the footer saying what was made and the dashboard closing. `S`
on such a line makes the session the same way, then opens the shell
window at the root in the workspace session and switches there (issue
#373). A user who has moved on meanwhile, to a form or a question in
the view, or with the client to another session, is left there: the
footer says the session is there and enter on the line goes there, or
`S` opens the shell there. A click and a digit are jumps, so they make
the session too. On another host, one that is down among them, and for
a detached worktree or a branch only shown, the footer says how `add`
would start one: its command line, or what add needs first, and `S`
says the row is no workspace. `z` on such a line says which: that
enter creates one with a shell and the `add` line makes one with an
agent, or the refusal. A main checkout goes as `jump` takes it: with
no home, through its most recently active agent; with neither, where the
host cannot make the home, the footer says no agent runs there, and
`z` adds no `add` line, since add makes a main checkout none. An
orphaned row's session exists locally and is switched to.

- **`sidebar [toggle|on|off]`**, meant for a key binding. `on` sets
  eight server hooks at indexes laatmux owns, `after-new-window[9101]`
  and `after-new-session[9102]` running `sidebar attach '#{window_id}'
  '#{session_id}'`,
  `pane-exited[9103]` and `after-kill-pane[9104]` running `sidebar reap`,
  `window-resized[9105]` running `sidebar fit '#{window_id}'`, and
  `client-session-changed[9106]`, `session-window-changed[9107]` and
  `window-pane-changed[9108]` running `sidebar seen`, then walks every window on the default server and splits a pane off
  the left edge of each that has none, full height, at the configured
  width, running `sidebar pane`. The split is detached, so focus stays
  where it was, and the pane is tagged `@laatmux_sidebar` by the id
  `split-window -P` printed, never as the window's active pane: an
  `after-split-window` hook of the user's runs between the commands of a
  sequence and may select or split another pane. Every check-and-create
  runs under an exclusive flock on `$LAATMUX_HOME/sidebar.lock`, held
  from the check to the tag, so no `attach` sees the pane untagged; and
  `attach` reads the hooks under it and does nothing when they are gone,
  so an attach queued behind `off` puts no pane back. `reap` kills a sidebar pane that is alone in its
  window, counting a dead pane kept by `remain-on-exit`, such as a
  workspace's attach pane, as the window's. `fit` puts a window's
  sidebar back to the configured width: tmux shares a window's change of
  width out among its panes, so a session made detached, 80 columns
  wide, would otherwise widen its sidebar by a share of the terminal
  once a client switches to it, and so would resizing the terminal. In
  a window narrower than twice the width the sidebar gets half, when it
  is split off as when it is fitted. The width is the
  sidebar's own, set in the config: a border dragged by hand is put
  back at the next resize of the window. A zoomed window stays zoomed. A sidebar turned on by an older build gets the
  hook when `on` runs again. `off` unsets the hooks and kills every tagged pane. `toggle` reads the hooks. The hook
  commands name the binary by its absolute path. `q` in a sidebar pane
  closes it; that window has no sidebar until a new window is made or
  `on` runs again.
- **`sidebar pane`** is one client of the local daemon's merged stream,
  in the configured layout, marking the session it sits in from
  `TMUX_PANE`, redrawing on every change and every five seconds for the
  ages, staying after a jump. It reads no local sessions itself: settled
  and orphaned come from the stream. `q` and `Ctrl-C` ask `Quit
  sidebar? y/n` first, since a key meant for another pane is common;
  while filtering `q` is a letter of the filter and `Ctrl-C` asks; `?`
  lists the keys.
- **Placement** (milestone five, step 8): `sidebar.position: top` puts
  the sidebar along the top of the window instead, `sidebar.height`
  lines (3) of chips `sidebar.horizontal.item_width` wide (24),
  separated by ` │ `, each the `top` template's lines as far as the
  height allows; the strip scrolls sideways to keep the selection in
  view and counts the chips past the edge, `→8`; `h`, `l` and the
  arrows move through them, a click lands on one, and it shows the
  agent view alone, with no host lines above it and no hosts line.
  `sidebar.width` takes columns or `N%` of the window; unset it is 10%
  of the window clamped to 25..50 columns, an explicit width is not
  clamped, and either is halved in a window narrower than twice it.
- **Scope** is what a pane shows, by the viewer's row, the one
  following picks: `all`, every row; `session`, the viewer's worktree,
  every agent of it whatever session each runs in and every task at
  its root, in the tree its line or the task standing for it with its
  children under its repository, and with no worktree the viewer's
  line alone; `project`, every line under the viewer's worktree's
  repository. `session` and `project` also keep every row that is the
  viewer's, whatever its worktree: an agent started outside any
  worktree from a split of the viewer's session, or observed in a
  window of the viewer's session, which stands in other sessions; and
  another worktree's line with its children, such as the line of an
  agent observed in a window of the viewer's session with its
  directory in that worktree, or of one started from a split of the
  viewer's session after a `cd` into that worktree. Such a line is not
  the viewer's row while a line is the viewer's by its own session:
  `session` is still that line's worktree and `project` its
  repository, whichever sorts first. A pane in a session that is no
  row's shows the empty state under `session` and `project`. `F`
  switches the pane to
  `session` and, pressed again, back to the scope the pane had before,
  whatever set it; on `session` already it goes to `all`. `F` acts on
  that pane alone and is not kept. The footer names a scope in force:
  `[session]`; a strip names it at its right end. `f` sets the folds of
  the lines the scope and the filter leave, not a repository line
  shared with the panes on `all`: folded, that line is a closed fold
  shown, and `f` opens it here alone, with the lines under it. The dashboard
  starts at `all`, whatever the file says: a scope the CLI set for the
  panes would empty a popup opened from an unrelated shell. `laatmux
  sidebar on --session` puts panes in the current session's windows
  only: the hooks stay global, since a hook on the session would
  shadow the user's global hooks of that name there, and `on
  --session` names the session in the server option
  `@laatmux_sidebar_sessions`, which `attach` reads for the windows the
  hooks report; a plain `on` clears the option, so every session gets
  panes again, and `off` unsets it. `on` with `jump_keys` off unbinds
  the jump keys an earlier `on` bound.
- **Control from the CLI:** `laatmux sidebar next | prev | jump N |
  view agents|tree | scope all|session|project [-t window] [-c
  client] [--all]` act on the sidebar pane in the window the command
  runs for, `-t` a window or the current one on the default server (a
  shell nested on the laatmux server has no current window there, and
  the command does nothing), or on every pane with `--all`, which
  `view` and `scope` take. Each pane listens on a unix
  socket under `$LAATMUX_HOME/sidebar/`, named by the server's pid and
  the pane id, and writes the path to the pane option
  `@laatmux_sidebar_socket`, which the CLI reads. The command is one
  line to the socket, handled as a navigation event, not as typed keys:
  it moves the selection or switches the view whether the pane is
  filtering or not, and is ignored while an overlay or a question is
  open. `jump N` is the digit key, switching the client `-c` names
  with `switch-client -c`. For `view` and `scope` the CLI writes the
  new default to `sidebar.json` once, whether or not a pane answered;
  the panes only apply it. A window with no sidebar pane, a leftover
  socket or a refused connection makes the command exit quietly, since
  a binding's error flashes in the status line; `sidebar reap` removes
  a socket whose pane is gone or that refuses. With
  `sidebar.jump_keys: true`, `on` binds `M-1`..`M-9` in tmux's root
  table to `sidebar jump N -t '#{window_id}' -c '#{client_name}'`, the
  `{jump_key}` token shows them in the sidebar panes, not in the
  dashboard, and `off` unbinds them; by default
  they are unbound, since bound there they take the keys from every
  pane.
- **`sidebar.json`** under the state directory keeps two kinds of
  thing. The view and layout last chosen by a key or the CLI and the
  scope last set by the CLI are *start defaults*: a pane reads them
  when it starts, over the config's (`sidebar.scope` among them), and
  `F` is never written; the layout and the scope of one pane never
  move another's, and `--all` is how to change every pane's. The view
  is followed between the sidebar panes: `Tab` in one switches every
  other to the tree or the agents, read with the folds, so the sidebar
  is one view whatever window shows it; the dashboard keeps its own.
  The folds the user toggled, the
  stale fold among them, are *shared*: every pane and the dashboard
  read them again when the file's mtime changes, checked every second,
  keeping the selection on its row, and a fold carried across a task's
  handoff is written under the node that took the children. Panes
  write the file read-modify-write under a lock file and replace it by
  rename, and each writes only the folds it set since it last wrote,
  so two panes toggling folds at once lose neither and a fold taken
  from another pane is never written back over that pane's later
  change. The dashboard keeps its own view and layout defaults in the
  same file, under `dashboard_view` and `dashboard_layout`: it opens
  in a wide popup where compact suits, `--layout` wins over the stored
  one, and neither `sidebar.view` nor the CLI touches them; a fold
  carried at a handoff is written only where the file has none, since
  every running pane carries the same value and one ahead may have
  changed it; a value the file held last time is not applied again, so
  a fold a pane opened to reveal a selection, its own and not written,
  stays open when an unrelated write comes round; and each pane
  refreshes its folds' sightings once an hour, so a node in sight for
  a day is not dropped by a write elsewhere. A fold is kept by node id
  with the time its node was last seen, and one not seen for a day is
  dropped. The strip's view and layout are its own, never written. The
  dashboard takes the view and, without `--layout`, the layout from the
  file.
- **`dashboard`** is the same view filling whatever it runs in, compact
  with titles by default, `--layout tiles` otherwise. A jump exits, so
  under `display-popup -E` the popup closes:
  `bind-key C-s display-popup -E -w 90% -h 80% -d '#{pane_current_path}' -T ' laatmux ' 'laatmux dashboard'`.
  `-d` matters: the form's repository defaults to the repository of
  the directory the popup runs in, which without it is the session's;
  opened from a workspace session the form is for that session's
  repository on its host, read from the session's tags, since the
  attach pane's directory says nothing of a worktree on another
  machine.
  Actions: `a` opens the task form of
  [milestone four](docs/milestone-four.md): chips for the repository,
  the host and the agent, preselecting what `add` would take, a prompt
  box, and a branch line filled from the prompt as it is typed until
  it is edited. `Tab` and `Shift-Tab` move between the fields; on a
  chip `Left` and `Right` cycle, `Enter` opens the picker and `Ctrl-J`
  submits; in the prompt `Enter` submits and `Ctrl-J` inserts a
  newline; on the branch line either submits. The prompt takes the
  readline chords within a line: `Ctrl-A` and `Ctrl-E` to its start and
  end, `Ctrl-B` and `Ctrl-F` a rune back and forward, `Ctrl-D` the rune
  under the cursor, `Ctrl-U` and `Ctrl-K` the text before and after it,
  `Ctrl-W` the word before it; on the branch line, and in the list's
  `/` filter, `Ctrl-U` clears and `Ctrl-W` takes the last word or
  segment. A click on a field focuses it. `Shift-Enter` and
  `Ctrl-Enter` are `Ctrl-J` where the terminal reports them as
  distinct keys, and `Enter` elsewhere: the view asks
  for xterm's modifyOtherKeys at level 1 and reads both the
  `CSI 27 ; m ; 13 ~` and the `CSI 13 ; m u` forms, which under tmux
  takes `extended-keys on` (the default is off) and the outer
  terminal's `extkeys` feature, for example
  `set -as terminal-features 'xterm*:extkeys'`, with either
  `extended-keys-format`; a terminal bound to send `ESC CR` for
  `Shift-Enter` gets a newline as well. `Esc` cancels,
  and an Alt chord, `Alt-Backspace` say, which the terminal sends as an
  escape and the key in one read, is not `Esc` and is dropped.
  Bracketed paste is on, so a pasted line break is a newline, never a
  submit, and a paste goes into the prompt wherever the focus is but
  the branch line; in a chip's open picker a paste goes into its filter.
  A repository's source pasted into the repository picker is that
  repository: a listed one in whatever form, else a new one, shown
  marked `new` and taken by `Enter` or a click even where its text is
  part of a listed source; the host and agent chips then take what a
  repository without a last use gets, the footer says the repository
  is added to the config's `repos` once its worktree is made, and the
  submit carries it as the add's `repo_entry` (see the config above).
  Each chip's picker reads the config again as it opens, and every
  chip takes it (see the config above).
  With no repository configured the form still opens, saying so, and a
  submit without a repository is refused. A paste whose bytes stop for a second is shown as
  far as it came, with its framing kept, and one whose end marker
  never comes is ended by `Esc` or `Ctrl-C` pressed alone after that
  and left for a second, the key spent on ending it. On a worktree row
  without a session the chips and the branch
  are pre-filled from the record, the branch explicit. A submit hands
  the add to the local daemon's relay and closes the popup on
  `accepted`; the task is a row in the views from then on, and in
  `laatmux tasks`; a refusal keeps the form up with the error, and an
  answer lost after the daemon may hold the task drops the form and
  keeps the view up with the id (in `compose`, an ended log, then the
  exit), so nothing is submitted twice. The relay is the only path: a
  daemon that cannot open its pending directory exits with the error
  before announcing itself rather than run without it, and the form's
  hosts, the dashboard and `compose`, and `tasks` refuse a daemon
  without the relay, an older build, as every client refuses one
  without the merged stream. The footer
  says `tasks not supported by <host>'s daemon` for a host whose cached
  capabilities lack `task`. `x` confirms then removes
  the worktree; a refusal that asks for force carries the hint to use
  `X`. On a pending task's row `x` dismisses it instead, with a confirm
  line, where it needs the user or would otherwise be stuck, and `p`
  delivers a prompt that did not reach the agent, or may not have.
  `z` settles or unsettles; `S` opens the shell window and jumps, on a
  worktree or a main checkout with no session and no agent making the
  session first as enter does (see the jump from the view below); `s`
  folds and unfolds the selected line.
  The commands are `internal/command`, the same implementations the
  CLI's `add`, `rm`, `run` and `shell` call, with the printing separated
  from the doing.
- **`compose`** is the form alone, for a binding from any window:
  `bind-key T display-popup -E -w 80% -h 60% -d '#{pane_current_path}' -T ' task ' 'laatmux compose'`.
  It exits on submit or cancel, the repository and host defaulting to
  the workspace session it was opened from, else the repository to the
  directory the popup was opened from.
- In both, a background add from the form is a row of its own until it
  hands over to its worktree row, and the agent it starts is on that
  row, not on a tile of its own: a tile at the top of the agent view,
  the newest first, and in the tree and `ls` a line under its
  repository where its worktree will be. Its mark spins while the add runs and is
  `!` once it needs the user, when the row is dim too. The second line
  in tiles, or the state column in compact, says where it is:
  `adding: <stage>`, `host unreachable, retrying`, `host <name> is
  paused`, `failed at <stage>`,
  `prompt not delivered`, `prompt delivery unknown`, `outcome unknown`,
  `done, awaiting the listing`, `done, worktree gone`, or `host
  removed`; the line under it, the title line in compact, has the
  detail or the reason. The worktree row at the same root is not drawn
  while a task for it stands, and the task's row takes its agent and
  session: `Enter` jumps once the add is done and does nothing while it
  runs, and refuses a task whose host is removed or now answers as
  another machine, or whose worktree is gone. A task that failed, or
  whose worktree was gone after the add, does not stand for the
  worktree row, so one made again at the root is drawn beside it. `p` and `x` on a task's row work in the sidebar as
  in the dashboard, the sidebar's only keys that act, with `H`.
- In both, a working row's mark spins: braille frames in cyan, one per
  tenth of a second from the clock, so every pane spins in step; the
  view redraws at that rate only while a working row is on the list. A
  working agent that is gone, or on a host that is down, keeps the
  plain `*`, as `ls` always prints it.
- In both, the selection follows the viewer's own row, the session the
  sidebar pane sits in or the popup was opened from, wherever the sort
  moves it, and rests on nothing when no row is that session, so `Enter`
  does nothing and a digit counts the tiles or the worktree lines as
  always. The first key or
  wheel step that moves the selection makes it the user's: from then on
  it stays on the row it was put on across refreshes, as before. A click
  on a row, or a digit, jumps to that row and leaves the selection
  following, the user's own selection included: back in this window it
  is on the viewer's own row again, not on the one clicked, and the
  window the viewer arrives at shows its own row, not wherever a click
  from it once left the band. In
  the sidebar, tmux's click binding makes the sidebar the active pane,
  so a click that jumps makes the pane that was active before it the
  active one again (`last-pane`), and typing goes where it went before
  the click, whether the jump left the session or stayed in it. That
  rests on tmux's default click binding, which selects the pane
  clicked; a sidebar focused with the keyboard before the click gives
  the focus to the pane active before it. A click that hits no row, or
  a click whose jump did not happen, on a task still running or one
  refused with a message, leaves the sidebar focused and selects the
  row clicked, so the message and `p` and `x` are about it. A
  task's row hands the selection to its worktree row when it hands
  over, through the handoffs the merged stream carries even when the
  view missed the steps between. A selected row that goes with
  nothing to hand over to leaves the selection on none, not on the row
  that took its place, until the row is back or a key moves it.
- Both refuse a local daemon without `merged` with what to do; a sidebar
  per window is the case the capability exists for. `watch` stays the
  plain scrolling list for a terminal that is not a tmux pane.

Config: `sidebar: {position: left, width: 40, height: 3, horizontal:
{item_width: 24}, layout: tiles, view: agents, scope: all, sort:
priority, dim_stale: true, collapse_stale: true, stale_after: 1h,
jump_keys: false}`; position `left` or `top`, width at least 10
columns or `1%` to `100%` (unset or 0: 10%, clamped to 25..50), layout `tiles` or `compact`, view `agents`
or `tree`, scope `all`, `session` or `project`, sort `priority`,
`recency` or `window`, stale_after a Go duration. The look is set at the top level: `icons: emoji|nerdfont|ascii`,
`status_icons: {working|waiting|done|stale: "…"}`, `kind_icons:
{worktree|main: "…"}` for the `{kind_icon}` token, `agent_icons:
{claude: {icon: CC, color: "#d97757"}}` for the `{agent_icon}` token,
and `theme: {mode: auto|dark|light, custom: {accent: "#b48ead"}}` with
the palette `info`, `accent`, `success`, `warning`, `danger`, `dimmed`,
`text`, `border`, `header`, `highlight_row_bg` and
`current_worktree_fg`, colours as `#rrggbb` or 0 to 255.

### Templates

Every line of every layout is a template, and the layouts above are
the defaults, `sidebar.templates` in the config:

```yaml
sidebar:
  templates:
    tiles:
      - "{stripe} {status_icon} {title} {pane_suffix}{fill}{elapsed}"
      - "{stripe}    {subtitle} @{host}{fill}{git_stats}"
      - "{stripe}    {pane_title}{fill}{pr_number} {pr_checks}"
    compact: "{stripe} {status_icon} {primary} {pane_suffix} {secondary} @{host}{fill}{git_stats} {elapsed}"
    top: "{status_icon} {title} {pane_suffix}"
    tree:
      repo: "#[fg=header,bold]{fold}{repo}"
      worktree: "{indent}{fold}{kind_icon} {primary} ({host}){fill}#[fg=warning]{status_label}#[default] {git_stats}  {pr_number} {pr_checks}  {worst_status}"
      agent: "{indent}{status_icon} {agent_label}  #[dim]{pane_title}"
      pane: "{indent}$ {command}"
      run: "{indent}▶ {command}{fill}{elapsed}"
```

The dashboard's defaults differ where it has the room a sidebar
seldom has: `{git_stats}  {git_sync}` on the second tile line and in
`compact`, `{pr_state} {pr_number} {pr_checks} {pr_detail}` on the
third, and both on the tree's worktree line before `{worst_status}`.
A line the config sets applies to the sidebar and the dashboard
alike; only the lines it leaves differ.

- **Tokens.** Labels: `{primary}`, `{secondary}` (the branch, then the
  repository, as a line reads them), `{title}`, `{subtitle}` (the
  repository, then the branch, as a tile reads them; a row that is no
  worktree's is titled by its session's name with no subtitle), `{branch}`, `{repo}`,
  `{host}` (dim for every host but this machine, `?` when no host
  claims the record, with `/server` on a tile or an agent line for an
  agent observed off the managed server), `{session}`, `{window}`
  (tmux's `session:index`), `{window_index}`, `{pane_title}` (the
  cleaned title, or on a tile what the row is instead: a task's state,
  `no session`, `claude gone`), `{pane_suffix}` (`(2)` on the second
  of a worktree's agents). Status: `{stripe}`, `{status_icon}`,
  `{status_label}` (`waiting`, `working`, `done`, `stale`, `settled`,
  `idle`, `gone`; `no agent` or `no session` on a tile without one; a
  task's state; `worktree gone`, dim, on an orphaned session's line), `{agent_icon}`, `{agent_label}`, `{elapsed}` (the
  time since the status changed; a run's running time). Git:
  `{git_stats}` (the whole `R +46 -11 ✎ +28 -3`), `{git_committed}`,
  `{git_uncommitted}`, `{git_ahead}` (`↑2`), `{git_behind}` (`↓1`),
  `{git_dirty}` (`✎`), `{git_conflict}` (`!`), `{git_rebase}` (`R`),
  `{git_branch}` (the base), `{git_sync}` (how the branch stands
  against its base: `→base` when the base is not `main`, `master` or
  the branch itself, its `origin/` taken off and at most twelve cells,
  the conflict mark `!`, `↑2 ↓1`; when narrow the base is cut, then
  goes, then `↓`, then `↑`, and the mark stays). PR: `{pr_number}` (an
  OSC 8 hyperlink to the PR, clickable where the terminal shows them;
  under tmux the outer terminal needs the `hyperlinks` feature, as
  `set -as terminal-features 'xterm*:hyperlinks'`, or tmux drops the
  link and the number is plain text), `{pr_checks}`, `{pr_state}` (the
  PR's state as an icon: `●` open in
  green, `◌` a draft dim, `◆` merged in purple, `⊘` closed in red, a
  draft closed as one being closed; `o` `d` `m` `c` in ascii, octicons
  in nerdfont), `{pr_detail}` (pending checks: the time since this
  machine first saw them pending, in purple, ticking under an hour,
  shown whole or dropped; failing: the first failing check's name in
  red, cut like a label; else nothing; on `main` and `master` only the
  failing name). A stale answer leaves them dim and plain, as a refresh
  that timed out leaves the git stats, sync, base, counts and marks;
  the pending time is then as of the last answer.
  Position: `{idx}` (the row's number, as the digits count),
  `{jump_key}` (`M-2`, with the jump keys on). Tree lines: `{indent}`
  (two cells a level), `{fold}` (`▾ `, `▸ `, or the space of one),
  `{repo_count}` on a repository line, `{child_count}` and
  `{worst_status}` (the most pressing agent's icon, on a folded line)
  on a worktree line, `{command}` on a pane or run line, `{kind_icon}`
  (the set's glyph for what the row's checkout is: `⎇` a worktree, a
  detached one too, `⌂` a main checkout, on its line and on the
  agents, panes, runs and tiles in it; nothing on a task's line or
  tile, while it stands for a worktree too, though the agents, panes
  and runs under it keep the worktree's, and nothing on an orphaned
  session's line or another session's; plain, so in the line's colour,
  dim on a dim line and not the viewer's colour, which stays on the
  label; an override in `kind_icons` is drawn whole or dropped, never
  cut). A token that has nothing on a row is empty.
- **`{fill}`** splits the line into a left and a right part, the right
  against the right edge. An empty token takes the adjacent run of
  spaces with it, the one after it, else the one before, so separators
  do not pile up; a line whose tokens are all empty is still a line, so
  tiles keep their height; an empty entry in `tiles` removes that line.
  An empty `compact` or tree template is the default: those rows keep a
  line, so they can be selected.
- **Overflow.** A line wider than the pane gives way in this order:
  the flexible tokens, the labels and the pane title on either side,
  are cut with `…` down to a third of the width (at most twelve
  cells), the rightmost first; `{git_stats}`, `{git_sync}` and
  `{pr_checks}` shrink themselves, the rightmost first, never to
  nothing; the fields on the right are dropped,
  the widest first and a folded line's icon last; the flexible tokens
  are cut further; the tokens on the left are dropped, the last first;
  then the line is clipped. A dropped token takes its separator, the
  literal before it (else the one after) up to a bracket, and a
  bracket pair around it alone, `({host})`, goes with it. What dropping leaves over
  goes back to the cut labels, then to the shrunk stats, sync and checks. A
  stale branch's `?` sits on `{pr_checks}` when they are drawn, else on
  `{pr_number}`: a number drawn on a stale row always has it, and of a
  one-digit number and the marked check, equally wide, the number goes
  first.
- **Styles** are tmux's: `#[fg=accent,bg=#112233,bold,dim]`, undone by
  `nobold`, `nodim` and `default`, with a palette name or a colour as
  the config writes them. A style holds until the next one and leaves
  a token's own colours alone. A dim token with no colour of its
  own, a remote host, a draft's number or state, or a token a stale
  answer leaves dim and plain, takes only the style's background:
  dim wins over the style's colour and bold, so the token reads dim
  whatever the template says. A dim token in a colour of its own, a
  closed PR's red or the committed counts' green and red, keeps it:
  the colour wins over dim, and the style applies as to any coloured
  token. The padding `{fill}` takes the style in force at the fill,
  and a background gives way to the selection's band and is not
  drawn on a dim row, in the list or the strip. Dim text with no
  colour on a style's background (a colourless dim token always,
  `#[dim]` text under a style without `fg`) is not faint in the
  terminal's colour, which can vanish on the background, but drawn in
  the dim colour that reads on it: of the theme's `dimmed`, the dark
  and light defaults' (`#565f89`, `#8990b3`) and the background itself
  half way to white and half way to black, the one with the most
  contrast against the background. So the dark default's on the
  lightest backgrounds and the light default's on the darkest, a
  `dimmed` of your own where it reads better than the rest, and the
  background's own lighter or darker half on those between, from about
  `colour235` up through a mid grey and the light theme's accents; on
  the last two neither default's `dimmed` reaches 2:1. An indexed
  background is measured by xterm's colours; on one of the colours 0
  to 15, which the terminal sets, the text stays faint.
- **Errors.** A template that does not parse is shown in the view in
  its place, `template error: unknown token {x} at column 7 in
  tiles[0]`, rather than failing the pane. The fold row, the
  `other sessions` header and the session lines under it are fixed.

## Which tmux servers the daemon polls

Decided in issue #3: status for any tmux server the host config names; attach
only to managed sessions.

- The `laatmux` server (`tmux -L laatmux`) is the managed one. The daemon
  configures it explicitly (prefix and prefix2 off, status off, mouse off,
  root and prefix key tables unbound) and `new` creates sessions there. A
  daemon whose list leaves it out does not advertise the `new` capability.
- Every other server in `tmux_servers`, the user's `default` included, is
  observed read-only: `list-panes` and `capture-pane`, nothing else. No
  workmux config, state or hooks are read. `default` selects `-L default`
  explicitly, so a daemon started from inside another tmux server still
  polls the default one rather than following the inherited `TMUX`.
- Agent ids are `<environment_id>/<server>/<pane_id>` and records carry the
  server, so `%1` on two servers cannot collide. Clients treat the id as
  opaque.
- Only panes with an identified agent instance, alive or gone, are published.
  Shells and other tools' panes never appear on any server, and their title
  churn produces no traffic.
- `ls` shows agents on the managed server as `@host` and others as
  `@host/server`, which is what `jump --server` takes. Jump to a session on
  this machine's default server is a `switch-client`, since it is already in
  the user's tmux; it must be run from a client of that server. Jump to a remote host's default server, or to any other
  unmanaged server, is refused.

The intended policy from issue #1: the laptop watches its default server plus
the managed server; remote hosts watch only the managed server, with the
laptop providing the UI.

## The merged stream

Every client used to dial every host: a sidebar pane per window would be
an ssh channel per host per window. The daemon on the machine the user
sits at is the one process there, so it is the merge point. Every
daemon advertises `merged` (a host's config lists only the host
itself, as a rule, and then its merged stream holds its own records),
and `subscribe` with `merged: true` gets one stream with every host's
records:

```
-> {type: subscribe, merged: true}
<- {type: snapshot, seq, hosts, agents, worktrees, sessions, sessions_error}
<- {type: upsert, seq, host_status: {...}}          a host's connectivity changed
<- {type: upsert, seq, agent | worktree | pane | run: {...}}   as before, from any host
<- {type: upsert, seq, local_session: {...}}         a local workspace session changed
<- {type: remove, seq, host_name | agent_id | worktree_id | pane_record_id | run_id | local_session_name}
```

- **Host records** `{name, ssh, environment_id, connected, listed, error,
  version, capabilities, since}` are the connectivity axis, one per
  configured host. `connected` is a live connection with a completed
  hello; `listed` is that the host's records come from a snapshot of that
  connection. Records from before a drop stay while `listed` is false,
  so a listing shows what was last known with the host row saying `DOWN`
  and ssh's own message. Absence is authoritative only when both bits are
  set: a local session is orphaned only against a host that is connected,
  listed and publishes worktrees, and `jump` reports a workspace missing
  only from a listed host. The local host is itself, not a dial of its
  own socket; it is listed once the daemon's first poll of every server
  and of git is complete, and a merged subscription does not wait for
  that as a plain one does. Records are forwarded unchanged, ids included; a client maps
  a record's `environment_id` to a host name through the host records.
  `seq` is the merging daemon's own.
- **The relay**, capability `relay`, the laptop's side of
  [milestone four](docs/milestone-four.md): `{type: add, id, relay:
  <host>, repo, name, branch, generated, agent_name, cmd, prompt,
  submitted_at}` is written to `<state>/pending/<id>.json`, mode 0600
  with the prompt, before the answer says `accepted`; the daemon then
  dials the host as the client would have, sends the add under the
  client's id and follows it to the result, reconnecting with the merged
  stream's backoff for as long as the task is outstanding, and to the
  host's listing after it. Every connection is pinned to the host's
  environment id, from the host row at accept or from the first hello,
  and requires `add`, `follow` and `task`; a host known to lack `task` is
  refused at accept. The pending records travel in the merged stream:
  `pendings` and `handoffs` in a snapshot, `pending` in an upsert,
  `pending_id` with `replaced_by` in a remove. A record carries `taken`
  (the host has the add), `reachable` with `unreachable` saying why not
  and `mismatch` when another machine answers under the host's name,
  the last progress line's `stage`, `state` and `detail`, `branch` and
  `root` as the host reports them, then `done` with `ok`, `error`,
  `prompt` (the delivery state), `attempt`, `attempt_open` and
  `attempt_error` (the host's refusal of the last attempt), `listed`,
  `listing_error` and `gone`. A record is complete when the add succeeded and the prompt
  is delivered or there was none; the relay takes the host's listings
  until one passes the result's barrier, and a complete record whose
  worktree is in it is retired: the handoff is written to the file,
  then the remove is published with `replaced_by`, the worktree row's
  id. The retired record is what says what the worktree was made for:
  the file is kept, prompt and all, for as long as the worktree is
  there, and its handoff joins the task to the worktree row in every
  snapshot. The prompt stays on this machine; the host keeps none of
  it, and the stream carries the handoff, not the text. The file goes
  on `laatmux tasks dismiss <id>`, when `rm` drops the tasks at the
  worktree, when the host reports the worktree removed by a listing
  after the task's add (`removed_in` on the remove stamps that
  listing), when the host's listing no longer has it, asked as for a task that needs the
  user, and a day after the handoff once its host has left the config
  or answers as another machine. No worktree at the root is `gone`, a record
  the user dismisses. A record that needs the user stays, prompt
  retained, until `{type: dismiss, id}` or until `{type: prompt, id}`
  delivers it. A dismiss's result has in `detail` what went with the
  record: the append of a repository new to the config that the add
  asked for (`remember`) and that was not made, with the source, the
  name and why; the daemon logs it once, a dismiss through `rm` too. The
  append is not made after the dismiss: the two take turns per record.
  Its worktree removed meanwhile, by `rm` here or
  elsewhere or by hand, makes it `gone` too: a removal the host reports,
  and a host's successful listing that lacks the worktree, a
  reconnect's snapshot or a later poll's, have the host asked again, as
  the handoff asks it, until it answers; a remote host's listings are
  followed while a view is subscribed. `rm` drops the finished tasks at the worktree it
  removed, through `{type: dismiss, environment_id, root, listing}` to a
  local daemon that is running and has the capability `dismiss-root`,
  `listing` being the removal's stamp from rm's result; a task that
  handed over goes only when that stamp is from after its add, and
  without one the host's listing decides. Until then, `{type: prompt, id}`
  delivers it as the next attempt, written to the file first, one
  unresolved at a time; `recovery expired` from the host ends that, and
  `laatmux tasks show <id>` prints the prompt for pasting by hand. An add
  older than seven days is not sent and is `outcome unknown`, as is one
  the host refuses as `submission expired`; a follow answered
  `interrupted` or `unknown command` resends the add. A daemon that
  starts resumes every file: adds without an outcome, open attempts
  first, listings owed. A host gone from the config leaves its records
  waiting with `host removed`. `laatmux add --detach` submits; `laatmux
  tasks` lists, the tasks that handed over to their worktrees after the
  pending ones.
- **Hosts follow the config file.** The daemon re-reads `hosts` on every
  merged subscription, so a host added shows up on the next `ls`; one
  removed gets a `remove` for its records and then its host record. It
  does the same for the subscribers already there when it sees the file
  changed, every two seconds, so a view that stays up, the dashboard or
  a sidebar pane, lists a host added without a restart; the view reads
  the file itself too, for its jumps, removals and task form (the config
  section above says what else it takes). A host paused there is not
  dialled: its records are removed as a removed host's are, and its
  host record comes back with `paused` set and nothing else, which a
  one-shot client does not wait on; resumed, the record is replaced by
  one connecting.
- **Held only while wanted.** Remote subscriptions are opened by the
  first merged subscriber and dropped 60 seconds after the last leaves,
  so a laptop with no sidebar open holds no ssh channels; each remote
  daemon sees one subscriber per laptop whatever the laptop shows. A host
  that is down is redialled with a backoff of 1 s doubling to 30 s.
- **Local sessions** are listed by the daemon while it has a merged
  subscriber, once a second against the default server, and published as
  `{name, key, host, source, branch, attach, settled}`, so a settle
  reaches every subscriber within a second and a view needs no tmux
  access of its own. The listing also runs once, synchronously, before
  each merged snapshot, so the snapshot is as fresh as the connection. A
  listing that fails for a reason other than no server puts its message
  in `sessions_error`, which `ls` prints after the tree.
- **PR and checks**, capability `branches` (milestone five, step 5): the
  daemon asks GitHub, through `gh api graphql` on this machine, about
  every branch a worktree in the stream has on github.com or a host in
  the config's `github_hosts` (bare host names, read when the daemon
  starts), 32 branches to a query,
  with the names as variables. It asks every 30 s while a view is open,
  and at once when the set of branches changes. Each branch gets a
  `branch_status` record keyed by source key and branch:
  - the PR from the source's own repository, an open one first, else
    the newest merged or closed, with the branch it was opened against
    as `base`; a branch deleted after its PR merged keeps the PR;
  - the checks of the PR's last commit, or of the branch's own, counted
    from the rollup's aggregates, with the first failing check's name.

  The records travel as `branch_statuses` in a snapshot, `branch_status`
  in an upsert and `branch_status_key` in a remove; `github_ok` clears
  `github_error`. The answers are kept in `branches.json` under
  `$LAATMUX_HOME`. One
  older than five minutes, or whose last query failed, is stale; a
  branch no worktree has had for a day is dropped. A missing or
  logged-out `gh` is the daemon's `github_error`, printed by `laatmux
  hosts` on a `github:` line, which asks gh itself when the daemon has
  not. The views show `#N` and the checks at the
  right of a tile's third line: `✓`, `× 3/5` or a spinner and `3/5`,
  dim with `?` when stale; on `main` and `master` only failing checks.
  The dashboard adds the PR's state icon before the number and, after
  the checks, the time they have been pending or the first failing
  check's name, and `→base ! ↑A ↓B` after the git stats. Its `o`
  opens the PR and `O` its checks.
- **Attention**, capability `attention`: the daemon's done-and-seen
  state as `attention` records, `{agent_id, finished_at, seen_at}`, in
  the snapshot as `attentions` and removed by `attention_id`, keys an
  older client passes over; and `{type: poke}`, which has it list the
  clients at once and is not answered. See Sidebar and dashboard.
- **One-shot clients wait for readiness.** The snapshot comes at once
  with what the daemon knows, on a cold daemon the host rows alone. `ls`
  reads on until every host is listed or carries an error, or 20 seconds
  have passed, and marks the hosts still neither. `jump`, `path`, `rm`
  and `run` wait the same way for the one host they act on and then talk
  to that host directly, as before. A connection that was up and dropped
  is not an error to stop on: the host record carries `reconnecting`
  from the drop until the next dial's outcome, and a client waits on it
  as on a host still connecting, since a daemon restarted for an upgrade
  is back within seconds; ssh's own errors, a refused connection or a
  missing binary, end the wait at once as before. The header line says
  `DOWN disconnected (reconnecting)` meanwhile.
- **Older daemons.** A local daemon without `merged` is an older build
  still running: `ls`, `watch`, the sidebar, the dashboard and compose
  refuse it and say to stop it with `laatmux stop`, after which the next
  client starts the current build; `jump`, `path`, `rm` and `run` dial
  the host directly; `tasks` refuses. Plain `subscribe` still means this
  host's own records, which is what a remote daemon serves to the
  merging one.

## Model

- Three status axes, never collapsed: **activity** from the screen (working,
  blocked, idle, unknown), **liveness** of the identified process (alive, gone),
  and **host connectivity**, which is client side: the merging daemon's
  host records, never a field of an agent record.
- **Identity** is the agent process, not the pane: pid plus start time, looked
  for among every process on the pane's tty, not only the foreground group, so
  a tool taking the foreground never replaces the agent. Only a process's own
  program identifies it: argv[0], comm, or the script an interpreter runs (node
  running Claude Code's `cli.js` is Claude). Arguments never count, so
  `safehouse /opt/bin/claude` and `bash -c 'claude; sleep 60'` are wrappers, not
  Claude, before, between and after their children. Among candidates the
  deepest descendant wins. An env hint (`LAATMUX_AGENT`) gives a tentative
  identity that selects detection rules only; the daemon keeps searching and a
  verified process replaces it. A verified instance is checked for existence
  each poll, so an exited agent stays on record as gone with its last activity
  until a new instance appears. A process-table read error changes nothing,
  and the same instance found again after a transient omission is restored
  without a reset. Claude
  Code's native binary is named by version (`2.1.278`), so a bare semver comm
  counts as claude.
- **Detection** evaluates herdr's rule manifests against the pane title and the
  visible screen every 300 ms. Captures are unconditional and never include
  scrollback, so a dismissed dialog in history cannot override the prompt. A
  failed capture is an unavailable observation, not an empty screen: the last
  activity is kept. Working to idle
  is debounced (3 confirmations, 700 ms cap); a visible idle prompt bypasses it.
  Fresh processes get a 3 s grace measured from process start.
- **Agent hooks** are not used: every agent state comes from the pane,
  its screen and title, and the process table. The tmux hooks laatmux
  sets are the sidebar's, on the user's default server (see Sidebar
  and dashboard).
- **Managed server** reconciliation runs on discovery, once per tmux server
  pid: global options, every global hook, session-level overrides of the
  isolation options, both key tables, `update-environment` (back to
  tmux's own list), the user's login shell from the password database
  as `default-shell` and as `SHELL` in the global environment (the
  daemon's own SHELL is whatever shell started it, bash under a
  sandbox on a zsh machine, and would otherwise be every new pane's),
  extended keys (`extended-keys on`, and an outer tmux
  taken for a terminal that has them, `terminal-features[9100]
  tmux*:extkeys`, since its clients are the attach panes, an outer tmux
  locally or through `ssh -t`, which tmux does not credit with them by
  itself; so Shift-Enter reaches Claude Code in a managed session as
  it does in a plain one, given the outer server's own `extended-keys
  on` and its terminal's `extkeys` feature; an attach made before the
  server was reconciled this way must be made again, by a jump after
  the attach pane dies or by hand), and the locale of the global
  environment that new panes start with. That locale is the daemon's when its character set is
  UTF-8 on the host, else the server's own when that one is, else
  `LANG=C.UTF-8` (en_US.UTF-8 or another UTF-8 locale the host has where
  C.UTF-8 is missing). Each locale variable is kept only when it is C,
  POSIX or a UTF-8 locale the host has, as the host's `locale charmap`
  says under the server's LOCPATH and LOCALE_ARCHIVE; one that names a
  missing locale or another character set is dropped. Agents get a UTF-8
  locale even when the daemon was started over ssh or as a service
  without one. A cold start also passes
  `-f /dev/null`. `new` refuses a session that comes up with more than one
  pane. Only the `laatmux` server is ever reconciled; the other servers the
  daemon polls are the user's and are read only. Protocol: a daemon with a different protocol number is refused; within
  a number, clients branch on capabilities.
- **Daemon startup** is arbitrated by a kernel-held flock. Snapshots wait for
  the first complete poll. A subscriber that falls behind is disconnected so it
  reconnects and resnapshots. Ssh connections use ServerAlive so a silent
  network loss surfaces as a disconnect within about 45 s.

## Dev loop under safehouse

The sandbox denies unix socket binds, new tmux servers and `ps`. Use:

```sh
export LAATMUX_HOME=$PWD/.spike
export LAATMUX_CONFIG=$PWD/.spike/config.yaml
export LAATMUX_SERVE_ARGS="--listen tcp:127.0.0.1:0 --tmux-servers default"
```

What was tried by hand, milestone by milestone, is in
[docs/verified.md](docs/verified.md); `go test ./...` is what runs.
