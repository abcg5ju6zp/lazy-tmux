package store

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/alchemmist/lazy-tmux/internal/snapshot"
)

const (
	socketA = "/tmp/tmux-1000/default"
	socketB = "/tmp/tmux-1000/work"
)

func namespacedSnapshot(socket, name, marker string) snapshot.SessionSnapshot {
	return snapshot.SessionSnapshot{
		Version:      snapshot.FormatVersion,
		SessionName:  name,
		ServerSocket: socket,
		CapturedAt:   time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		CurrentPane:  0,
		Windows: []snapshot.Window{{
			Index: 0, Name: marker, IsActive: true,
			Panes: []snapshot.Pane{{
				Index:       0,
				CurrentCmd:  "zsh",
				CurrentPath: "/tmp",
				Scrollback: &snapshot.ScrollbackRef{
					Content: strings.Repeat(marker, 64),
				},
			}},
		}},
	}
}

func TestSameSessionNameOnTwoServersStaysIndependent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	storeA := NewForServer(dir, socketA)
	storeB := NewForServer(dir, socketB)

	if err := storeA.SaveSession(namespacedSnapshot(socketA, "shared", "alpha")); err != nil {
		t.Fatalf("save A: %v", err)
	}
	if err := storeB.SaveSession(namespacedSnapshot(socketB, "shared", "bravo")); err != nil {
		t.Fatalf("save B: %v", err)
	}

	loadedA, err := storeA.LoadSession("shared")
	if err != nil {
		t.Fatalf("load A: %v", err)
	}
	loadedB, err := storeB.LoadSession("shared")
	if err != nil {
		t.Fatalf("load B: %v", err)
	}

	if loadedA.Windows[0].Name != "alpha" || loadedB.Windows[0].Name != "bravo" {
		t.Fatalf("snapshots leaked across namespaces: A=%q B=%q",
			loadedA.Windows[0].Name, loadedB.Windows[0].Name)
	}
	if loadedA.ServerSocket != socketA || loadedB.ServerSocket != socketB {
		t.Fatalf("server stamps wrong: A=%q B=%q", loadedA.ServerSocket, loadedB.ServerSocket)
	}
	if content := loadedA.Windows[0].Panes[0].Scrollback.Content; !strings.Contains(
		content,
		"alpha",
	) {
		t.Fatalf("A scrollback mixed up: %q", content)
	}
	if content := loadedB.Windows[0].Panes[0].Scrollback.Content; !strings.Contains(
		content,
		"bravo",
	) {
		t.Fatalf("B scrollback mixed up: %q", content)
	}

	if storeA.ServerID() == storeB.ServerID() {
		t.Fatal("two sockets must map to distinct namespace ids")
	}
}

func TestServerIDDeterministicAndDistinct(t *testing.T) {
	t.Parallel()

	if ServerID(socketA) != ServerID(socketA) {
		t.Fatal("server id not stable")
	}
	if ServerID(socketA) == ServerID(socketB) {
		t.Fatal("different sockets share an id")
	}
	if ServerID("") != LegacyServerID {
		t.Fatalf("empty socket must map to legacy, got %q", ServerID(""))
	}
	if strings.ContainsAny(ServerID(socketA), `/\`) {
		t.Fatalf("server id must be filesystem-safe: %q", ServerID(socketA))
	}
	if ServerLabel(socketB) != "work" {
		t.Fatalf("label = %q, want work", ServerLabel(socketB))
	}
}

func TestListRecordsMergesNamespacesWithServerStamps(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	storeA := NewForServer(dir, socketA)
	storeB := NewForServer(dir, socketB)

	if err := storeA.SaveSession(namespacedSnapshot(socketA, "alpha", "a")); err != nil {
		t.Fatalf("save A: %v", err)
	}
	if err := storeB.SaveSession(namespacedSnapshot(socketB, "beta", "b")); err != nil {
		t.Fatalf("save B: %v", err)
	}

	records, err := storeA.ListRecords()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(records) != 2 {
		t.Fatalf("expected records from both servers, got %d: %+v", len(records), records)
	}

	byName := map[string]snapshot.Record{}
	for _, rec := range records {
		byName[rec.SessionName] = rec
	}
	if byName["alpha"].ServerSocket != socketA || byName["beta"].ServerSocket != socketB {
		t.Fatalf("records missing server stamps: %+v", byName)
	}
}

func TestDeleteAndAccessTimeNeverCrossNamespaces(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	storeA := NewForServer(dir, socketA)
	storeB := NewForServer(dir, socketB)

	if err := storeA.SaveSession(namespacedSnapshot(socketA, "shared", "alpha")); err != nil {
		t.Fatalf("save A: %v", err)
	}
	if err := storeB.SaveSession(namespacedSnapshot(socketB, "shared", "bravo")); err != nil {
		t.Fatalf("save B: %v", err)
	}

	when := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if err := storeA.MarkSessionAccessed("shared", when); err != nil {
		t.Fatalf("mark A: %v", err)
	}

	if err := storeB.DeleteSession("shared"); err != nil {
		t.Fatalf("delete B: %v", err)
	}

	if exists, err := storeA.SessionExists("shared"); err != nil || !exists {
		t.Fatalf("A record must survive B's delete: exists=%v err=%v", exists, err)
	}

	recs, err := storeA.ListRecords()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	for _, rec := range recs {
		if rec.ServerSocket == socketA && rec.SessionName == "shared" &&
			!rec.LastAccessed.Equal(when) {
			t.Fatalf("A access time changed by B: %v", rec.LastAccessed)
		}
	}
	for _, rec := range recs {
		if rec.ServerSocket == socketB {
			t.Fatalf("B record still listed after delete: %+v", rec)
		}
	}
}

func TestForServerIDAddressesForeignNamespace(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	storeA := NewForServer(dir, socketA)
	if err := storeA.SaveSession(namespacedSnapshot(socketA, "foreign", "f")); err != nil {
		t.Fatalf("save: %v", err)
	}

	foreign, err := storeA.ForServerID(ServerID(socketA))
	if err != nil {
		t.Fatalf("self lookup: %v", err)
	}
	if foreign != storeA {
		t.Fatal("same-id lookup should return the bound store")
	}

	if _, err := storeA.ForServerID("does-not-exist"); err == nil {
		t.Fatal("unknown namespace id must be rejected")
	}
	if _, err := storeA.ForServerID("../escape"); err == nil {
		t.Fatal("namespace id with path separators must be rejected")
	}
}

func TestNamespaceLayoutHoldsIdentityAndLocks(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := NewForServer(dir, socketB)
	if err := s.SaveSession(namespacedSnapshot(socketB, "x", "x")); err != nil {
		t.Fatalf("save: %v", err)
	}

	ns := namespaceFor(dir, socketB)
	for _, path := range []string{
		filepath.Join(ns.root, identityFileName),
		filepath.Join(ns.root, indexFileName),
		filepath.Join(ns.root, sessionsDirName, "x.json"),
		filepath.Join(ns.root, scrollbackDir, "x"),
	} {
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("namespace layout missing %s: %v", path, err)
		}
	}
	if socket := readNamespaceSocket(ns.root); socket != socketB {
		t.Fatalf("identity socket = %q, want %q", socket, socketB)
	}
}
