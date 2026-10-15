package app

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/alchemmist/lazy-tmux/internal/snapshot"
	"github.com/alchemmist/lazy-tmux/internal/tmux"
)

var errGenerationFake = errors.New("generation fake failure")

type generationFakeTmux struct {
	tmuxClient

	mu sync.Mutex

	inside bool
	exists bool

	engineCalls    int
	createCalls    int
	killedSessions []string
	syncedTargets  []string
	switches       []string
	switchErr      error
	engineErr      error
	cleanupErr     error
}

func (g *generationFakeTmux) SessionExists(string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	return g.exists
}

func (g *generationFakeTmux) InsideTmux() bool { return g.inside }

func (g *generationFakeTmux) SwitchClient(target string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.switches = append(g.switches, target)

	return g.switchErr
}

func (g *generationFakeTmux) SynchronizeWindowSize(target string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.syncedTargets = append(g.syncedTargets, target)

	return nil
}

func (g *generationFakeTmux) AttachSession(string) error { return nil }

func (g *generationFakeTmux) KillSession(name string) error {
	g.mu.Lock()
	defer g.mu.Unlock()

	g.killedSessions = append(g.killedSessions, name)

	return nil
}

func (g *generationFakeTmux) RestoreSession(context.Context, snapshot.SessionSnapshot) error {
	return nil
}

func (g *generationFakeTmux) RestoreSessionJournaled(
	ctx context.Context,
	_ snapshot.SessionSnapshot,
	journal *tmux.RestoreJournal,
	commit tmux.RestoreJournalCommit,
) error {
	g.mu.Lock()
	g.engineCalls++
	alreadyCreated := journal.SessionCreated
	engineErr := g.engineErr
	g.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return fmt.Errorf("fake restore canceled: %w", err)
	}

	if engineErr != nil {
		return engineErr
	}

	if !alreadyCreated {
		g.mu.Lock()
		g.createCalls++
		g.mu.Unlock()

		journal.SessionCreated = true
		journal.Windows[0] = &tmux.WindowJournal{
			Created:  true,
			Panes:    1,
			Commands: map[int]string{0: "1"},
		}

		if commit != nil {
			if err := commit(); err != nil {
				return err
			}
		}
	}

	return nil
}

func (g *generationFakeTmux) CleanupRestoreJournal(
	journal *tmux.RestoreJournal,
	commit tmux.RestoreJournalCommit,
) error {
	g.mu.Lock()
	cleanupErr := g.cleanupErr
	g.mu.Unlock()

	if cleanupErr != nil {
		return cleanupErr
	}

	if journal.SessionCreated && !journal.PreExisted {
		g.mu.Lock()
		g.killedSessions = append(g.killedSessions, journal.SessionName)
		g.mu.Unlock()

		journal.SessionCreated = false
		journal.Windows = make(map[int]*tmux.WindowJournal)
	}

	if commit != nil {
		return commit()
	}

	return nil
}

func (g *generationFakeTmux) killed(session string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	return slices.Contains(g.killedSessions, session)
}

func generationTestApp(t *testing.T, fake *generationFakeTmux) *App {
	t.Helper()

	a, _ := newTestApp(t)
	a.tmux = fake

	return a
}

func saveGenerationSnapshot(t *testing.T, a *App, name string) {
	t.Helper()

	err := a.store.SaveSession(snapshot.SessionSnapshot{
		Version:     snapshot.FormatVersion,
		SessionName: name,
		CapturedAt:  time.Now(),
		Windows: []snapshot.Window{{
			Index: 0,
			Name:  "main",
			Panes: []snapshot.Pane{{Index: 0, CurrentPath: "/tmp"}},
		}},
	})
	if err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
}

func TestGenerationRestoreSuccessConcludesJournal(t *testing.T) {
	t.Parallel()

	fake := &generationFakeTmux{inside: true}
	a := generationTestApp(t, fake)
	saveGenerationSnapshot(t, a, "s1")

	if err := a.RestoreTarget(PickerTarget{SessionName: "s1"}, true); err != nil {
		t.Fatalf("restore: %v", err)
	}

	if !slices.Contains(fake.switches, "s1") || len(fake.syncedTargets) != 1 {
		t.Fatalf("handoff not completed: switches=%v synced=%v", fake.switches, fake.syncedTargets)
	}

	if _, ok, err := a.loadRestoreJournal("s1"); err != nil || ok {
		t.Fatalf("journal should be removed after success, ok=%v err=%v", ok, err)
	}
}

func TestGenerationMissingSnapshotLeavesNoJournal(t *testing.T) {
	t.Parallel()

	fake := &generationFakeTmux{inside: true}
	a := generationTestApp(t, fake)

	err := a.RestoreTarget(PickerTarget{SessionName: "missing"}, true)
	if err == nil {
		t.Fatal("expected missing snapshot error")
	}

	if _, ok, loadErr := a.loadRestoreJournal("missing"); loadErr != nil || ok {
		t.Fatalf("missing snapshot must not leave a journal, ok=%v err=%v", ok, loadErr)
	}
}

func TestGenerationSessionExistsKeepsCompatHandoff(t *testing.T) {
	t.Parallel()

	fake := &generationFakeTmux{inside: true, exists: true, engineErr: tmux.ErrSessionExists}
	a := generationTestApp(t, fake)
	saveGenerationSnapshot(t, a, "live")

	if err := a.RestoreTarget(PickerTarget{SessionName: "live"}, true); err != nil {
		t.Fatalf("restore existing session: %v", err)
	}

	if !slices.Contains(fake.switches, "live") {
		t.Fatalf("existing session should still hand off: %v", fake.switches)
	}
	if len(fake.syncedTargets) != 0 {
		t.Fatalf("existing session must not resize, synced=%v", fake.syncedTargets)
	}
	if fake.killed("live") {
		t.Fatal("existing session must never be killed")
	}

	if _, ok, _ := a.loadRestoreJournal("live"); ok {
		t.Fatal("journal should be removed after SessionExists handoff")
	}
}

func TestCancelCleanupFailureLeavesCleaningJournal(t *testing.T) {
	t.Parallel()

	fake := &generationFakeTmux{cleanupErr: errGenerationFake}
	a := generationTestApp(t, fake)
	saveGenerationSnapshot(t, a, "c1")

	generation, err := a.beginRestoreGeneration(PickerTarget{SessionName: "c1"})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := a.driveRestoreGeneration(ctx, PickerTarget{SessionName: "c1"}, generation); err == nil {
		t.Fatal("expected canceled restore error")
	}

	if err := a.convergeRestoreCleanup(&generation.journal); err == nil {
		t.Fatal("kill failure must not report cancellation as complete")
	}
	generation.release()

	journal, ok, err := a.loadRestoreJournal("c1")
	if err != nil || !ok {
		t.Fatalf("cleaning residue journal must persist, ok=%v err=%v", ok, err)
	}
	if journal.Status != tmux.RestoreStatusCleaning || journal.LastError == "" {
		t.Fatalf("journal status=%s error=%q", journal.Status, journal.LastError)
	}

	// A new restore must converge the cleaning generation first.
	if _, err := a.beginRestoreGeneration(PickerTarget{SessionName: "c1"}); err == nil {
		t.Fatal("restore while cleanup is failing must be blocked")
	}

	// Once tmux responds, the next restore converges and starts fresh.
	fake.cleanupErr = nil

	generation2, err := a.beginRestoreGeneration(PickerTarget{SessionName: "c1"})
	if err != nil {
		t.Fatalf("begin after cleanup recovered: %v", err)
	}
	defer generation2.release()

	if err := a.driveRestoreGeneration(
		context.Background(), PickerTarget{SessionName: "c1"}, generation2,
	); err != nil {
		t.Fatalf("drive after cleanup: %v", err)
	}
	if err := a.finishRestoreGeneration(
		PickerTarget{SessionName: "c1"}, generation2, false,
	); err != nil {
		t.Fatalf("finish: %v", err)
	}

	if _, ok, _ := a.loadRestoreJournal("c1"); ok {
		t.Fatal("converged restore must remove its journal")
	}
}

func TestCancelOnPreExistingSessionNeverKillsSession(t *testing.T) {
	t.Parallel()

	fake := &generationFakeTmux{inside: true, exists: true}
	a := generationTestApp(t, fake)
	saveGenerationSnapshot(t, a, "pre")

	generation, err := a.beginRestoreGeneration(PickerTarget{SessionName: "pre"})
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer generation.release()

	if !generation.journal.PreExisted {
		t.Fatal("generation must record that the session pre-existed")
	}

	// Simulate a window added before cancellation.
	generation.journal.Windows[1] = &tmux.WindowJournal{
		Created:  true,
		Panes:    1,
		Commands: map[int]string{},
	}
	_ = a.saveRestoreJournal(generation.journal)

	if err := a.convergeRestoreCleanup(&generation.journal); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if err := a.deleteRestoreJournal("pre"); err != nil {
		t.Fatalf("delete journal: %v", err)
	}

	if fake.killed("pre") {
		t.Fatal("pre-existing session must not be killed on cancel")
	}
}

func TestResumeRunningGenerationSkipsCreation(t *testing.T) {
	t.Parallel()

	fake := &generationFakeTmux{inside: true, exists: true}
	a := generationTestApp(t, fake)
	saveGenerationSnapshot(t, a, "r1")

	running := *tmux.NewRestoreJournal("r1", false)
	running.Adopted = true
	running.Status = tmux.RestoreStatusRunning
	running.SessionCreated = true
	running.Windows[0] = &tmux.WindowJournal{
		Created:  true,
		Panes:    1,
		Commands: map[int]string{0: "1"},
	}
	if err := a.saveRestoreJournal(running); err != nil {
		t.Fatalf("seed journal: %v", err)
	}

	if err := a.RestoreTarget(PickerTarget{SessionName: "r1"}, true); err != nil {
		t.Fatalf("resume: %v", err)
	}

	if fake.createCalls != 0 {
		t.Fatalf("resume must not recreate resources, createCalls=%d", fake.createCalls)
	}
	if !slices.Contains(fake.switches, "r1") {
		t.Fatalf("resume should finish handoff: %v", fake.switches)
	}

	records, err := a.store.ListRecords()
	if err != nil {
		t.Fatal(err)
	}
	if len(records) != 1 || records[0].LastAccessed.IsZero() {
		t.Fatalf("resume should only complete the access-time stage: %+v", records)
	}
}

func TestSwitchFailureRetryCompletesOnlyHandoff(t *testing.T) {
	t.Parallel()

	fake := &generationFakeTmux{inside: true, switchErr: errGenerationFake}
	a := generationTestApp(t, fake)
	saveGenerationSnapshot(t, a, "h1")

	if err := a.RestoreTarget(PickerTarget{SessionName: "h1"}, true); err == nil {
		t.Fatal("first handoff should fail")
	}

	journal, ok, err := a.loadRestoreJournal("h1")
	if err != nil || !ok || !journal.Accessed || !journal.SessionCreated {
		t.Fatalf("failed handoff must keep a resumable journal: %+v err=%v", journal, err)
	}

	fake.switchErr = nil

	if err := a.RestoreTarget(PickerTarget{SessionName: "h1"}, true); err != nil {
		t.Fatalf("retry handoff: %v", err)
	}

	if fake.createCalls != 1 {
		t.Fatalf("retry must not rebuild the session, createCalls=%d", fake.createCalls)
	}
	if len(fake.switches) != 2 {
		t.Fatalf("switch should be retried: %v", fake.switches)
	}
	if _, ok, _ := a.loadRestoreJournal("h1"); ok {
		t.Fatal("successful retry must remove the journal")
	}
}

func TestReconcileRestoreJournalsConvergesSweep(t *testing.T) {
	t.Parallel()

	fake := &generationFakeTmux{inside: false, exists: true}
	a := generationTestApp(t, fake)
	saveGenerationSnapshot(t, a, "sweep-run")

	cleaning := *tmux.NewRestoreJournal("sweep-clean", false)
	cleaning.Status = tmux.RestoreStatusCleaning
	cleaning.SessionCreated = true
	if err := a.saveRestoreJournal(cleaning); err != nil {
		t.Fatalf("seed cleaning journal: %v", err)
	}

	running := *tmux.NewRestoreJournal("sweep-run", false)
	running.Status = tmux.RestoreStatusRunning
	running.SessionCreated = true
	running.Windows[0] = &tmux.WindowJournal{Created: true, Panes: 1}
	if err := a.saveRestoreJournal(running); err != nil {
		t.Fatalf("seed running journal: %v", err)
	}

	if err := a.ReconcileRestoreJournals(); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	journals, err := a.listRestoreJournals()
	if err != nil {
		t.Fatal(err)
	}
	if len(journals) != 0 {
		t.Fatalf("all generations must converge, remaining: %+v", journals)
	}
	if !fake.killed("sweep-clean") {
		t.Fatal("cleaning generation should have killed its session")
	}
	if fake.createCalls != 0 {
		t.Fatal("running generation must be resumed without recreation")
	}
}
