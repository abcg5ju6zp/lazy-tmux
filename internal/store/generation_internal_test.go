package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/alchemmist/lazy-tmux/internal/snapshot"
)

var (
	errSimulatedDisk   = errors.New("simulated disk failure")
	errSimulatedRename = errors.New("simulated rename failure")
)

// generationSnapshot builds a one-window, one-pane snapshot carrying a marker
// in the window name and optional scrollback content.
func generationSnapshot(
	name, marker string,
	captured time.Time,
	withScrollback bool,
) snapshot.SessionSnapshot {
	var scrollback *snapshot.ScrollbackRef
	if withScrollback {
		scrollback = &snapshot.ScrollbackRef{Content: "scrollback-" + marker + "\n"}
	}

	return snapshot.SessionSnapshot{
		Version:     snapshot.FormatVersion,
		SessionName: name,
		CapturedAt:  captured,
		CurrentWin:  0,
		CurrentPane: 0,
		Windows: []snapshot.Window{{
			Index:      0,
			Name:       "win-" + marker,
			IsActive:   true,
			ActivePane: 0,
			Panes: []snapshot.Pane{{
				Index:       0,
				CurrentPath: "/tmp",
				CurrentCmd:  "zsh",
				Scrollback:  scrollback,
				IsActive:    true,
			}},
		}},
	}
}

type sessionState struct {
	windowMarker     string
	scrollback       string
	capturedAt       time.Time
	scrollbackExists bool
}

func readSessionState(t *testing.T, dir, name string) sessionState {
	t.Helper()

	s := New(dir)

	snap, err := s.LoadSession(name)
	if err != nil {
		t.Fatalf("load session %s: %v", name, err)
	}

	state := sessionState{
		windowMarker: strings.TrimPrefix(snap.Windows[0].Name, "win-"),
		capturedAt:   snap.CapturedAt,
	}

	if sb := snap.Windows[0].Panes[0].Scrollback; sb != nil {
		state.scrollback = sb.Content
	}

	exists, err := s.ScrollbackExists(name)
	if err != nil {
		t.Fatalf("scrollback exists: %v", err)
	}

	state.scrollbackExists = exists

	return state
}

func indexCapturedAt(t *testing.T, dir, name string) time.Time {
	t.Helper()

	records, err := New(dir).ListRecords()
	if err != nil {
		t.Fatalf("list records: %v", err)
	}

	for _, record := range records {
		if record.SessionName == name {
			return record.CapturedAt
		}
	}

	t.Fatalf("no index record for %s", name)

	return time.Time{}
}

func assertSessionState(
	t *testing.T,
	dir, name, marker string,
	captured time.Time,
	hasScrollback bool,
) {
	t.Helper()

	state := readSessionState(t, dir, name)

	if state.windowMarker != marker {
		t.Fatalf("%s window marker = %q, want %q", name, state.windowMarker, marker)
	}

	if !state.capturedAt.Equal(captured) {
		t.Fatalf("%s captured = %v, want %v", name, state.capturedAt, captured)
	}

	wantScrollback := ""
	if hasScrollback {
		wantScrollback = "scrollback-" + marker + "\n"
	}

	if state.scrollback != wantScrollback {
		t.Fatalf("%s scrollback = %q, want %q", name, state.scrollback, wantScrollback)
	}

	if state.scrollbackExists != hasScrollback {
		t.Fatalf("%s scrollbackExists = %v, want %v", name, state.scrollbackExists, hasScrollback)
	}

	if got := indexCapturedAt(t, dir, name); !got.Equal(captured) {
		t.Fatalf("%s index captured = %v, want %v", name, got, captured)
	}
}

func assertNoGenerationResidue(t *testing.T, dir string) {
	t.Helper()

	if _, err := os.Stat(filepath.Join(dir, ".txn")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transaction dir left behind: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "index.json.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("staged index file left behind: %v", err)
	}

	sessionsDir := filepath.Join(dir, sessionsDirName)

	sessionEntries, err := os.ReadDir(sessionsDir)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read sessions dir: %v", err)
	}

	for _, entry := range sessionEntries {
		name := entry.Name()
		if strings.HasSuffix(name, ".tmp") || strings.Contains(name, ".gen-") {
			t.Fatalf("unexpected session residue %q", name)
		}
	}

	scrollRoot := filepath.Join(dir, scrollbackDir)

	scrollEntries, err := os.ReadDir(scrollRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("read scrollback dir: %v", err)
	}

	for _, entry := range scrollEntries {
		name := entry.Name()
		if strings.HasSuffix(name, ".tmp") || strings.HasSuffix(name, ".bak") {
			t.Fatalf("unexpected scrollback residue %q", name)
		}
	}
}

func seedGeneration(t *testing.T, dir, marker string, captured time.Time, names ...string) {
	t.Helper()

	snaps := make([]snapshot.SessionSnapshot, 0, len(names))
	for _, name := range names {
		snaps = append(snaps, generationSnapshot(name, marker, captured, true))
	}

	if err := New(dir).SaveSessions(snaps); err != nil {
		t.Fatalf("seed generation: %v", err)
	}
}

func nextGenerationSnapshots(
	captured time.Time,
	names ...string,
) []snapshot.SessionSnapshot {
	snaps := make([]snapshot.SessionSnapshot, 0, len(names))
	for _, name := range names {
		snaps = append(snaps, generationSnapshot(name, "new", captured, true))
	}

	return snaps
}

func TestSaveSessionsBatchRoundTrip(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := New(dir)

	old := time.Unix(1000, 0).UTC()
	seedGeneration(t, dir, "old", old, "alpha", "beta")

	records, err := s.ListRecords()
	if err != nil {
		t.Fatalf("list records: %v", err)
	}

	if len(records) != 2 {
		t.Fatalf("expected 2 records, got %d", len(records))
	}

	assertSessionState(t, dir, "alpha", "old", old, true)
	assertSessionState(t, dir, "beta", "old", old, true)
	assertNoGenerationResidue(t, dir)

	// A session that vanishes between cycles keeps its previous snapshot.
	only := time.Unix(2000, 0).UTC()
	if err := s.SaveSessions(nextGenerationSnapshots(only, "alpha")); err != nil {
		t.Fatalf("second generation: %v", err)
	}

	assertSessionState(t, dir, "alpha", "new", only, true)
	assertSessionState(t, dir, "beta", "old", old, true)

	records, err = s.ListRecords()
	if err != nil {
		t.Fatalf("list records: %v", err)
	}

	if len(records) != 2 {
		t.Fatalf("vanished session must stay in the index, got %d records", len(records))
	}

	assertNoGenerationResidue(t, dir)
}

func TestEmptySaveSessionsTouchesNothing(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if err := New(dir).SaveSessions(nil); err != nil {
		t.Fatalf("empty save: %v", err)
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("read data dir: %v", err)
	}

	if len(entries) != 0 {
		t.Fatalf("empty save created artifacts: %v", entries)
	}
}

func TestSaveSessionsStagingFailureKeepsPreviousGeneration(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	old := time.Unix(1000, 0).UTC()
	seedGeneration(t, dir, "old", old, "alpha", "beta", "gamma")

	s := New(dir)
	s.genHook = &generationHooks{
		beforeScrollbackFile: func(sessionName, fileName string) error {
			if sessionName == "beta" {
				return errSimulatedDisk
			}

			return nil
		},
	}

	newTime := time.Unix(2000, 0).UTC()
	err := s.SaveSessions(nextGenerationSnapshots(newTime, "alpha", "beta", "gamma"))
	if err == nil {
		t.Fatal("expected staging failure")
	}

	for _, name := range []string{"alpha", "beta", "gamma"} {
		assertSessionState(t, dir, name, "old", old, true)
	}

	assertNoGenerationResidue(t, dir)
}

func TestSaveSessionsPromoteFailureRollsBack(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	old := time.Unix(1000, 0).UTC()
	seedGeneration(t, dir, "old", old, "alpha", "beta")

	s := New(dir)
	s.genHook = &generationHooks{
		beforePromote: func(sessionName, target string) error {
			if sessionName == "beta" && target == promoteTargetScrollback {
				return errSimulatedRename
			}

			return nil
		},
	}

	newTime := time.Unix(2000, 0).UTC()
	if err := s.SaveSessions(nextGenerationSnapshots(newTime, "alpha", "beta")); err == nil {
		t.Fatal("expected promote failure")
	}

	for _, name := range []string{"alpha", "beta"} {
		assertSessionState(t, dir, name, "old", old, true)
	}

	assertNoGenerationResidue(t, dir)
}

func TestGenerationCrashRecoveryPhases(t *testing.T) {
	t.Parallel()

	rollbackPhases := []string{
		phaseStaged,
		phaseScrollbackLive,
		phaseSessionsLive,
		phaseIndexReady,
	}
	finishPhases := []string{"index-published", phaseIndexed}

	for _, phase := range slices.Concat(rollbackPhases, finishPhases) {
		t.Run(phase, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			old := time.Unix(1000, 0).UTC()
			seedGeneration(t, dir, "old", old, "alpha", "beta")

			s := New(dir)
			s.genHook = &generationHooks{
				afterPhase: func(reached string) error {
					if reached == phase {
						return errSimulatedGenerationCrash
					}

					return nil
				},
			}

			newTime := time.Unix(2000, 0).UTC()
			err := s.SaveSessions(nextGenerationSnapshots(newTime, "alpha", "beta"))
			if !errors.Is(err, errSimulatedGenerationCrash) {
				t.Fatalf("expected simulated crash, got %v", err)
			}

			// A fresh process (new store) converges the leftover generation.
			recoverer := New(dir)

			if err := recoverer.RecoverGenerations(); err != nil {
				t.Fatalf("recover: %v", err)
			}

			// Recovery is idempotent: running it again changes nothing.
			if err := recoverer.RecoverGenerations(); err != nil {
				t.Fatalf("second recover: %v", err)
			}

			wantMarker := "old"
			wantTime := old

			if slices.Contains(finishPhases, phase) {
				wantMarker = "new"
				wantTime = newTime
			}

			assertSessionState(t, dir, "alpha", wantMarker, wantTime, true)
			assertSessionState(t, dir, "beta", wantMarker, wantTime, true)
			assertNoGenerationResidue(t, dir)
		})
	}
}

func TestCrashMidScrollbackPromotionRollsBack(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	old := time.Unix(1000, 0).UTC()
	seedGeneration(t, dir, "old", old, "alpha", "beta")

	s := New(dir)
	s.genHook = &generationHooks{
		beforePromote: func(sessionName, target string) error {
			// alpha promotes first (sorted capture order from ListSessions is
			// stable; force the crash on the second session regardless).
			if sessionName == "beta" && target == promoteTargetScrollback {
				return errSimulatedGenerationCrash
			}

			return nil
		},
	}

	newTime := time.Unix(2000, 0).UTC()
	if err := s.SaveSessions(
		nextGenerationSnapshots(newTime, "alpha", "beta"),
	); !errors.Is(
		err,
		errSimulatedGenerationCrash,
	) {
		t.Fatalf("expected simulated crash, got %v", err)
	}

	if err := New(dir).RecoverGenerations(); err != nil {
		t.Fatalf("recover: %v", err)
	}

	assertSessionState(t, dir, "alpha", "old", old, true)
	assertSessionState(t, dir, "beta", "old", old, true)
	assertNoGenerationResidue(t, dir)
}

func TestRepeatedCyclesLeaveSingleActiveSnapshot(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	s := New(dir)

	for iteration := range 5 {
		captured := time.Unix(int64(1000+iteration), 0).UTC()
		snaps := []snapshot.SessionSnapshot{
			generationSnapshot("alpha", fmt.Sprintf("m%d", iteration), captured, true),
			generationSnapshot("beta", fmt.Sprintf("m%d", iteration), captured, true),
		}

		if err := s.SaveSessions(snaps); err != nil {
			t.Fatalf("generation %d: %v", iteration, err)
		}
	}

	entries, err := os.ReadDir(filepath.Join(dir, sessionsDirName))
	if err != nil {
		t.Fatalf("read sessions dir: %v", err)
	}

	if len(entries) != 2 {
		t.Fatalf("expected exactly 2 active session files, got %v", entries)
	}

	scrollEntries, err := os.ReadDir(filepath.Join(dir, scrollbackDir))
	if err != nil {
		t.Fatalf("read scrollback dir: %v", err)
	}

	if len(scrollEntries) != 2 {
		t.Fatalf("expected exactly 2 scrollback dirs, got %v", scrollEntries)
	}

	records, err := s.ListRecords()
	if err != nil {
		t.Fatalf("list records: %v", err)
	}

	if len(records) != 2 {
		t.Fatalf("expected 2 index records, got %d", len(records))
	}

	last := time.Unix(1004, 0).UTC()
	assertSessionState(t, dir, "alpha", "m4", last, true)
	assertSessionState(t, dir, "beta", "m4", last, true)
	assertNoGenerationResidue(t, dir)
}

func TestManualSaveOnlyAffectsTargetSession(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	old := time.Unix(1000, 0).UTC()
	seedGeneration(t, dir, "old", old, "alpha", "beta", "gamma")

	if err := New(dir).MarkSessionAccessed("alpha", time.Unix(500, 0).UTC()); err != nil {
		t.Fatalf("mark accessed: %v", err)
	}

	manualTime := time.Unix(3000, 0).UTC()
	manual := generationSnapshot("beta", "manual", manualTime, true)

	if err := New(dir).SaveSession(manual); err != nil {
		t.Fatalf("manual save: %v", err)
	}

	assertSessionState(t, dir, "alpha", "old", old, true)
	assertSessionState(t, dir, "beta", "manual", manualTime, true)
	assertSessionState(t, dir, "gamma", "old", old, true)

	records, err := New(dir).ListRecords()
	if err != nil {
		t.Fatalf("list records: %v", err)
	}

	byName := map[string]snapshot.Record{}
	for _, record := range records {
		byName[record.SessionName] = record
	}

	if len(byName) != 3 {
		t.Fatalf("expected 3 records, got %d", len(byName))
	}

	if !byName["alpha"].LastAccessed.Equal(time.Unix(500, 0).UTC()) {
		t.Fatalf("manual save dropped alpha LastAccessed: %v", byName["alpha"].LastAccessed)
	}

	assertNoGenerationResidue(t, dir)
}

func TestScrollbackRemovalIsTransactional(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	old := time.Unix(1000, 0).UTC()
	seedGeneration(t, dir, "old", old, "alpha")

	// New generation without scrollback removes the live scrollback dir.
	noScrollback := generationSnapshot("alpha", "new", time.Unix(2000, 0).UTC(), false)
	if err := New(dir).SaveSessions([]snapshot.SessionSnapshot{noScrollback}); err != nil {
		t.Fatalf("save without scrollback: %v", err)
	}

	assertSessionState(t, dir, "alpha", "new", time.Unix(2000, 0).UTC(), false)
	assertNoGenerationResidue(t, dir)
}

func TestScrollbackRemovalRolledBackAfterCrash(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	old := time.Unix(1000, 0).UTC()
	seedGeneration(t, dir, "old", old, "alpha", "beta")

	s := New(dir)
	s.genHook = &generationHooks{
		afterPhase: func(phase string) error {
			if phase == phaseScrollbackLive {
				return errSimulatedGenerationCrash
			}

			return nil
		},
	}

	snaps := []snapshot.SessionSnapshot{
		generationSnapshot("alpha", "new", time.Unix(2000, 0).UTC(), false),
		generationSnapshot("beta", "new", time.Unix(2000, 0).UTC(), false),
	}

	if err := s.SaveSessions(snaps); !errors.Is(err, errSimulatedGenerationCrash) {
		t.Fatalf("expected simulated crash, got %v", err)
	}

	if err := New(dir).RecoverGenerations(); err != nil {
		t.Fatalf("recover: %v", err)
	}

	assertSessionState(t, dir, "alpha", "old", old, true)
	assertSessionState(t, dir, "beta", "old", old, true)
	assertNoGenerationResidue(t, dir)
}

func TestRecoverySweepsLegacyArtifacts(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(dir, sessionsDirName), defaultDirPerm); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(dir, scrollbackDir), scrollbackDirPerm); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Legacy fixed-name staging files and directories.
	if err := os.WriteFile(
		filepath.Join(dir, sessionsDirName, "keep.json.tmp"),
		[]byte("{}"),
		defaultFilePerm,
	); err != nil {
		t.Fatalf("write legacy tmp: %v", err)
	}

	if err := os.WriteFile(
		filepath.Join(dir, "index.json.tmp"),
		[]byte("{}"),
		defaultFilePerm,
	); err != nil {
		t.Fatalf("write legacy index tmp: %v", err)
	}

	if err := os.MkdirAll(
		filepath.Join(dir, scrollbackDir, "ghost.tmp"),
		scrollbackDirPerm,
	); err != nil {
		t.Fatalf("mkdir legacy scrollback tmp: %v", err)
	}

	// Legacy backup whose live directory vanished must be restored, not lost.
	restorable := filepath.Join(dir, scrollbackDir, "restorable.bak")
	if err := os.MkdirAll(restorable, scrollbackDirPerm); err != nil {
		t.Fatalf("mkdir legacy backup: %v", err)
	}

	if err := os.WriteFile(
		filepath.Join(restorable, "w0_p0.log"),
		[]byte("kept"),
		0o600,
	); err != nil {
		t.Fatalf("write legacy backup content: %v", err)
	}

	// Named repair backups must survive sweeping.
	repairBackup := filepath.Join(dir, sessionsDirName, "keep.json.pre-agent-repair.bak")
	if err := os.WriteFile(repairBackup, []byte("{}"), defaultFilePerm); err != nil {
		t.Fatalf("write repair backup: %v", err)
	}

	if err := New(dir).RecoverGenerations(); err != nil {
		t.Fatalf("recover: %v", err)
	}

	if _, err := os.Stat(
		filepath.Join(dir, sessionsDirName, "keep.json.tmp"),
	); !errors.Is(
		err,
		os.ErrNotExist,
	) {
		t.Fatalf("legacy session tmp not removed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, "index.json.tmp")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("legacy index tmp not removed: %v", err)
	}

	if _, err := os.Stat(
		filepath.Join(dir, scrollbackDir, "ghost.tmp"),
	); !errors.Is(
		err,
		os.ErrNotExist,
	) {
		t.Fatalf("legacy scrollback tmp not removed: %v", err)
	}

	restored, err := os.ReadFile(filepath.Join(dir, scrollbackDir, "restorable", "w0_p0.log"))
	if err != nil {
		t.Fatalf("legacy backup not restored: %v", err)
	}

	if string(restored) != "kept" {
		t.Fatalf("restored content = %q", restored)
	}

	if _, err := os.Stat(repairBackup); err != nil {
		t.Fatalf("repair backup must survive recovery: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, ".txn")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transaction dir should not remain: %v", err)
	}
}

func TestRecoveryReconcilesOrphanGenerationBackups(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	const id = "gen-20260102T030405.000000000-deadbeef"

	if err := os.MkdirAll(filepath.Join(dir, txnDirName, id), defaultDirPerm); err != nil {
		t.Fatalf("mkdir gen: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(dir, sessionsDirName), defaultDirPerm); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	if err := os.MkdirAll(filepath.Join(dir, scrollbackDir), scrollbackDirPerm); err != nil {
		t.Fatalf("mkdir: %v", err)
	}

	// Orphan JSON backup with no live file: restore.
	jsonBackup := filepath.Join(dir, sessionsDirName, "lost.json."+id+".bak")
	if err := os.WriteFile(jsonBackup, []byte("{}"), defaultFilePerm); err != nil {
		t.Fatalf("write orphan json backup: %v", err)
	}

	// Orphan scrollback backup while a newer live dir exists: discard backup.
	newerLive := filepath.Join(dir, scrollbackDir, "newer")
	if err := os.MkdirAll(newerLive, scrollbackDirPerm); err != nil {
		t.Fatalf("mkdir live: %v", err)
	}

	olderBackup := filepath.Join(dir, scrollbackDir, "newer."+id+".bak")
	if err := os.MkdirAll(olderBackup, scrollbackDirPerm); err != nil {
		t.Fatalf("mkdir orphan scrollback backup: %v", err)
	}

	if err := New(dir).RecoverGenerations(); err != nil {
		t.Fatalf("recover: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, sessionsDirName, "lost.json")); err != nil {
		t.Fatalf("orphan json backup should be restored when live is missing: %v", err)
	}

	if _, err := os.Stat(olderBackup); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("stale backup with newer live should be removed: %v", err)
	}

	if _, err := os.Stat(filepath.Join(dir, ".txn")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transaction dir should not remain: %v", err)
	}
}

func TestRecoverOnCleanDirectoryCreatesNoSnapshots(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	if err := New(dir).RecoverGenerations(); err != nil {
		t.Fatalf("recover clean dir: %v", err)
	}

	for _, unwanted := range []string{txnDirName, sessionsDirName, scrollbackDir, indexFileName} {
		if _, err := os.Stat(filepath.Join(dir, unwanted)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("clean recovery created %s: %v", unwanted, err)
		}
	}
}

func TestConcurrentGenerationsStayConsistent(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	base := time.Unix(1000, 0).UTC()
	seedGeneration(t, dir, "seed", base, "alpha", "beta", "gamma")

	var group sync.WaitGroup

	for iteration := range 6 {
		group.Go(func() {
			s := New(dir)
			captured := time.Unix(int64(2000+iteration), 0).UTC()

			err := s.SaveSessions([]snapshot.SessionSnapshot{
				generationSnapshot("alpha", fmt.Sprintf("batch%d", iteration), captured, true),
				generationSnapshot("beta", fmt.Sprintf("batch%d", iteration), captured, true),
			})
			if err != nil {
				t.Errorf("batch generation %d: %v", iteration, err)
			}
		})

		group.Go(func() {
			s := New(dir)
			captured := time.Unix(int64(3000+iteration), 0).UTC()

			err := s.SaveSession(
				generationSnapshot("gamma", fmt.Sprintf("manual%d", iteration), captured, true),
			)
			if err != nil {
				t.Errorf("manual generation %d: %v", iteration, err)
			}
		})
	}

	group.Wait()

	for _, name := range []string{"alpha", "beta", "gamma"} {
		state := readSessionState(t, dir, name)
		if state.windowMarker == "seed" {
			t.Fatalf("%s never reached a new generation", name)
		}

		if state.scrollback != "scrollback-"+state.windowMarker+"\n" {
			t.Fatalf("%s mixed scrollback %q with marker %q",
				name, state.scrollback, state.windowMarker)
		}
	}

	assertNoGenerationResidue(t, dir)
}

func TestSaveSessionsEmptyNameRejected(t *testing.T) {
	t.Parallel()

	err := New(t.TempDir()).SaveSessions([]snapshot.SessionSnapshot{{SessionName: "ok"}, {}})
	if !errors.Is(err, errEmptySessionName) {
		t.Fatalf("expected empty session name error, got %v", err)
	}
}

// A failure after the index commit point must not roll the new generation
// back: the new index is live, so recovery finishes cleanup instead.
func TestPostCommitCleanupFailureDoesNotRollBack(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	old := time.Unix(1000, 0).UTC()
	seedGeneration(t, dir, "old", old, "alpha", "beta")

	s := New(dir)
	s.genHook = &generationHooks{
		afterPhase: func(phase string) error {
			if phase == phaseIndexed {
				return errSimulatedDisk
			}

			return nil
		},
	}

	newTime := time.Unix(2000, 0).UTC()
	if err := s.SaveSessions(nextGenerationSnapshots(newTime, "alpha", "beta")); err == nil {
		t.Fatal("expected post-commit cleanup failure")
	}

	// Recovery must finish (keep new generation, remove backups and the gen
	// directory), never restore old artifacts under the new index.
	if err := New(dir).RecoverGenerations(); err != nil {
		t.Fatalf("recover: %v", err)
	}

	assertSessionState(t, dir, "alpha", "new", newTime, true)
	assertSessionState(t, dir, "beta", "new", newTime, true)
	assertNoGenerationResidue(t, dir)
}
