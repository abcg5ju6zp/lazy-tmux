package app

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/alchemmist/lazy-tmux/internal/config"
	"github.com/alchemmist/lazy-tmux/internal/picker"
	"github.com/alchemmist/lazy-tmux/internal/snapshot"
	"github.com/alchemmist/lazy-tmux/internal/store"
	"github.com/alchemmist/lazy-tmux/internal/tmux"
)

const (
	appSocketA = "/tmp/tmux-4242/default"
	appSocketB = "/tmp/tmux-4242/work"
)

type scopedTmux struct {
	recordingTmux

	socket      string
	live        map[string]bool
	restored    []snapshot.SessionSnapshot
	killed      []string
	renamed     []string
	newSessions []string
}

func (f *scopedTmux) SocketPath() string { return f.socket }
func (f *scopedTmux) ListSessions() ([]string, error) {
	names := make([]string, 0, len(f.live))
	for name, alive := range f.live {
		if alive {
			names = append(names, name)
		}
	}

	return names, nil
}
func (f *scopedTmux) SessionsLastAttached() map[string]time.Time { return nil }
func (f *scopedTmux) CurrentSession() (string, error)            { return "", nil }
func (f *scopedTmux) SessionExists(name string) bool             { return f.live[name] }
func (f *scopedTmux) KillSession(name string) error {
	f.killed = append(f.killed, name)
	delete(f.live, name)

	return nil
}

func (f *scopedTmux) RenameSession(session, name string) error {
	f.renamed = append(f.renamed, session+">"+name)
	delete(f.live, session)
	f.live[name] = true

	return nil
}

func (f *scopedTmux) NewSession(name string) error {
	f.newSessions = append(f.newSessions, name)
	f.live[name] = true

	return nil
}

func (f *scopedTmux) RestoreSession(
	_ context.Context,
	snap snapshot.SessionSnapshot,
) error {
	if f.live[snap.SessionName] {
		return tmux.ErrSessionExists
	}
	f.restored = append(f.restored, snap)

	return nil
}

func newScopedApp(t *testing.T) (*App, *scopedTmux) {
	t.Helper()

	dir := t.TempDir()
	a := New(config.Config{DataDir: dir, TmuxBin: "tmux"})
	a.serverSocket = appSocketA
	a.store = store.NewForServer(dir, appSocketA)
	fake := &scopedTmux{socket: appSocketA, live: map[string]bool{}}
	a.tmux = fake

	return a, fake
}

func scopedSnapshot(socket, name, marker string, captured time.Time) snapshot.SessionSnapshot {
	return snapshot.SessionSnapshot{
		Version:      snapshot.FormatVersion,
		SessionName:  name,
		ServerSocket: socket,
		CapturedAt:   captured,
		Windows: []snapshot.Window{{
			Index: 0, Name: marker, IsActive: true,
			Panes: []snapshot.Pane{{Index: 0, CurrentCmd: "zsh", CurrentPath: "/tmp"}},
		}},
	}
}

func sessionByID(sessions []picker.Session, serverID, name string) (picker.Session, bool) {
	for _, sess := range sessions {
		if sess.ServerID == serverID && sess.Record.SessionName == name {
			return sess, true
		}
	}

	return picker.Session{}, false
}

// Same-named session saved on two servers: only the current server's copy may
// ever look "restored" while both stay selectable.
func TestPickerSessionsScopesLivenessToCurrentServer(t *testing.T) {
	t.Parallel()

	a, fake := newScopedApp(t)
	if err := a.store.SaveSession(
		scopedSnapshot(appSocketA, "shared", "a", time.Now()),
	); err != nil {
		t.Fatalf("save A: %v", err)
	}
	foreign := store.NewForServer(a.cfg.DataDir, appSocketB)
	if err := foreign.SaveSession(
		scopedSnapshot(appSocketB, "shared", "b", time.Now()),
	); err != nil {
		t.Fatalf("save B: %v", err)
	}
	fake.live["shared"] = true

	sessions, err := a.pickerSessions(DefaultPickerSortOptions())
	if err != nil {
		t.Fatalf("picker sessions: %v", err)
	}
	if len(sessions) != 2 {
		t.Fatalf("expected both records visible, got %d", len(sessions))
	}

	currentID := a.currentServerID()
	foreignID := store.ServerID(appSocketB)

	current, ok := sessionByID(sessions, currentID, "shared")
	if !ok {
		t.Fatalf("current record missing: %+v", sessions)
	}
	if !current.Restored || current.Foreign {
		t.Fatalf("current record must look live: %+v", current)
	}

	other, ok := sessionByID(sessions, foreignID, "shared")
	if !ok {
		t.Fatalf("foreign record missing: %+v", sessions)
	}
	if other.Restored || !other.Foreign || other.Label != "work" {
		t.Fatalf("foreign record must stay offline and labeled: %+v", other)
	}
}

// Deleting another server's record must never kill the current server's
// same-named live session or remove its snapshot.
func TestDeleteForeignRecordIsolatesCurrentServer(t *testing.T) {
	t.Parallel()

	a, fake := newScopedApp(t)
	if err := a.store.SaveSession(
		scopedSnapshot(appSocketA, "shared", "a", time.Now()),
	); err != nil {
		t.Fatalf("save A: %v", err)
	}
	foreign := store.NewForServer(a.cfg.DataDir, appSocketB)
	if err := foreign.SaveSession(
		scopedSnapshot(appSocketB, "shared", "b", time.Now()),
	); err != nil {
		t.Fatalf("save B: %v", err)
	}
	fake.live["shared"] = true

	if err := a.DeleteSession(store.ServerID(appSocketB), "shared"); err != nil {
		t.Fatalf("delete foreign: %v", err)
	}

	if slices.Contains(fake.killed, "shared") {
		t.Fatalf("foreign delete killed the live current-server session: %v", fake.killed)
	}
	exists, err := a.store.SessionExists("shared")
	if err != nil || !exists {
		t.Fatalf("current snapshot must survive: exists=%v err=%v", exists, err)
	}
	foreignStillExists, err := foreign.SessionExists("shared")
	if err != nil || foreignStillExists {
		t.Fatalf("foreign snapshot must be gone: exists=%v err=%v", foreignStillExists, err)
	}
}

// Renaming on a foreign namespace edits only that namespace record.
func TestRenameForeignSessionLeavesCurrentUntouched(t *testing.T) {
	t.Parallel()

	a, fake := newScopedApp(t)
	if err := a.store.SaveSession(
		scopedSnapshot(appSocketA, "shared", "a", time.Now()),
	); err != nil {
		t.Fatalf("save A: %v", err)
	}
	foreign := store.NewForServer(a.cfg.DataDir, appSocketB)
	if err := foreign.SaveSession(
		scopedSnapshot(appSocketB, "shared", "b", time.Now()),
	); err != nil {
		t.Fatalf("save B: %v", err)
	}
	fake.live["shared"] = true

	if err := a.RenameSession(store.ServerID(appSocketB), "shared", "renamed"); err != nil {
		t.Fatalf("rename foreign: %v", err)
	}

	if len(fake.renamed) != 0 {
		t.Fatalf("foreign rename touched tmux: %v", fake.renamed)
	}
	if exists, _ := a.store.SessionExists("shared"); !exists {
		t.Fatal("current snapshot must remain under its name")
	}
	if exists, _ := foreign.SessionExists("shared"); exists {
		t.Fatal("old foreign snapshot must be gone")
	}
	if exists, _ := foreign.SessionExists("renamed"); !exists {
		t.Fatal("renamed foreign snapshot missing")
	}
}

func TestSleepForeignRecordReportedNotRunning(t *testing.T) {
	t.Parallel()

	a, _ := newScopedApp(t)
	foreign := store.NewForServer(a.cfg.DataDir, appSocketB)
	if err := foreign.SaveSession(
		scopedSnapshot(appSocketB, "shared", "b", time.Now()),
	); err != nil {
		t.Fatalf("save B: %v", err)
	}

	err := a.Sleep(store.ServerID(appSocketB), "shared")
	if !errors.Is(err, errNotRunning) {
		t.Fatalf("foreign sleep must report not running, got %v", err)
	}
}

func TestWakeupForeignRecordRefusesWhenCurrentHasSameNameLive(t *testing.T) {
	t.Parallel()

	a, fake := newScopedApp(t)
	foreign := store.NewForServer(a.cfg.DataDir, appSocketB)
	if err := foreign.SaveSession(
		scopedSnapshot(appSocketB, "shared", "b", time.Now()),
	); err != nil {
		t.Fatalf("save B: %v", err)
	}
	fake.live["shared"] = true

	err := a.Wakeup(store.ServerID(appSocketB), "shared")
	if !errors.Is(err, errAlreadyAwake) {
		t.Fatalf("wake must refuse when current server runs the same name, got %v", err)
	}
	if len(fake.restored) != 0 {
		t.Fatalf("nothing should be restored: %+v", fake.restored)
	}
}

func TestRestoreExplicitTargetLoadsForeignSnapshot(t *testing.T) {
	t.Parallel()

	a, fake := newScopedApp(t)
	foreign := store.NewForServer(a.cfg.DataDir, appSocketB)
	if err := foreign.SaveSession(
		scopedSnapshot(appSocketB, "shared", "foreign-win", time.Now()),
	); err != nil {
		t.Fatalf("save B: %v", err)
	}

	err := a.RestoreTarget(PickerTarget{
		SessionName: "shared",
		ServerID:    store.ServerID(appSocketB),
	}, false)
	if err != nil {
		t.Fatalf("restore foreign: %v", err)
	}
	if len(fake.restored) != 1 || fake.restored[0].Windows[0].Name != "foreign-win" {
		t.Fatalf("restored snapshot mismatch: %+v", fake.restored)
	}
}

func TestRestoreBareNameFallsBackToLegacy(t *testing.T) {
	t.Parallel()

	a, fake := newScopedApp(t)
	legacy := store.New(a.cfg.DataDir)
	if err := legacy.SaveSession(snapshot.SessionSnapshot{
		Version:     snapshot.FormatVersion,
		SessionName: "oldie",
		CapturedAt:  time.Now(),
		Windows: []snapshot.Window{{
			Index: 0, Panes: []snapshot.Pane{{Index: 0, CurrentCmd: "zsh"}},
		}},
	}); err != nil {
		t.Fatalf("save legacy: %v", err)
	}

	if err := a.Restore("oldie", false); err != nil {
		t.Fatalf("bare-name restore must find legacy record: %v", err)
	}
	if len(fake.restored) != 1 || fake.restored[0].SessionName != "oldie" {
		t.Fatalf("legacy snapshot not restored: %+v", fake.restored)
	}
}

func TestBootstrapLastStaysInsideCurrentNamespace(t *testing.T) {
	t.Parallel()

	a, fake := newScopedApp(t)
	if err := a.store.SaveSession(
		scopedSnapshot(appSocketA, "current-session", "a", time.Now().Add(-time.Hour)),
	); err != nil {
		t.Fatalf("save A: %v", err)
	}
	foreign := store.NewForServer(a.cfg.DataDir, appSocketB)
	if err := foreign.SaveSession(
		scopedSnapshot(appSocketB, "foreign-newer", "b", time.Now()),
	); err != nil {
		t.Fatalf("save B: %v", err)
	}

	if err := a.Bootstrap("last"); err != nil {
		t.Fatalf("bootstrap: %v", err)
	}
	if len(fake.restored) != 1 {
		t.Fatalf("expected one restore, got %+v", fake.restored)
	}
	if got := fake.restored[0].SessionName; got != "current-session" {
		t.Fatalf("bootstrap must pick current server record, got %q", got)
	}
}

func TestListRecordsFromEveryNamespace(t *testing.T) {
	t.Parallel()

	a, _ := newScopedApp(t)
	if err := a.store.SaveSession(scopedSnapshot(appSocketA, "a", "a", time.Now())); err != nil {
		t.Fatal(err)
	}
	foreign := store.NewForServer(a.cfg.DataDir, appSocketB)
	if err := foreign.SaveSession(scopedSnapshot(appSocketB, "b", "b", time.Now())); err != nil {
		t.Fatal(err)
	}

	records, err := a.ListRecords()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	sockets := map[string]int{}
	for _, rec := range records {
		sockets[rec.ServerSocket]++
	}
	if sockets[appSocketA] != 1 || sockets[appSocketB] != 1 {
		t.Fatalf("expected records from both servers, got %+v", records)
	}
}

// Sanity: current server id is stable and different from the other server.
func TestCurrentServerID(t *testing.T) {
	t.Parallel()

	a, _ := newScopedApp(t)
	if got := a.currentServerID(); got != store.ServerID(appSocketA) {
		t.Fatalf("current id = %q", got)
	}
	if a.ServerSocket() != appSocketA {
		t.Fatalf("socket = %q", a.ServerSocket())
	}
}
