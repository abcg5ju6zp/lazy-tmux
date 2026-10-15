package store

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/alchemmist/lazy-tmux/internal/snapshot"
)

// writeLegacyLayout creates a pre-namespace data directory: index.json,
// sessions/ and scrollback/ directly under the data root.
func writeLegacyLayout(t *testing.T, dir string, snaps []snapshot.SessionSnapshot) {
	t.Helper()

	if err := os.MkdirAll(filepath.Join(dir, sessionsDirName), defaultDirPerm); err != nil {
		t.Fatalf("mkdir sessions: %v", err)
	}
	if err := os.MkdirAll(filepath.Join(dir, scrollbackDir), scrollbackDirPerm); err != nil {
		t.Fatalf("mkdir scrollback: %v", err)
	}

	idx := snapshot.Index{
		Version:  2,
		Updated:  time.Now().UTC(),
		Sessions: map[string]snapshot.Record{},
	}

	for _, snap := range snaps {
		snap.Version = 2
		snap.ServerSocket = ""
		writeLegacyScrollback(t, dir, snap)

		data, err := json.MarshalIndent(snap, "", "  ")
		if err != nil {
			t.Fatalf("marshal legacy snapshot: %v", err)
		}
		path := filepath.Join(
			dir,
			sessionsDirName,
			sanitizeName(snap.SessionName)+".json",
		)
		if err := os.WriteFile(path, append(data, '\n'), defaultFilePerm); err != nil {
			t.Fatalf("write legacy snapshot: %v", err)
		}

		panes := 0
		for _, w := range snap.Windows {
			panes += len(w.Panes)
		}
		idx.Sessions[snap.SessionName] = snapshot.Record{
			SessionName:  snap.SessionName,
			File:         path, // old format stored absolute paths
			CapturedAt:   snap.CapturedAt,
			LastAccessed: time.Date(2025, 6, 7, 8, 9, 0, 0, time.UTC),
			Windows:      len(snap.Windows),
			Panes:        panes,
		}
	}

	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		t.Fatalf("marshal legacy index: %v", err)
	}
	if err := os.WriteFile(
		filepath.Join(dir, indexFileName),
		append(data, '\n'),
		defaultFilePerm,
	); err != nil {
		t.Fatalf("write legacy index: %v", err)
	}
}

func safeNameOrPanic(name string) string {
	safe, err := safeScrollbackSessionName(name)
	if err != nil {
		panic(err)
	}

	return safe
}

// writeLegacyScrollback materializes pane scrollbacks as flat-layout files and
// rewrites the snapshots to reference them, like the pre-migration writer.
func writeLegacyScrollback(
	t *testing.T,
	dir string,
	snap snapshot.SessionSnapshot,
) {
	t.Helper()

	for windowIndex := range snap.Windows {
		for paneIndex := range snap.Windows[windowIndex].Panes {
			pane := &snap.Windows[windowIndex].Panes[paneIndex]
			if pane.Scrollback == nil || pane.Scrollback.Content == "" {
				continue
			}

			scrollDir := filepath.Join(
				dir,
				scrollbackDir,
				safeNameOrPanic(snap.SessionName),
			)
			if err := os.MkdirAll(scrollDir, scrollbackDirPerm); err != nil {
				t.Fatalf("mkdir scrollback session: %v", err)
			}
			logName := "w" + itoa(windowIndex) + "_p" + itoa(paneIndex) + ".log"
			logPath := filepath.Join(scrollDir, logName)
			if err := os.WriteFile(
				logPath,
				[]byte(pane.Scrollback.Content),
				scrollbackFilePerm,
			); err != nil {
				t.Fatalf("write legacy scrollback: %v", err)
			}
			pane.Scrollback.Ref = filepath.Join(
				scrollbackDir,
				safeNameOrPanic(snap.SessionName),
				logName,
			)
			pane.Scrollback.Bytes = len(pane.Scrollback.Content)
			pane.Scrollback.Lines = countLines(pane.Scrollback.Content)
			pane.Scrollback.Content = ""
		}
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	out := ""
	for n > 0 {
		out = string(rune('0'+n%10)) + out
		n /= 10
	}

	return out
}

func TestLegacyLayoutMigratesIntoCurrentServerNamespace(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeLegacyLayout(t, dir, []snapshot.SessionSnapshot{
		legacyScrollbackSnapshot("alpha", "A"),
		legacyScrollbackSnapshot("beta", "B"),
	})

	s := NewForServer(dir, socketA)

	records, err := s.ListRecords()
	if err != nil {
		t.Fatalf("list after migration: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("expected 2 migrated records, got %d", len(records))
	}
	for _, rec := range records {
		if rec.ServerSocket != socketA {
			t.Fatalf("record %s attributed to %q, want %q",
				rec.SessionName, rec.ServerSocket, socketA)
		}
		if rec.LastAccessed.IsZero() {
			t.Fatalf("record %s lost its last-accessed time", rec.SessionName)
		}
	}

	if legacyTopLevelPresent(dir) {
		t.Fatal("flat legacy layout should be gone after migration")
	}

	loaded, err := s.LoadSession("alpha")
	if err != nil {
		t.Fatalf("load migrated: %v", err)
	}
	if loaded.ServerSocket != socketA {
		t.Fatalf("migrated snapshot not stamped: %q", loaded.ServerSocket)
	}
	content := loaded.Windows[0].Panes[0].Scrollback.Content
	if content != "scrollback-A\n" {
		t.Fatalf("migrated scrollback lost: %q", content)
	}

	if _, err := s.LoadSession("beta"); err != nil {
		t.Fatalf("load beta: %v", err)
	}
}

func legacyScrollbackSnapshot(name, marker string) snapshot.SessionSnapshot {
	return snapshot.SessionSnapshot{
		Version:     2,
		SessionName: name,
		CapturedAt:  time.Date(2026, 2, 3, 4, 5, 6, 0, time.UTC),
		Windows: []snapshot.Window{{
			Index: 0, Name: "win", Panes: []snapshot.Pane{{
				Index: 0, CurrentCmd: "zsh",
				Scrollback: &snapshot.ScrollbackRef{Content: "scrollback-" + marker + "\n"},
			}},
		}},
	}
}

func TestLegacyLayoutParksThenDrainsAcrossInvocations(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeLegacyLayout(t, dir, []snapshot.SessionSnapshot{
		legacyScrollbackSnapshot("parked", "P"),
	})

	// A store without a real server only parks the flat layout in legacy/.
	parked := New(dir)
	records, err := parked.ListRecords()
	if err != nil {
		t.Fatalf("park list: %v", err)
	}
	if len(records) != 1 || records[0].ServerSocket != "" {
		t.Fatalf("parked record should remain legacy: %+v", records)
	}
	if _, err := parked.LoadSession("parked"); err != nil {
		t.Fatalf("parked record stays selectable: %v", err)
	}
	if !dirExists(filepath.Join(dir, LegacyServerID)) {
		t.Fatal("parked data should live under legacy/")
	}

	// The real server drains the legacy namespace on its next invocation.
	s := NewForServer(dir, socketA)
	records, err = s.ListRecords()
	if err != nil {
		t.Fatalf("drain list: %v", err)
	}
	if len(records) != 1 || records[0].ServerSocket != socketA {
		t.Fatalf("drained record should belong to %s: %+v", socketA, records)
	}
	if _, err := s.LoadSession("parked"); err != nil {
		t.Fatalf("drained record loadable: %v", err)
	}
	if dirExists(filepath.Join(dir, LegacyServerID)) {
		t.Fatal("drained legacy namespace should be cleaned up")
	}
}

func TestLegacyRecordCollisionStaysSelectableInLegacy(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// Current server already owns "shared".
	current := NewForServer(dir, socketA)
	if err := current.SaveSession(namespacedSnapshot(socketA, "shared", "current")); err != nil {
		t.Fatalf("save current: %v", err)
	}

	// An unmigrated flat layout also contains "shared".
	writeLegacyLayout(t, dir, []snapshot.SessionSnapshot{
		legacyScrollbackSnapshot("shared", "legacy"),
	})

	records, err := current.ListRecords()
	if err != nil {
		t.Fatalf("list with collision: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("collision must keep both records, got %d", len(records))
	}

	sockets := map[string]int{}
	for _, rec := range records {
		sockets[rec.ServerSocket]++
	}
	if sockets[socketA] != 1 || sockets[""] != 1 {
		t.Fatalf("expected one current + one legacy record, got %+v", records)
	}

	loaded, err := current.LoadSession("shared")
	if err != nil {
		t.Fatalf("load current: %v", err)
	}
	if loaded.Windows[0].Name != "current" {
		t.Fatalf("current snapshot must win in its namespace: %q", loaded.Windows[0].Name)
	}

	legacy, err := current.ForServerID(LegacyServerID)
	if err != nil {
		t.Fatalf("legacy namespace: %v", err)
	}
	legacySnap, err := legacy.LoadSession("shared")
	if err != nil {
		t.Fatalf("legacy copy must stay selectable: %v", err)
	}
	if legacySnap.Windows[0].Name != "win" {
		t.Fatalf("unexpected legacy snapshot: %+v", legacySnap.Windows)
	}
}

func TestMigrationIsIdempotent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	writeLegacyLayout(t, dir, []snapshot.SessionSnapshot{
		legacyScrollbackSnapshot("alpha", "A"),
		legacyScrollbackSnapshot("beta", "B"),
	})

	s := NewForServer(dir, socketA)
	for range 3 {
		records, err := s.ListRecords()
		if err != nil {
			t.Fatalf("repeat migration: %v", err)
		}
		if len(records) != 2 {
			t.Fatalf("repeated migration duplicated records: %d", len(records))
		}
	}
}
