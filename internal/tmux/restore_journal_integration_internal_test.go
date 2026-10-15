package tmux

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"strings"
	"testing"

	"github.com/alchemmist/lazy-tmux/internal/snapshot"
	"github.com/alchemmist/lazy-tmux/internal/testutil"
)

func restoreJournalTestSnap(name string) snapshot.SessionSnapshot {
	return snapshot.SessionSnapshot{
		Version:     snapshot.FormatVersion,
		SessionName: name,
		CurrentWin:  1,
		CurrentPane: 0,
		Windows: []snapshot.Window{
			{
				Index: 0, Name: "first", ActivePane: 0,
				Panes: []snapshot.Pane{{Index: 0, CurrentPath: "/tmp"}},
			},
			{
				Index: 1, Name: "second", IsActive: true, ActivePane: 1,
				Panes: []snapshot.Pane{
					{Index: 0, CurrentPath: "/tmp", IsActive: true},
					{Index: 1, CurrentPath: "/tmp"},
				},
			},
		},
	}
}

//nolint:paralleltest // uses a real shared tmux server via testutil.IsolatedTmux
func TestJournaledRestoreResumesWithoutDuplicates(t *testing.T) {
	testutil.IsolatedTmux(t)

	client := NewClient("tmux")
	client.SetRestoreTimeout(0)

	const name = "gen-resume"
	snap := restoreJournalTestSnap(name)

	journal := NewRestoreJournal(name, false)
	commits := 0
	commit := func() error {
		commits++

		return nil
	}

	if err := client.RestoreSessionJournaled(
		context.Background(),
		snap,
		journal,
		commit,
	); err != nil {
		t.Fatalf("first restore: %v", err)
	}

	if commits == 0 {
		t.Fatal("expected creation confirmations to be committed")
	}

	// Simulate a process restart: same generation, adopted from disk.
	journal.Adopted = true
	if err := client.RestoreSessionJournaled(
		context.Background(),
		snap,
		journal,
		commit,
	); err != nil {
		t.Fatalf("resumed restore: %v", err)
	}

	out := testutil.Tmux(t, "list-windows", "-t", "="+name, "-F", "#{window_index}")
	if got := strings.Fields(out); len(got) != 2 {
		t.Fatalf("resume created duplicate windows: %v", got)
	}

	for _, expected := range []struct {
		window string
		panes  int
	}{
		{"0", 1}, {"1", 2},
	} {
		out := testutil.Tmux(
			t, "list-panes", "-t", "="+name+":"+expected.window, "-F", "#{pane_index}",
		)
		if got := strings.Fields(out); len(got) != expected.panes {
			t.Fatalf("window %s panes = %v, want %d", expected.window, got, expected.panes)
		}
	}
}

//nolint:paralleltest // uses a real shared tmux server via testutil.IsolatedTmux
func TestJournaledCancelCleansCreatedSession(t *testing.T) {
	testutil.IsolatedTmux(t)

	client := NewClient("tmux")
	client.SetRestoreTimeout(0)

	const name = "gen-cancel"
	snap := snapshot.SessionSnapshot{
		Version:     snapshot.FormatVersion,
		SessionName: name,
		Windows: []snapshot.Window{
			{Index: 0, Name: "first", Panes: []snapshot.Pane{{Index: 0, CurrentPath: "/tmp"}}},
			{Index: 1, Name: "second", Panes: []snapshot.Pane{{Index: 0, CurrentPath: "/tmp"}}},
			{Index: 2, Name: "third", Panes: []snapshot.Pane{{Index: 0, CurrentPath: "/tmp"}}},
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	journal := NewRestoreJournal(name, false)
	err := client.RestoreSessionJournaled(ctx, snap, journal, func() error { return nil })
	if err == nil || !strings.Contains(err.Error(), "restore canceled") {
		t.Fatalf("expected restore canceled error, got %v", err)
	}

	if !journal.SessionCreated {
		t.Fatal("the session created before cancellation must be recorded for cleanup")
	}

	out := testutil.Tmux(t, "list-windows", "-t", "="+name, "-F", "#{window_index}")
	if got := len(strings.Fields(out)); got != 1 {
		t.Fatalf("canceled restore left %d windows, want 1", got)
	}

	if err := client.CleanupRestoreJournal(journal, nil); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	if _, err := testutil.TmuxTry("has-session", "-t", "="+name); err == nil {
		t.Fatal("cleanup must kill the session this generation created")
	}
}

//nolint:paralleltest // uses a real shared tmux server via testutil.IsolatedTmux
func TestCleanupKeepsPreExistingSession(t *testing.T) {
	testutil.IsolatedTmux(t)

	client := NewClient("tmux")
	const name = "gen-preexisting"

	testutil.Tmux(t, "new-session", "-d", "-s", name, "-n", "keep")
	testutil.Tmux(t, "new-window", "-d", "-t", "="+name+":", "-n", "added")

	journal := NewRestoreJournal(name, true)
	journal.SessionCreated = false
	journal.Windows[1] = &WindowJournal{Created: true, Panes: 1, Commands: map[int]string{}}

	if err := client.CleanupRestoreJournal(journal, nil); err != nil {
		t.Fatalf("cleanup: %v", err)
	}

	if _, err := testutil.TmuxTry("has-session", "-t", "="+name); err != nil {
		t.Fatal("a pre-existing session must never be killed on cancel")
	}

	out := testutil.Tmux(t, "list-windows", "-t", "="+name, "-F", "#{window_name}")
	if strings.TrimSpace(out) != "keep" {
		t.Fatalf("cleanup must remove only added windows, got %q", out)
	}
}

//nolint:paralleltest // uses a real shared tmux server via testutil.IsolatedTmux
func TestResumeKeepsStagedTokenAndCompletesAck(t *testing.T) {
	testutil.IsolatedTmux(t)

	client := NewClient("tmux")
	client.SetRestoreTimeout(0)

	const name = "gen-token"
	testutil.Tmux(t, "new-session", "-d", "-s", name, "-n", "work", "sleep", "600")

	intent := pendingRestore{
		Token: "staged-token",
		Pane:  snapshot.Pane{CurrentCmd: "cat"},
	}
	raw, err := json.Marshal(intent)
	if err != nil {
		t.Fatal(err)
	}
	encoded := base64.StdEncoding.EncodeToString(raw)
	testutil.Tmux(
		t, "set-option", "-p", "-t", "="+name+":0.0", pendingRestoreOption, encoded,
	)

	journal := NewRestoreJournal(name, false)
	window := snapshot.Window{
		Index: 0,
		Name:  "work",
		Panes: []snapshot.Pane{{Index: 0, CurrentPath: "/tmp", RestoreCmd: "cat"}},
	}
	if err := client.restoreWindowCommandsJournaled(
		context.Background(), name, window, 0, journal, func() error { return nil },
	); err != nil {
		t.Fatal(err)
	}

	if got := journal.Windows[0].Commands[0]; got != "staged-token" {
		t.Fatalf("staged token should be adopted, got %q", got)
	}

	out := testutil.Tmux(t, "list-panes", "-t", "="+name, "-F", "#{pane_current_command}")
	if strings.Contains(out, "cat") {
		t.Fatalf("pane with staged token must not be relaunched, got %q", out)
	}

	// The pane is past startup (running sleep), so reconciliation must ack it.
	client.reconcileRestoreAcks(context.Background(), name)

	ack := testutil.Tmux(t, "show-options", "-pqv", "-t", "="+name+":0.0", restoreAckOption)
	if strings.TrimSpace(ack) != "staged-token" {
		t.Fatalf("ack = %q, want staged-token", ack)
	}
}
