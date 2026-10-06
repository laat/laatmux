# Verification logs

What was tried by hand, by milestone, moved here from the README on 2026-10-06. The tests under `go test ./...` are the checks that run.

## Verified

Locally against live panes: identity through the safehouse bash wrapper,
working and idle for Claude Code and Codex, snapshot and remove on session
kill, `new` with tagged pane options, on-demand daemon start, hello with
capabilities.

Against a Debian VM over ssh (`laatmux-test.coder`): on-demand remote daemon
start through the ssh bridge, unix socket listening on both sides, the
dedicated `laatmux` tmux server with prefix, status, mouse off and both key
tables empty, `new --host vm` starting Claude Code, `/proc` identity on Linux,
blocked on the folder trust dialog, idle at the prompt, working during a turn and idle again when it finished, a daemon of
version 0.0.1 and one of 0.0.2 talking to the same client, and recovery under
`watch` after killing the ssh bridge and then the remote daemon, with no ghost
or duplicate entries.

One bug found remotely: tmux 3.5 strips control characters from expanded
formats, so a `\x1f` field separator fused the `list-panes` fields on Linux
while passing through on macOS 3.6. Fields are now separated by a printable
sequence (`tmux.Sep`).

After the first design review, on the VM: cold start under a hostile
`~/.tmux.conf` (status on, prefix `C-a`, mouse on, a root binding and a
split-on-new-session hook) still gave one pane with prefix, status and mouse
off and no root bindings. Claude killed under a surviving `bash -c` wrapper
was reported as a new instance with a new pid after the wrapper restarted it,
and after the final kill the daemon reported no agent on the tty. Identity
stayed on the Claude pid across a tool call.

After the second review, on the VM: a server started by hand under that config
carried prefix `C-a`, a session-level prefix override, status on, the split
hook, a second pane and 17 root bindings; a plain `laatmux ls` reconciled all
of it, and two `new` calls afterwards each produced one pane. Wrapper-before-
child, exit under a surviving wrapper, tentative-then-verified, transient read
error, capture failure on a blocked prompt, discovery-time reconciliation,
subprocess close under cancellation, and protocol mismatch are covered by
tests under `-race`.

## Jump and attach spike

Milestone one's jump opened an attach window in whatever session it ran
from; milestone two replaced that with the workspace session (see the
README's Workspaces, client side), and the first and fifth findings
below describe the old behaviour: jump now
switches to the workspace session, and a dead attach stays as a dead pane
under `remain-on-exit` until the next jump respawns it. The attach command
and the transport findings, keys, size, two attachments, the preflight and
mouse, carried over unchanged. Run from a scratch tmux session so the
user's view stayed untouched. Local and remote:

- First `jump` opens a window in the session it was run from and tags the
  pane; a second `jump` focuses that window instead of opening another.
- Keys typed into the attach pane reach the managed pane; paste-buffer too.
- The inner pane follows the outer pane's size, locally and over ssh.
- Two attachments to one session both see the same output; closing one leaves
  the other. With `window-size latest` the inner size trails one event behind
  whichever client last acted, which is tmux's semantics.
- Killing the attach pane's ssh closes the window; the remote session survives
  and the next `jump` reattaches. Killing the remote session closes the window
  and the next `jump` reports "no such session" instead of opening a dying one.
- Killing the daemon repeatedly during initial discovery while `watch` was
  subscribed left one row per agent, no ghosts, after reconnect.

Two bugs found by the spike: the inner tmux refused to nest because `TMUX` was
still set in the attach pane, and the exact-match session target `=name` was
eaten by zsh's equals expansion. Both fixed.

From review: the remote preflight (`has-session` over ssh before opening a
window) now reports an absent session, an ssh failure with ssh's own message,
and a timeout as three different errors, and is bounded to 15 s with
ConnectTimeout and keepalives. Verified live: an unresolvable host fails at
once with ssh's message. Under `remain-on-exit on` a dead attach pane is
respawned in place and keys pass through afterwards.

Mouse: a probe in the managed pane that requests mouse mode and echoes its
input (`printf '\e[?1000h\e[?1006h'; cat -v`) received SGR click and wheel
reports injected into the attach pane with `send-keys -l`, locally and over
ssh, and the mode request propagated outward: both the inner pane and the
outer attach pane showed `mouse_any_flag` set, which is what makes the outer
tmux forward a real click rather than use it. Claude Code and Codex do not
request mouse mode, so ordinary clicks stay with the local tmux. A real click
in a real terminal was not part of the spike; to do it, run the probe in a
managed pane, click in the attach pane and expect `^[[<0;12;5M` with the
column and row of the click.

## Milestone two, step 3, on the VM

Against the Debian VM through the ssh bridge with raw protocol messages,
since the client commands are not built yet: a first `add` cloned a local
bare repository into `~/src/proj`, made the branch and worktree, ran both
setup commands and started the agent in a tagged session, and the worktree
record named the session. Then, in order: a half-written copy temp file and
a removed setup marker left behind as if by a crash, after which the retry
finished the copy, reran only the unmarked command and skipped the agent
stage by root, and the same id sent again replayed the identical stream
without running anything. A branch whose committed `.laatmux.yaml` fails
stopped at `setup` with the command's output and left the worktree; the
file fixed in the worktree itself was read on the retry. A hand-made session
under the intended name on another root failed the `agent` stage as a name
in use, and once killed the retry started the agent with every other step
skipped. `rm` on a worktree with an untracked file was refused with git's
message and the session left running; with `force` the worktree went and the
session was killed; a repeat was `ok`; a detached worktree made by hand was
removed by root alone. A worktree directory deleted by hand disappeared from
the listing, and the next `add` pruned it, registered it again, reran setup
because the markers died with the git directory, and found the surviving
session by root. Two `add`s on the public laatmux repository at once, one
starting Claude Code over an HTTPS clone, ran one after the other under the
per-repository lock; the snapshot showed Claude blocked on the trust dialog
and four worktree records each with its session.

## Milestone two, steps 4 and 5, on the VM

From the laptop with a scratch config naming the VM as the only host and a
`sleep` agent, run outside tmux so nothing switched the user's client:
`add` streamed every stage and created the tagged local session; a repeat
`add` skipped every step and found the session by key; `path` printed the
root; `jump` on a worktree without a session said how to start one, and on
an unknown name reported no such session. `shell` run with `TMUX_PANE` set
to the workspace's attach pane opened an ssh window at the root on the VM,
a second call selected it, and outside a workspace session it said so.
`settle` moved the row under `settled` in `ls` and `unsettle` by name
cleared it. Killing the attach pane's ssh left it dead and `jump`
respawned it; a session whose attach pane had been closed got a new attach
window on the next `jump`. `rm` was refused with git's message while
`setup.log` was untracked and the sessions stayed; `rm --force` removed
the worktree, killed the managed session and the local one. A worktree
removed by hand on the VM showed the local session under `stale`, and `rm`
on it returned `ok`, killed the surviving managed session by root and the
local session. A detached worktree was removed with `--root`.

The user's tmux config splits every new session with a sidebar pane, which
is why the attach pane is tagged by id rather than taken as the active
pane.

After review, on the VM: a stale `@laatmux_host` set by hand on a
workspace session was refreshed by the next `jump`; a workspace session
renamed by hand whose worktree was then removed on the VM was found by its
source and branch tags, and `rm` killed the surviving managed session by
root and the renamed local session; `add` run from a checkout whose origin
is not configured refused with the origin named rather than guessing from
the directory. Then, with the source in the record: `add` under a local
label that differs from the host's, and `jump` by the host's label, left
the source tag intact; `rm` on a branch with no worktree whose local
session by name carried another source and branch sent no root and left
both that session and the other worktree alone, while the same session
with no identity tags had its root sent and was killed.

## Milestone three, step 2, on the VM

The merged stream, with an isolated daemon (`LAATMUX_HOME=.spike`, a
scratch copy of the config with hosts `mac` and `vm`) and the VM's daemon
an older build without `merged`. `ls` through the local daemon listed
both hosts in 1.8 s cold and left one `ssh -T ... laatmux bridge` behind;
twenty `watch` processes at once held that one channel and one bridge on
the VM. A host with an unresolvable alias appended to the config file
showed up on the next `ls` and in every running `watch` as `DOWN` with
ssh's own message, and was gone after its removal. Killing the bridge on
the VM turned its row `DOWN  disconnected` with its worktrees still
listed, then `connected (snapshot pending)`, then `connected`, within the
backoff. `jump` on a worktree without a session answered in 25 ms from
the stream with the `add` hint; `jump` on an unknown session was refused
by the direct preflight as before. Seventy seconds after the last `watch`
was killed there was no ssh channel on the laptop and no bridge on the
VM, and the next `ls` reconnected in 1.3 s.

## Milestone three, step 3, under tmux

The sidebar and the dashboard, on an isolated default server
(`TMUX_TMPDIR` pointed at a scratch directory) started with the user's
tmux.conf and given a hook like their sidebar's that splits a pane off
every new window and session, against the laptop's real daemon. `sidebar
on` in a server with two windows set the four hooks and left each window
with a 35-column tagged pane on the left running `sidebar pane`, focus on
the pane that had it, in 0.8 s, also with an `after-split-window` hook
that selects the next pane. A new window and a new session each got
a sidebar through the hooks with the other hook's pane beside it; a
second `attach` on a window that has one changed nothing. `hook_window`
and `hook_session` expand to nothing in after-hooks on tmux 3.6a, so the
hooks use `window_id`, which is the new window in both. In the pane, `j`
moved the reverse-video selection, `v` switched to compact, `Enter` on a
worktree without a session put the `add` line in the footer, `/bro`
filtered to the one matching row and `Esc` restored the list, host tags
for the remote host were dim. Killing a window's other panes ran `reap`
and the window was gone within a second. `off` removed the hooks and
every tagged pane. `dashboard` in a window drew the compact layout with
the hint line, kept the `add` message on a failed jump, and `q` closed
it. `ls` printed as before from the shared rows.

## Milestone three, step 5, on the VM

With the VM's daemon rebuilt and a scratch laptop config naming its
repositories, from the laptop: `run proj/step5 --host vm -- sh -c ...`
printed the stdout line to stdout and the stderr line to stderr, the
VM's hostname and the worktree root, and exited 4 as the command did.
Ctrl-C during `sleep 100` printed `cancelling`, then `cancelled`, exit
130, and no sleep was left on the VM. During a 12-line one-per-second
run, `pkill -f "laatmux bridge"` on the VM made the client print
`connection lost (EOF); reconnecting to follow run`; the output had all
twelve lines once, in order, then `done`, exit 0. `SIGTERM` to the VM's
daemon mid-run ended the run as `cancelled`, exit 130, with the process
gone and the next client restarting the daemon. `rm proj/step5 --force`
during a run returned ok after the run's client had printed
`cancelled`, and the worktree was gone. On an isolated laptop daemon
the same held, plus: inside a workspace session `run -- pwd` printed
the root with no target named; `SIGKILL` to the daemon mid-run made the
client reconnect, start a fresh daemon, follow, and exit 255 with the
outcome-unknown message while the process kept running, as documented;
`split -h` on the workspace's pane, by pane id with `TMUX` set as a
`run-shell` job has it, opened a pane with its shell at the worktree
root while the attach pane stayed where it was, and on a plain session
opened one in the pane's directory; the bound form
`run-shell "laatmux split -h '#{pane_id}'"` did the same.

## Not yet verified

Nothing in milestone one's acceptance list. Mouse passthrough was checked with
injected reports, not a physical click.
