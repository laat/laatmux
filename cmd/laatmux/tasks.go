package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/command"
	"github.com/laat/laatmux/internal/config"
	"github.com/laat/laatmux/internal/daemon"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/merged"
	"github.com/laat/laatmux/internal/protocol"
	"github.com/laat/laatmux/internal/tmux"
)

// cmdTasks lists the pending records the local daemon holds, one line
// each with the state, then the tasks that handed over to their
// worktrees, whose records the daemon keeps with the prompt for the
// worktree's life; `tasks show <id>` prints a record's retained prompt
// to stdout, for pasting by hand once the host can no longer deliver
// it, or to see what a worktree was made for; `tasks dismiss <id>`
// drops a record that needs the user, or a handed-over one, prompt and
// all; `tasks prompt <id>` delivers its prompt now.
func cmdTasks(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return listTasks(ctx)
	}
	if len(args) != 2 {
		return errors.New("usage: laatmux tasks [show|dismiss|prompt <id>]")
	}
	id := args[1]
	switch args[0] {
	case "show":
		return showTask(id)
	case "dismiss":
		return command.Dismiss(ctx, id)
	case "prompt":
		state, reason, err := command.DeliverPending(ctx, id)
		if err != nil {
			return err
		}
		if reason != "" {
			fmt.Printf("prompt %s: %s\n", state, reason)
		} else {
			fmt.Printf("prompt %s\n", state)
		}
		return nil
	}
	return errors.New("usage: laatmux tasks [show|dismiss|prompt <id>]")
}

// listTasks reads the merged stream's snapshot, which carries the
// pending records, and prints them.
func listTasks(ctx context.Context) error {
	// The config is read here as every command reads it: a file that
	// does not parse is the error, not a host list with nothing in it,
	// which would make every record read as host removed.
	if _, err := config.Load(); err != nil {
		return err
	}
	c, err := dialMergedOrExplain(ctx)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := needRelay(c); err != nil {
		return err
	}
	m := merged.New()
	if _, err := m.Read(ctx, c, 5*time.Second, func([]string) bool { return true }); err != nil {
		return err
	}
	fmt.Print(taskReport(m.Status("")))
	return nil
}

// taskReport is what tasks prints of the merged state: the pending
// records, oldest first, then the tasks that handed over, by id, each
// named by its worktree when that is listed. tasks configures no
// labels, so a worktree's repository is the host's name for it.
func taskReport(s merged.Status) string {
	ps := append([]protocol.Pending(nil), s.Input.Pendings...)
	var handed []string
	for id, to := range s.Handoffs {
		where := to
		if i := slices.IndexFunc(s.Input.Worktrees, func(w protocol.Worktree) bool { return w.ID == to }); i >= 0 {
			w := s.Input.Worktrees[i]
			where = w.Repo + "/" + w.Branch
			if host := s.ByHost[to]; host != "" {
				where += " on " + host
			}
		}
		// An unlisted worktree is named by its id, which has its root.
		handed = append(handed, fmt.Sprintf("%s  handed over to %s; laatmux tasks show %s prints its prompt, tasks dismiss drops it", id, tmux.Printable(where), id))
	}
	if len(ps) == 0 && len(handed) == 0 {
		return "no pending tasks\n"
	}
	var b strings.Builder
	sort.Slice(ps, func(i, j int) bool { return ps[i].SubmittedAt.Before(ps[j].SubmittedAt) })
	for _, p := range ps {
		_, configured := s.Host(p.Host)
		// git takes a C1 control character and a byte that is not
		// UTF-8 in a branch.
		fmt.Fprintf(&b, "%s  %s on %s  %s  %s\n", p.ID, tmux.Printable(p.Repo+"/"+p.Branch), p.Host, p.SubmittedAt.Local().Format(time.DateTime), TaskState(p, configured))
	}
	sort.Strings(handed)
	for _, l := range handed {
		b.WriteString(l + "\n")
	}
	return b.String()
}

// TaskState is one line saying where a pending record is, as the
// views say it too. configured is whether the merged stream's host
// list has the record's host: the daemon re-reads the config for
// every subscription, so a host removed from it has no row, and the
// record says so first, since it is dismissable then whatever else it
// says. An error is put as tmux.Printable shows it: a failed setup's
// is the last lines of its output, and git's can span lines.
func TaskState(p protocol.Pending, configured bool) string {
	switch {
	case !configured:
		return "host removed; laatmux tasks dismiss " + p.ID + " drops it"
	case p.Mismatch != "":
		return "host replaced: " + p.Mismatch + "; laatmux tasks dismiss " + p.ID + " drops it"
	case p.Done && !p.OK:
		return tmux.Printable(p.Error)
	case p.Done && p.Gone:
		return "done, worktree gone"
	case p.Done && p.AttemptOpen:
		return fmt.Sprintf("delivering the prompt, attempt %d", p.Attempt)
	case p.Done && p.AttemptError == protocol.ErrRecoveryExpired:
		return "prompt " + p.Prompt + "; recovery expired, laatmux tasks show " + p.ID + " prints it"
	case p.Done && p.AttemptError != "":
		return "prompt " + p.Prompt + "; last attempt refused: " + tmux.Printable(p.AttemptError)
	case p.Done && p.Prompt == protocol.DeliveryNotDelivered:
		return "prompt not delivered: " + tmux.Printable(p.Error)
	case p.Done && p.Prompt == protocol.DeliveryUnknown:
		return "prompt delivery unknown: " + tmux.Printable(p.Error)
	case p.Done && !p.Listed && p.ListingError != "":
		return "done, awaiting the listing: " + tmux.Printable(p.ListingError)
	case p.Done && !p.Listed:
		return "done, awaiting the listing"
	case p.Done:
		return "done"
	case !p.Reachable && p.Unreachable != "":
		return "host unreachable, retrying: " + tmux.Printable(p.Unreachable)
	case !p.Taken:
		return "submitted"
	default:
		return "adding: " + p.Stage
	}
}

// showTask prints the retained prompt of a pending record, read from
// its file under the state directory.
func showTask(id string) error {
	// The id names a file only through the daemon's mapping, which
	// hashes anything that is not a plain name; a path is never built
	// from it.
	b, err := os.ReadFile(filepath.Join(home.Dir(), "pending", daemon.FileName(id)))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("no pending record %s", id)
		}
		return err
	}
	var p struct {
		PromptText string `json:"prompt_text"`
	}
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	if p.PromptText == "" {
		return fmt.Errorf("no prompt retained for %s", id)
	}
	fmt.Print(p.PromptText)
	if !strings.HasSuffix(p.PromptText, "\n") {
		fmt.Println()
	}
	return nil
}
