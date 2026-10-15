package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/alchemmist/lazy-tmux/internal/snapshot"
)

const (
	migrationLockName  = ".migration.lock"
	migrationStateName = ".migration.json"
)

type migrationState struct {
	ServerID  string    `json:"server_id"`
	Socket    string    `json:"socket_path,omitempty"`
	Claimed   []string  `json:"claimed,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// ensureMigrated moves a pre-namespace data directory (index.json, sessions/,
// scrollback/ at the data root) into per-server namespaces. It is idempotent
// and safe to interrupt: every claimed record is reconciled on the next run,
// snapshots are never deleted before their destination is durable, and names
// that already exist in the target namespace stay behind as selectable legacy
// records.
func (s *Store) ensureMigrated(serverNS namespace) error {
	if !s.migrationNeeded(serverNS) {
		return nil
	}

	unlock, err := lockMigration(s.baseDir)
	if err != nil {
		return err
	}
	defer unlock()

	if !s.migrationNeeded(serverNS) {
		return nil
	}

	legacy := legacyNamespace(s.baseDir)
	if legacyTopLevelPresent(s.baseDir) {
		parkErr := parkLegacyLayout(s.baseDir, legacy)
		if parkErr != nil {
			return parkErr
		}
	}

	// Without a real current server the records can only be parked in the
	// legacy namespace; draining is retried on the next invocation bound to a
	// tmux server.
	if serverNS.id == LegacyServerID {
		return nil
	}

	if !legacyHasRecords(legacy) || s.legacyDrainMarkerExists(serverNS) {
		return nil
	}

	leftover, err := s.drainLegacyNamespaces(serverNS, legacy)
	if err != nil {
		return err
	}

	// Only mark the drain done when collision records remain; a fully drained
	// legacy namespace is removed entirely so later starts see a clean dir.
	if leftover {
		return s.writeLegacyDrainMarker(serverNS)
	}

	return nil
}

// migrationNeeded reports whether parking or draining is pending. Each real
// server drains the legacy namespace once; records that collide with its own
// stay behind in legacy/ and are skipped on later runs.
func (s *Store) migrationNeeded(serverNS namespace) bool {
	if legacyTopLevelPresent(s.baseDir) {
		return true
	}

	if serverNS.id == LegacyServerID {
		return false
	}

	legacy := legacyNamespace(s.baseDir)

	return legacyHasRecords(legacy) && !s.legacyDrainMarkerExists(serverNS)
}

func legacyHasRecords(legacy namespace) bool {
	entries, err := os.ReadDir(legacy.sessionsDir())
	if err != nil {
		return false
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			return true
		}
	}

	return false
}

func (s *Store) legacyDrainMarkerPath(serverNS namespace) string {
	return filepath.Join(s.baseDir, LegacyServerID, ".drained-"+serverNS.id)
}

func (s *Store) legacyDrainMarkerExists(serverNS namespace) bool {
	_, err := os.Stat(s.legacyDrainMarkerPath(serverNS))

	return err == nil
}

func (s *Store) writeLegacyDrainMarker(serverNS namespace) error {
	path := s.legacyDrainMarkerPath(serverNS)
	mkdirErr := os.MkdirAll(filepath.Dir(path), defaultDirPerm)
	if mkdirErr != nil {
		return fmt.Errorf("create legacy dir for marker: %w", mkdirErr)
	}
	writeErr := os.WriteFile(path, []byte("drained\n"), defaultFilePerm)
	if writeErr != nil {
		return fmt.Errorf("write drain marker: %w", writeErr)
	}

	return nil
}

func lockMigration(baseDir string) (func(), error) {
	mkdirErr := os.MkdirAll(baseDir, defaultDirPerm)
	if mkdirErr != nil {
		return nil, fmt.Errorf("create data dir: %w", mkdirErr)
	}

	path := filepath.Join(baseDir, migrationLockName)

	file, err := os.OpenFile( // #nosec G304 -- fixed migration lock under the data dir
		path,
		os.O_CREATE|os.O_RDWR,
		0o600,
	)
	if err != nil {
		return nil, fmt.Errorf("open migration lock: %w", err)
	}

	lockErr := syscall.Flock(int(file.Fd()), syscall.LOCK_EX)
	if lockErr != nil {
		_ = file.Close()

		return nil, fmt.Errorf("acquire migration lock: %w", lockErr)
	}

	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

func legacyTopLevelPresent(baseDir string) bool {
	for _, name := range []string{indexFileName, sessionsDirName, scrollbackDir} {
		_, statErr := os.Stat(filepath.Join(baseDir, name))
		if statErr == nil {
			return true
		}
	}

	return false
}

// parkLegacyLayout moves the flat pre-namespace layout into the legacy
// namespace directory. Files and directory children that already exist from
// an interrupted run are never overwritten, so parking always resumes.
func parkLegacyLayout(baseDir string, legacy namespace) error {
	mkdirErr := os.MkdirAll(legacy.root, defaultDirPerm)
	if mkdirErr != nil {
		return fmt.Errorf("create legacy namespace: %w", mkdirErr)
	}

	moveIndexErr := moveFileIfPresent(
		filepath.Join(baseDir, indexFileName),
		legacy.indexPath(),
	)
	if moveIndexErr != nil {
		return moveIndexErr
	}

	for _, item := range []struct {
		from, dest string
	}{
		{filepath.Join(baseDir, sessionsDirName), legacy.sessionsDir()},
		{filepath.Join(baseDir, scrollbackDir), legacy.scrollRoot()},
	} {
		mergeErr := mergeDirChildren(item.from, item.dest)
		if mergeErr != nil {
			return mergeErr
		}
	}

	// Pre-namespace per-session locks are stale once the layout moved.
	_ = os.RemoveAll(filepath.Join(baseDir, sessionLocksDirName))

	return nil
}

func moveFileIfPresent(from, dest string) error {
	_, sourceErr := os.Stat(from)
	if sourceErr != nil {
		if errors.Is(sourceErr, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("stat %s: %w", from, sourceErr)
	}

	_, destErr := os.Stat(dest)
	if destErr == nil {
		// The destination already carries the parked copy; drop the duplicate.
		removeErr := os.Remove(from)
		if removeErr != nil {
			return fmt.Errorf("remove duplicate %s: %w", from, removeErr)
		}

		return nil
	} else if !errors.Is(destErr, os.ErrNotExist) {
		return fmt.Errorf("stat %s: %w", dest, destErr)
	}

	mkdirErr := os.MkdirAll(filepath.Dir(dest), defaultDirPerm)
	if mkdirErr != nil {
		return fmt.Errorf("create parent for %s: %w", filepath.Base(dest), mkdirErr)
	}

	return movePath(from, dest)
}

func mergeDirChildren(from, dest string) error {
	entries, err := os.ReadDir(from)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("read %s: %w", from, err)
	}

	for _, entry := range entries {
		child := filepath.Join(from, entry.Name())
		target := filepath.Join(dest, entry.Name())

		_, statErr := os.Stat(target)
		if statErr == nil {
			_ = os.RemoveAll(child) // parked in an earlier run

			continue
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return fmt.Errorf("stat %s: %w", target, statErr)
		}

		moveErr := movePath(child, target)
		if moveErr != nil {
			return moveErr
		}
	}

	_ = os.Remove(from)

	return nil
}

func movePath(from, dest string) error {
	mkdirErr := os.MkdirAll(filepath.Dir(dest), defaultDirPerm)
	if mkdirErr != nil {
		return fmt.Errorf("create parent for %s: %w", filepath.Base(dest), mkdirErr)
	}

	renameErr := os.Rename(from, dest)
	if renameErr != nil {
		copyErr := copyTree(from, dest)
		if copyErr != nil {
			return fmt.Errorf(
				"move %s to %s: %w (copy fallback: %w)",
				filepath.Base(from), filepath.Base(dest), renameErr, copyErr,
			)
		}

		_ = os.RemoveAll(from)
	}

	return nil
}

// copyTree copies a subtree within the data directory. It walks through
// os.Root handles so symlinks cannot escape the migrated namespace.
func copyTree(from, dest string) error {
	mkdirErr := os.MkdirAll(dest, defaultDirPerm)
	if mkdirErr != nil {
		return fmt.Errorf("create copy destination dir: %w", mkdirErr)
	}

	sourceRoot, err := os.OpenRoot(from)
	if err != nil {
		return fmt.Errorf("open copy source: %w", err)
	}
	defer func() { _ = sourceRoot.Close() }()

	destRoot, err := os.OpenRoot(dest)
	if err != nil {
		return fmt.Errorf("open copy destination: %w", err)
	}
	defer func() { _ = destRoot.Close() }()

	walkErr := fs.WalkDir(sourceRoot.FS(), ".", copyTreeEntry(sourceRoot, destRoot))
	if walkErr != nil {
		return fmt.Errorf("copy tree %s: %w", from, walkErr)
	}

	return nil
}

// copyTreeEntry returns the fs.WalkDirFunc that copies one entry between two
// root-scoped handles.
func copyTreeEntry(sourceRoot, destRoot *os.Root) fs.WalkDirFunc {
	return func(rel string, info os.DirEntry, walkInErr error) error {
		if walkInErr != nil {
			return fmt.Errorf("walk %s: %w", rel, walkInErr)
		}

		perm, permErr := dirEntryPerm(info)
		if permErr != nil {
			return permErr
		}

		if info.IsDir() {
			mkdirErr := destRoot.MkdirAll(rel, perm)
			if mkdirErr != nil {
				return fmt.Errorf("create %s: %w", rel, mkdirErr)
			}

			return nil
		}

		inFile, openErr := sourceRoot.Open(rel)
		if openErr != nil {
			return fmt.Errorf("open source %s: %w", rel, openErr)
		}
		defer func() { _ = inFile.Close() }()

		outFile, createErr := destRoot.OpenFile(rel, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
		if createErr != nil {
			return fmt.Errorf("create destination %s: %w", rel, createErr)
		}
		defer func() { _ = outFile.Close() }()

		_, copyErr := io.Copy(outFile, inFile)
		if copyErr != nil {
			return fmt.Errorf("copy %s: %w", rel, copyErr)
		}

		return nil
	}
}

func dirEntryPerm(entry os.DirEntry) (os.FileMode, error) {
	info, err := entry.Info()
	if err != nil {
		return 0, fmt.Errorf("inspect copied entry: %w", err)
	}

	return info.Mode().Perm(), nil
}

func (s *Store) drainLegacyNamespaces(current, legacy namespace) (bool, error) {
	state, err := initializedMigrationState(legacy, current)
	if err != nil {
		return false, err
	}

	target := resolveMigrationTarget(s.baseDir, current, state)

	layoutErr := s.ensureLayout(target)
	if layoutErr != nil {
		return false, layoutErr
	}

	names, err := legacySessionNames(legacy)
	if err != nil {
		return false, err
	}

	legacyIdx, targetIdx, err := s.loadDrainIndexes(legacy, target)
	if err != nil {
		return false, err
	}

	indexesChanged := false

	for _, name := range names {
		moved, claimErr := s.drainLegacyRecord(
			target, legacy, name, &state, &legacyIdx, &targetIdx,
		)
		if claimErr != nil {
			return false, claimErr
		}
		indexesChanged = indexesChanged || moved
	}

	if indexesChanged {
		legacyIndexErr := writeJSONAtomic(legacy.indexPath(), legacyIdx)
		if legacyIndexErr != nil {
			return false, legacyIndexErr
		}
		targetIndexErr := writeJSONAtomic(target.indexPath(), targetIdx)
		if targetIndexErr != nil {
			return false, targetIndexErr
		}
	}

	// A leftover snapshot on disk can remain even when the index dropped it,
	// so re-check the directory before deciding.
	if legacyHasRecords(legacy) {
		return true, nil
	}

	return false, reconcileEmptyLegacy(legacy)
}

func initializedMigrationState(legacy, current namespace) (migrationState, error) {
	state, err := loadMigrationState(legacy)
	if err != nil {
		return migrationState{}, err
	}
	if strings.TrimSpace(state.ServerID) != "" {
		return state, nil
	}

	state.ServerID = current.id
	state.Socket = current.socket
	stateErr := writeMigrationState(legacy, state)
	if stateErr != nil {
		return migrationState{}, stateErr
	}

	return state, nil
}

func (s *Store) loadDrainIndexes(
	legacy, target namespace,
) (snapshot.Index, snapshot.Index, error) {
	legacyIdx, err := s.loadIndexUnlocked(legacy)
	if err != nil {
		return snapshot.Index{}, snapshot.Index{}, err
	}
	targetIdx, err := s.loadIndexUnlocked(target)
	if err != nil {
		return snapshot.Index{}, snapshot.Index{}, err
	}

	return legacyIdx, targetIdx, nil
}

// drainLegacyRecord claims and moves one legacy record when it does not
// collide with the target namespace. It returns moved=false for collision
// leftovers.
func (s *Store) drainLegacyRecord(
	target, legacy namespace,
	name string,
	state *migrationState,
	legacyIdx, targetIdx *snapshot.Index,
) (bool, error) {
	collides, err := s.claimLegacyRecord(target, legacy, name, state)
	if err != nil {
		return false, err
	}
	if collides {
		return false, nil
	}

	moved, err := s.migrateOneRecord(target, legacy, name, legacyIdx, targetIdx)
	if err != nil {
		return false, err
	}

	state.removeClaim(name)

	stateErr := writeMigrationState(legacy, *state)
	if stateErr != nil {
		return false, stateErr
	}

	return moved, nil
}

// claimLegacyRecord records migration intent before any file moves. A
// snapshot that already exists in the target namespace collides and stays
// behind in the selectable legacy namespace.
func (s *Store) claimLegacyRecord(
	target, legacy namespace,
	name string,
	state *migrationState,
) (bool, error) {
	if state.claims(name) {
		return false, nil
	}

	_, targetStatErr := os.Stat(target.sessionPath(name))
	if targetStatErr == nil {
		return true, nil
	} else if !errors.Is(targetStatErr, os.ErrNotExist) {
		return false, fmt.Errorf("stat target snapshot: %w", targetStatErr)
	}

	state.addClaim(name)

	stateErr := writeMigrationState(legacy, *state)
	if stateErr != nil {
		return false, stateErr
	}

	return false, nil
}

// resolveMigrationTarget picks the namespace a claimed record moves into. A
// migration started for another tmux server finishes into that server's
// namespace even when invoked from a different server.
func resolveMigrationTarget(baseDir string, current namespace, state migrationState) namespace {
	if state.ServerID == current.id {
		return current
	}

	target, targetErr := namespaceByID(baseDir, state.ServerID)
	if targetErr == nil {
		return target
	}

	resolved := namespaceFor(baseDir, state.Socket)
	if resolved.id == state.ServerID {
		return resolved
	}

	// The claimed namespace vanished together with its identity; re-claim
	// for the current server.
	return current
}

// migrateOneRecord moves one session snapshot and its scrollback into target.
// It reports whether either index changed.
func (s *Store) migrateOneRecord(
	target, legacy namespace,
	name string,
	legacyIdx, targetIdx *snapshot.Index,
) (bool, error) {
	targetPath := target.sessionPath(name)

	_, statErr := os.Stat(targetPath)
	switch {
	case statErr == nil:
		// A previous run published the target; finish the cleanup.
		cleanupErr := finishMigratedCleanup(target, legacy, name)
		if cleanupErr != nil {
			return false, cleanupErr
		}
	case errors.Is(statErr, os.ErrNotExist):
		publishErr := publishMigratedSnapshot(target, legacy, name, targetPath)
		if publishErr != nil {
			return false, publishErr
		}
	default:
		return false, fmt.Errorf("stat target snapshot: %w", statErr)
	}

	rec, ok := legacyIdx.Sessions[name]
	if !ok {
		snap, err := readSnapshotAt(targetPath, name)
		if err != nil {
			return false, err
		}
		rec = recordFromSnapshot(target, snap)
	}
	rec.SessionName = name
	rec.File = filepath.ToSlash(filepath.Join(sessionsDirName, filepath.Base(targetPath)))
	rec.ServerSocket = target.socket

	targetIdx.Sessions[name] = rec
	delete(legacyIdx.Sessions, name)

	return true, nil
}

func publishMigratedSnapshot(target, legacy namespace, name, targetPath string) error {
	scrollbackErr := moveLegacyScrollback(target, legacy, name)
	if scrollbackErr != nil {
		return scrollbackErr
	}

	snap, err := readSnapshotForMigration(legacy.sessionPath(name))
	if err != nil {
		return err
	}

	snap.ServerSocket = target.socket
	snap.Version = snapshot.FormatVersion

	tmp, err := writeJSONTemp(targetPath, snap, defaultFilePerm)
	if err != nil {
		return err
	}
	renameErr := os.Rename(tmp, targetPath)
	if renameErr != nil {
		return fmt.Errorf("publish migrated snapshot: %w", renameErr)
	}

	removeErr := os.Remove(legacy.sessionPath(name))
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return fmt.Errorf("remove migrated legacy snapshot: %w", removeErr)
	}

	return nil
}

func finishMigratedCleanup(target, legacy namespace, name string) error {
	scrollbackErr := moveLegacyScrollback(target, legacy, name)
	if scrollbackErr != nil {
		return scrollbackErr
	}
	removeErr := os.Remove(legacy.sessionPath(name))
	if removeErr != nil && !errors.Is(removeErr, os.ErrNotExist) {
		return fmt.Errorf("remove migrated legacy snapshot: %w", removeErr)
	}

	return nil
}

func readSnapshotAt(path, name string) (snapshot.SessionSnapshot, error) {
	var snap snapshot.SessionSnapshot

	data, err := os.ReadFile(path) // #nosec G304 -- sanitized name under namespace dir
	if err != nil {
		return snap, fmt.Errorf("read migrated snapshot: %w", err)
	}
	unmarshalErr := json.Unmarshal(data, &snap)
	if unmarshalErr != nil {
		return snap, fmt.Errorf("decode migrated snapshot: %w", unmarshalErr)
	}
	snap.SessionName = name

	return snap, nil
}

func recordFromSnapshot(serverNS namespace, snap snapshot.SessionSnapshot) snapshot.Record {
	panes := 0
	for _, w := range snap.Windows {
		panes += len(w.Panes)
	}

	return snapshot.Record{
		SessionName:  snap.SessionName,
		File:         "",
		ServerSocket: serverNS.socket,
		CapturedAt:   snap.CapturedAt.UTC(),
		LastAccessed: time.Time{},
		Windows:      len(snap.Windows),
		Panes:        panes,
	}
}

func readSnapshotForMigration(path string) (snapshot.SessionSnapshot, error) {
	var snap snapshot.SessionSnapshot

	data, err := os.ReadFile(path) // #nosec G304 -- walking the legacy session dir
	if err != nil {
		return snap, fmt.Errorf("read legacy snapshot: %w", err)
	}
	versionErr := validateSnapshotVersion(data)
	if versionErr != nil {
		return snap, versionErr
	}
	unmarshalErr := json.Unmarshal(data, &snap)
	if unmarshalErr != nil {
		return snap, fmt.Errorf("decode legacy snapshot: %w", unmarshalErr)
	}

	return snap, nil
}

func moveLegacyScrollback(target, legacy namespace, name string) error {
	safeName, err := safeScrollbackSessionName(name)
	if err != nil {
		return err
	}

	source := filepath.Clean(filepath.Join(legacy.scrollRoot(), safeName))
	dest := filepath.Clean(filepath.Join(target.scrollRoot(), safeName))

	_, sourceStatErr := os.Stat(source)
	if sourceStatErr != nil {
		if errors.Is(sourceStatErr, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("stat legacy scrollback: %w", sourceStatErr)
	}

	_, destStatErr := os.Stat(dest)
	if destStatErr == nil {
		// Target already owns the scrollback (interrupted run); the legacy copy
		// is redundant.
		removeErr := os.RemoveAll(source)
		if removeErr != nil {
			return fmt.Errorf("remove redundant scrollback: %w", removeErr)
		}

		return nil
	} else if !errors.Is(destStatErr, os.ErrNotExist) {
		return fmt.Errorf("stat migrated scrollback: %w", destStatErr)
	}

	mkdirErr := os.MkdirAll(target.scrollRoot(), scrollbackDirPerm)
	if mkdirErr != nil {
		return fmt.Errorf("create scrollback namespace: %w", mkdirErr)
	}

	renameErr := os.Rename(source, dest)
	if renameErr != nil {
		copyErr := copyTree(source, dest)
		if copyErr != nil {
			return fmt.Errorf(
				"move scrollback for %s: %w (copy: %w)",
				name, renameErr, copyErr,
			)
		}

		_ = os.RemoveAll(source)
	}

	return nil
}

func legacySessionNames(legacy namespace) ([]string, error) {
	names := make(map[string]struct{})

	idx, err := loadLegacyIndex(legacy)
	if err != nil {
		return nil, err
	}
	for name := range idx.Sessions {
		names[name] = struct{}{}
	}

	entries, err := os.ReadDir(legacy.sessionsDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return sortedNames(names), nil
		}

		return nil, fmt.Errorf("read legacy sessions dir: %w", err)
	}
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		fileName := entry.Name()
		if !strings.HasSuffix(fileName, ".json") {
			continue
		}
		names[strings.TrimSuffix(fileName, ".json")] = struct{}{}
	}

	return sortedNames(names), nil
}

func sortedNames(names map[string]struct{}) []string {
	out := make([]string, 0, len(names))
	for name := range names {
		out = append(out, name)
	}
	sort.Strings(out)

	return out
}

func loadLegacyIndex(legacy namespace) (snapshot.Index, error) {
	emptyStore := &Store{}

	return emptyStore.loadIndexUnlocked(legacy)
}

func reconcileEmptyLegacy(legacy namespace) error {
	entries, err := os.ReadDir(legacy.sessionsDir())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect legacy sessions: %w", err)
	}
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".json") {
			// Selectable legacy records remain.
			return nil
		}
	}

	idx, err := loadLegacyIndex(legacy)
	if err != nil {
		return err
	}
	if len(idx.Sessions) > 0 {
		return nil
	}

	_ = os.RemoveAll(legacy.scrollRoot())

	indexRemoveErr := os.Remove(legacy.indexPath())
	if indexRemoveErr != nil && !errors.Is(indexRemoveErr, os.ErrNotExist) {
		return fmt.Errorf("remove drained legacy index: %w", indexRemoveErr)
	}

	statePath := filepath.Join(legacy.root, migrationStateName)
	stateRemoveErr := os.Remove(statePath)
	if stateRemoveErr != nil && !errors.Is(stateRemoveErr, os.ErrNotExist) {
		return fmt.Errorf("remove migration state: %w", stateRemoveErr)
	}

	// Remove the leftover directories/root when empty; ignore failures.
	_ = os.RemoveAll(legacy.sessionsDir())
	_ = os.RemoveAll(legacy.locksDir())
	_ = os.Remove(legacy.root)

	return nil
}

func loadMigrationState(legacy namespace) (migrationState, error) {
	state := migrationState{}

	data, err := os.ReadFile(
		filepath.Join(legacy.root, migrationStateName),
	) // #nosec G304 -- fixed state file under the legacy namespace
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return state, nil
		}

		return state, fmt.Errorf("read migration state: %w", err)
	}
	unmarshalErr := json.Unmarshal(data, &state)
	if unmarshalErr != nil {
		// A corrupt state file must not wedge the migration; start over.
		return migrationState{}, nil
	}

	return state, nil
}

func writeMigrationState(legacy namespace, state migrationState) error {
	state.UpdatedAt = time.Now().UTC()
	path := filepath.Join(legacy.root, migrationStateName)

	return writeJSONAtomic(path, state)
}

func (state *migrationState) claims(name string) bool {
	return slices.Contains(state.Claimed, name)
}

func (state *migrationState) addClaim(name string) {
	state.Claimed = append(state.Claimed, name)
	sort.Strings(state.Claimed)
}

func (state *migrationState) removeClaim(name string) {
	filtered := state.Claimed[:0]
	for _, claimed := range state.Claimed {
		if claimed != name {
			filtered = append(filtered, claimed)
		}
	}
	state.Claimed = filtered
}
