package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/laat/laatmux/internal/command"
	"github.com/laat/laatmux/internal/home"
	"github.com/laat/laatmux/internal/protocol"
)

// cmdTasks lists the pending records the local daemon holds, one line
// each with the state; `tasks show <id>` prints a record's retained
// prompt to stdout, for pasting by hand once the host can no longer
// deliver it; `tasks dismiss <id>` drops a record that needs the user;
// `tasks prompt <id>` delivers its prompt now.
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

// listTasks reads one merged snapshot and prints the pending records.
func listTasks(ctx context.Context) error {
	c, ok := dialMerged(ctx)
	if !ok {
		return errors.New("the local daemon has no merged stream")
	}
	defer c.Close()
	if !protocol.Has(c.Hello.Capabilities, protocol.CapRelay) {
		return fmt.Errorf("the local daemon %s has no relay capability", c.Hello.Version)
	}
	snap, err := c.Snapshot(ctx)
	if err != nil {
		return err
	}
	if len(snap.Pendings) == 0 {
		fmt.Println("no pending tasks")
		return nil
	}
	ps := snap.Pendings
	sort.Slice(ps, func(i, j int) bool { return ps[i].SubmittedAt.Before(ps[j].SubmittedAt) })
	for _, p := range ps {
		fmt.Printf("%s  %s/%s on %s  %s  %s\n", p.ID, p.Repo, p.Branch, p.Host, p.SubmittedAt.Local().Format(time.DateTime), TaskState(p))
	}
	return nil
}

// TaskState is one line saying where a pending record is, as the
// views say it too.
func TaskState(p protocol.Pending) string {
	switch {
	case p.Done && !p.OK:
		return p.Error
	case p.Done && p.Gone:
		return "done, worktree gone"
	case p.Done && p.Error == protocol.ErrRecoveryExpired:
		return "prompt " + p.Prompt + "; recovery expired, laatmux tasks show " + p.ID + " prints it"
	case p.Done && p.AttemptOpen:
		return fmt.Sprintf("delivering the prompt, attempt %d", p.Attempt)
	case p.Done && p.Prompt == protocol.DeliveryNotDelivered:
		return "prompt not delivered: " + p.Error
	case p.Done && p.Prompt == protocol.DeliveryUnknown:
		return "prompt delivery unknown: " + p.Error
	case p.Done && !p.Listed:
		return "done, awaiting the listing"
	case p.Done:
		return "done"
	case !p.Reachable && p.Unreachable != "":
		return "host unreachable, retrying: " + p.Unreachable
	case !p.Taken:
		return "submitted"
	default:
		return "adding: " + p.Stage
	}
}

// showTask prints the retained prompt of a pending record, read from
// its file under the state directory.
func showTask(id string) error {
	b, err := os.ReadFile(filepath.Join(home.Dir(), "pending", id+".json"))
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
