package app

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alchemmist/lazy-tmux/internal/snapshot"
)

var errFakeCaptureFailed = errors.New("tmux capture failed")

type saveAllFakeTmux struct {
	tmuxClient

	sessions []string
	fail     map[string]error
	captured map[string]snapshot.SessionSnapshot
}

func (f *saveAllFakeTmux) ListSessions() ([]string, error) {
	return f.sessions, nil
}

func (f *saveAllFakeTmux) CaptureSession(name string) (snapshot.SessionSnapshot, error) {
	if err := f.fail[name]; err != nil {
		return snapshot.SessionSnapshot{}, err
	}

	return f.captured[name], nil
}

func fakeCapturedSnapshot(name, marker string, captured time.Time) snapshot.SessionSnapshot {
	return snapshot.SessionSnapshot{
		Version:     snapshot.FormatVersion,
		SessionName: name,
		CapturedAt:  captured,
		CurrentWin:  0,
		Windows: []snapshot.Window{{
			Index: 0,
			Name:  "win-" + marker,
			Panes: []snapshot.Pane{{
				Index:       0,
				CurrentCmd:  "zsh",
				CurrentPath: "/tmp",
				Scrollback:  &snapshot.ScrollbackRef{Content: "sb-" + marker + "\n"},
			}},
		}},
	}
}

func seedAppGeneration(t *testing.T, a *App, marker string, captured time.Time, names ...string) {
	t.Helper()

	snaps := make([]snapshot.SessionSnapshot, 0, len(names))
	for _, name := range names {
		snaps = append(snaps, fakeCapturedSnapshot(name, marker, captured))
	}

	if err := a.store.SaveSessions(snaps); err != nil {
		t.Fatalf("seed generation: %v", err)
	}
}

func TestSaveAllPublishesCapturedSetAtomically(t *testing.T) {
	t.Parallel()

	a, dir := newTestApp(t)

	captured := time.Unix(2000, 0).UTC()
	a.tmux = &saveAllFakeTmux{
		sessions: []string{"alpha", "beta"},
		fail:     map[string]error{},
		captured: map[string]snapshot.SessionSnapshot{
			"alpha": fakeCapturedSnapshot("alpha", "new", captured),
			"beta":  fakeCapturedSnapshot("beta", "new", captured),
		},
	}

	count, err := a.SaveAll()
	if err != nil {
		t.Fatalf("save all: %v", err)
	}

	if count != 2 {
		t.Fatalf("saved count = %d, want 2", count)
	}

	for _, name := range []string{"alpha", "beta"} {
		snap, loadErr := a.store.LoadSession(name)
		if loadErr != nil {
			t.Fatalf("load %s: %v", name, loadErr)
		}

		if snap.Windows[0].Name != "win-new" {
			t.Fatalf("%s window = %q", name, snap.Windows[0].Name)
		}

		if sb := snap.Windows[0].Panes[0].Scrollback; sb == nil || sb.Content != "sb-new\n" {
			t.Fatalf("%s scrollback not published atomically: %+v", name, sb)
		}
	}

	if _, err := os.Stat(filepath.Join(dir, ".txn")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transaction dir left behind: %v", err)
	}
}

func TestSaveAllCaptureFailureKeepsPreviousGeneration(t *testing.T) {
	t.Parallel()

	a, dir := newTestApp(t)

	old := time.Unix(1000, 0).UTC()
	seedAppGeneration(t, a, "old", old, "alpha", "beta", "gamma")

	a.tmux = &saveAllFakeTmux{
		sessions: []string{"alpha", "beta", "gamma"},
		fail:     map[string]error{"beta": errFakeCaptureFailed},
		captured: map[string]snapshot.SessionSnapshot{
			"alpha": fakeCapturedSnapshot("alpha", "new", time.Unix(2000, 0).UTC()),
			"gamma": fakeCapturedSnapshot("gamma", "new", time.Unix(2000, 0).UTC()),
		},
	}

	if _, err := a.SaveAll(); err == nil {
		t.Fatal("expected capture failure")
	}

	for _, name := range []string{"alpha", "beta", "gamma"} {
		snap, loadErr := a.store.LoadSession(name)
		if loadErr != nil {
			t.Fatalf("load %s: %v", name, loadErr)
		}

		if snap.Windows[0].Name != "win-old" {
			t.Fatalf("%s leaked a new generation window %q", name, snap.Windows[0].Name)
		}

		if !snap.CapturedAt.Equal(old) {
			t.Fatalf("%s captured time changed to %v", name, snap.CapturedAt)
		}

		records, listErr := a.store.ListRecords()
		if listErr != nil {
			t.Fatalf("list records: %v", listErr)
		}

		var record snapshot.Record

		for _, candidate := range records {
			if candidate.SessionName == name {
				record = candidate

				break
			}
		}

		if !record.CapturedAt.Equal(old) {
			t.Fatalf("%s index record changed to %v", name, record.CapturedAt)
		}
	}

	if _, err := os.Stat(filepath.Join(dir, ".txn")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed capture left a transaction dir: %v", err)
	}
}

func TestSaveAllEmptySessionListTouchesNothing(t *testing.T) {
	t.Parallel()

	a, dir := newTestApp(t)
	a.tmux = &saveAllFakeTmux{sessions: nil, fail: map[string]error{}}

	count, err := a.SaveAll()
	if err != nil {
		t.Fatalf("save all empty: %v", err)
	}

	if count != 0 {
		t.Fatalf("count = %d, want 0", count)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read data dir: %v", err)
	}

	if len(entries) != 0 {
		t.Fatalf("empty save created artifacts: %v", entries)
	}
}

//nolint:paralleltest // stubs the package-level newDaemonTicker seam
func TestRunDaemonRecoversUnfinishedGenerationOnStartup(t *testing.T) {
	a, dir := newTestApp(t)

	// Residue from a daemon killed mid-publish: an orphan generation directory
	// (no manifest) plus legacy staging artifacts.
	orphanDir := filepath.Join(dir, ".txn", "gen-20260102T030405.000000000-abcd1234")
	if err := os.MkdirAll(orphanDir, 0o700); err != nil {
		t.Fatalf("mkdir orphan gen: %v", err)
	}

	if err := os.WriteFile(filepath.Join(orphanDir, "stray"), []byte("x"), 0o600); err != nil {
		t.Fatalf("write stray file: %v", err)
	}

	scrollRoot := filepath.Join(dir, "scrollback")
	if err := os.MkdirAll(scrollRoot, 0o700); err != nil {
		t.Fatalf("mkdir scrollback: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(scrollRoot, "ghost.tmp"), 0o700); err != nil {
		t.Fatalf("mkdir legacy tmp: %v", err)
	}

	saveCalls := 0
	a.saveAllFn = func() error {
		saveCalls++

		return nil
	}

	orig := newDaemonTicker
	defer func() { newDaemonTicker = orig }()

	ch := make(chan time.Time, 1)
	ch <- time.Time{}
	close(ch)

	newDaemonTicker = func(time.Duration) daemonTicker { return &fakeTicker{ch: ch} }

	if err := a.RunDaemon(time.Minute); err != nil {
		t.Fatalf("run daemon: %v", err)
	}

	if saveCalls != 2 {
		t.Fatalf("expected 2 save ticks, got %d", saveCalls)
	}

	if _, err := os.Stat(filepath.Join(dir, ".txn")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("daemon did not converge orphan generation: %v", err)
	}

	if _, err := os.Stat(filepath.Join(scrollRoot, "ghost.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("daemon did not sweep legacy staging dir: %v", err)
	}
}
