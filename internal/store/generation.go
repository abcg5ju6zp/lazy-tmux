package store

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/alchemmist/lazy-tmux/internal/snapshot"
)

// Snapshot generations are published transactionally. A save cycle first fixes
// the set of sessions it covers, stages every session JSON file and scrollback
// tree in an isolated directory, and only then publishes the whole generation:
// scrollback dirs, session JSON files and the index switch together.
//
// The manifest persisted inside the generation directory records the publish
// phase. A daemon (or any other process) that finds a generation directory
// after a crash either finishes a generation whose index is already published
// or rolls the live tree back to the previous complete generation.

const (
	txnDirName       = ".txn"
	manifestFileName = "manifest.json"
	stagedIndexName  = "index.json"
	genLockFileName  = ".lock"

	manifestVersion = 1

	phaseStaging        = "staging"
	phaseStaged         = "staged"
	phaseScrollbackLive = "scrollback-live"
	phaseSessionsLive   = "sessions-live"
	phaseIndexReady     = "index-ready"
	phaseIndexed        = "indexed"

	genKindBatch   = "batch"
	genKindSession = "session"

	promoteTargetScrollback = "scrollback"
	promoteTargetJSON       = "json"

	generationIDAttempts = 8
)

// errSimulatedGenerationCrash is a test-only sentinel that aborts a commit the
// way a process killed at that point would: the on-disk phase record is kept
// and the in-process generation lock is released, but no rollback or cleanup
// runs.
var errSimulatedGenerationCrash = errors.New("simulated generation crash")

var errGenerationIDExhausted = errors.New("allocate unique generation id")

type generationManifest struct {
	Version   int              `json:"version"`
	ID        string           `json:"id"`
	Kind      string           `json:"kind"`
	CreatedAt time.Time        `json:"created_at"`
	Phase     string           `json:"phase"`
	IndexSHA  string           `json:"index_sha256,omitempty"`
	Sessions  []plannedSession `json:"sessions"`
}

type plannedSession struct {
	Name string `json:"name"`

	SafeName string `json:"safe_name"`
	// HasScrollback reports whether the new generation stages scrollback for
	// the session.
	HasScrollback bool `json:"has_scrollback"`
	// HadJSON and HadScrollback record whether live artifacts of the previous
	// generation existed when the cycle was staged. Rollback uses them to tell
	// untouched old artifacts apart from newly published ones when no backup
	// was ever created.
	HadJSON       bool     `json:"had_json"`
	HadScrollback bool     `json:"had_scrollback"`
	ScrollFiles   []string `json:"scroll_files,omitempty"`
}

// generationHooks lets internal tests force failures and crash points.
type generationHooks struct {
	// beforeScrollbackFile is called before each staged scrollback file is
	// written in the isolated generation directory.
	beforeScrollbackFile func(sessionName, fileName string) error
	// beforePromote is called before a session artifact is switched from the
	// staged copy to its live location. target is promoteTargetScrollback or
	// promoteTargetJSON.
	beforePromote func(sessionName, target string) error
	// afterPhase is called after a phase record has been persisted. The
	// "index-published" pseudo-phase runs after the index rename but before the
	// phaseIndexed record is written.
	afterPhase func(phase string) error
}

type stagedSession struct {
	snap snapshot.SessionSnapshot
	plan plannedSession
}

type generation struct {
	store    *Store
	id       string
	dir      string
	manifest generationManifest
	lockFile *os.File
	// committed flips once the index rename lands. After that point failures
	// must never roll back: the next recovery finishes the committed gen.
	committed bool
}

func newGenerationID(now time.Time) string {
	var randomBytes [4]byte

	_, _ = rand.Read(randomBytes[:])

	return "gen-" + now.UTC().Format("20060102T150405.000000000") +
		"-" + hex.EncodeToString(randomBytes[:])
}

func (s *Store) txnRoot() string {
	return filepath.Join(s.baseDir, txnDirName)
}

func (s *Store) generationDir(generationID string) string {
	return filepath.Join(s.txnRoot(), generationID)
}

func jsonBackupPath(livePath, generationID string) string {
	return livePath + "." + generationID + ".bak"
}

func scrollbackBackupPath(liveDir, generationID string) string {
	return liveDir + "." + generationID + ".bak"
}

// SaveSessions publishes one generation covering all supplied snapshots.
// Every snapshot is captured by the caller first: a capture failure must be
// reported before SaveSessions is invoked so that no live artifact changes.
func (s *Store) SaveSessions(input []snapshot.SessionSnapshot) error {
	if len(input) == 0 {
		return nil
	}

	return s.withGenerationLock(syscall.LOCK_EX, func() error {
		err := s.recoverGenerationsLocked()
		if err != nil {
			return err
		}

		return s.saveSessionsLocked(input)
	})
}

// saveSessionsLocked stages and commits while the caller already holds the
// exclusive generation lock.
func (s *Store) saveSessionsLocked(input []snapshot.SessionSnapshot) error {
	gen, staged, err := s.stageGeneration(input)
	if err != nil {
		return err
	}
	if gen == nil {
		return nil
	}

	return s.commitGeneration(gen, staged)
}

// RecoverGenerations converges generations left behind by processes that died
// while publishing: a generation whose index is already live is finished,
// every other interrupted generation is rolled back to the previous complete
// one, and all staging or backup residue is removed.
func (s *Store) RecoverGenerations() error {
	return s.withGenerationLock(syscall.LOCK_EX, s.recoverGenerationsLocked)
}

func (s *Store) normalizeGenerationInput(
	input []snapshot.SessionSnapshot,
) ([]string, map[string]snapshot.SessionSnapshot, error) {
	now := time.Now().UTC()

	byName := make(map[string]snapshot.SessionSnapshot, len(input))
	names := make([]string, 0, len(input))

	for _, original := range input {
		if original.SessionName == "" {
			return nil, nil, errEmptySessionName
		}

		if _, seen := byName[original.SessionName]; seen {
			continue
		}

		snap := original
		snap.Version = snapshot.FormatVersion

		if snap.CapturedAt.IsZero() {
			snap.CapturedAt = now
		}

		byName[snap.SessionName] = snap
		names = append(names, snap.SessionName)
	}

	return names, byName, nil
}

func (s *Store) stageGeneration(
	input []snapshot.SessionSnapshot,
) (*generation, []stagedSession, error) {
	names, byName, err := s.normalizeGenerationInput(input)
	if err != nil {
		return nil, nil, err
	}

	if len(names) == 0 {
		return nil, nil, nil
	}

	err = os.MkdirAll(s.txnRoot(), defaultDirPerm)
	if err != nil {
		return nil, nil, fmt.Errorf("create transaction dir: %w", err)
	}

	kind := genKindBatch
	if len(names) == 1 {
		kind = genKindSession
	}

	gen, err := s.allocateGeneration(kind)
	if err != nil {
		return nil, nil, err
	}

	abort := func(cause error) (*generation, []stagedSession, error) {
		gen.remove()

		return nil, nil, cause
	}

	err = gen.writeManifest()
	if err != nil {
		return abort(err)
	}

	staged := make([]stagedSession, 0, len(names))

	for _, name := range names {
		one, stageErr := s.stageSession(gen, byName[name])
		if stageErr != nil {
			return abort(stageErr)
		}

		staged = append(staged, one)
	}

	return gen, staged, nil
}

func (s *Store) allocateGeneration(kind string) (*generation, error) {
	for range generationIDAttempts {
		generationID := newGenerationID(time.Now())
		genDir := s.generationDir(generationID)

		err := os.Mkdir(genDir, defaultDirPerm)
		if errors.Is(err, os.ErrExist) {
			continue // vanishingly unlikely timestamp+random collision
		}

		if err != nil {
			return nil, fmt.Errorf("create generation dir: %w", err)
		}

		gen, err := s.openNewGeneration(generationID, genDir, kind)
		if err == nil {
			return gen, nil
		}

		_ = os.RemoveAll(genDir)

		return nil, err
	}

	return nil, errGenerationIDExhausted
}

func (s *Store) openNewGeneration(
	generationID, genDir, kind string,
) (*generation, error) {
	err := os.Mkdir(filepath.Join(genDir, sessionsDirName), scrollbackDirPerm)
	if err != nil {
		return nil, fmt.Errorf("create generation sessions dir: %w", err)
	}

	err = os.Mkdir(filepath.Join(genDir, scrollbackDir), scrollbackDirPerm)
	if err != nil {
		return nil, fmt.Errorf("create generation scrollback dir: %w", err)
	}

	lockFile, err := openGenerationLock(genDir)
	if err != nil {
		return nil, err
	}

	manifest := generationManifest{
		Version:   manifestVersion,
		ID:        generationID,
		Kind:      kind,
		CreatedAt: time.Now().UTC(),
		Phase:     phaseStaging,
		IndexSHA:  "",
		Sessions:  nil,
	}

	return &generation{
		store:     s,
		id:        generationID,
		dir:       genDir,
		manifest:  manifest,
		lockFile:  lockFile,
		committed: false,
	}, nil
}

func (s *Store) stageSession(
	gen *generation,
	sessionSnapshot snapshot.SessionSnapshot,
) (stagedSession, error) {
	safeName, entries, err := s.planScrollbackUnlocked(&sessionSnapshot)
	if err != nil {
		return stagedSession{}, err
	}

	plan := plannedSession{
		Name:          sessionSnapshot.SessionName,
		SafeName:      safeName,
		HasScrollback: len(entries) > 0,
		HadJSON:       false,
		HadScrollback: false,
		ScrollFiles:   make([]string, 0, len(entries)),
	}

	hadJSON, hadScrollback, err := s.inspectPreviousArtifacts(
		sessionSnapshot.SessionName,
		safeName,
	)
	if err != nil {
		return stagedSession{}, err
	}

	plan.HadJSON = hadJSON
	plan.HadScrollback = hadScrollback

	jsonData, err := json.MarshalIndent(sessionSnapshot, "", "  ")
	if err != nil {
		return stagedSession{}, fmt.Errorf("marshal session json: %w", err)
	}

	err = writeFileAtomic(
		gen.stagedJSONPath(safeName),
		append(jsonData, '\n'),
		defaultFilePerm,
	)
	if err != nil {
		return stagedSession{}, fmt.Errorf("stage session json: %w", err)
	}

	err = s.stageScrollbackFiles(gen, &plan, sessionSnapshot.SessionName, entries)
	if err != nil {
		return stagedSession{}, err
	}

	return stagedSession{snap: sessionSnapshot, plan: plan}, nil
}

func (s *Store) inspectPreviousArtifacts(
	sessionName, safeName string,
) (bool, bool, error) {
	hadJSON := false
	hadScrollback := false

	_, jsonStatErr := os.Stat(s.sessionPath(sessionName))
	if jsonStatErr == nil {
		hadJSON = true
	} else if !errors.Is(jsonStatErr, os.ErrNotExist) {
		return false, false, fmt.Errorf("inspect live session file: %w", jsonStatErr)
	}

	liveScrollbackDir := filepath.Join(s.baseDir, scrollbackDir, safeName)

	_, scrollbackStatErr := os.Stat(liveScrollbackDir)
	if scrollbackStatErr == nil {
		hadScrollback = true
	} else if !errors.Is(scrollbackStatErr, os.ErrNotExist) {
		return false, false, fmt.Errorf("inspect live scrollback dir: %w", scrollbackStatErr)
	}

	return hadJSON, hadScrollback, nil
}

func (s *Store) stageScrollbackFiles(
	gen *generation,
	plan *plannedSession,
	sessionName string,
	entries []scrollbackEntry,
) error {
	if len(entries) == 0 {
		return nil
	}

	stagedScrollbackDir := gen.stagedScrollbackDir(plan.SafeName)

	for _, ent := range entries {
		plan.ScrollFiles = append(plan.ScrollFiles, ent.FileName)

		if hook := s.genHook; hook != nil && hook.beforeScrollbackFile != nil {
			hookErr := hook.beforeScrollbackFile(sessionName, ent.FileName)
			if hookErr != nil {
				return hookErr
			}
		}

		path := filepath.Join(stagedScrollbackDir, ent.FileName)

		err := os.WriteFile(path, []byte(ent.Content), scrollbackFilePerm)
		if err != nil {
			return fmt.Errorf("stage scrollback file: %w", err)
		}
	}

	return nil
}

func (g *generation) stagedJSONPath(safeName string) string {
	return filepath.Join(g.dir, sessionsDirName, safeName+".json")
}

func (g *generation) stagedScrollbackDir(safeName string) string {
	dir := filepath.Join(g.dir, scrollbackDir, safeName)
	_ = os.MkdirAll(dir, scrollbackDirPerm)

	return dir
}

func (g *generation) stagedIndexPath() string {
	return filepath.Join(g.dir, stagedIndexName)
}

func (g *generation) writeManifest() error {
	path := filepath.Join(g.dir, manifestFileName)

	err := writeJSONAtomic(path, g.manifest)
	if err != nil {
		return err
	}

	syncDir(filepath.Dir(path))

	return nil
}

func (g *generation) setPhase(phase string) error {
	g.manifest.Phase = phase

	return g.writeManifest()
}

func (g *generation) releaseLock() {
	if g.lockFile == nil {
		return
	}

	_ = syscall.Flock(int(g.lockFile.Fd()), syscall.LOCK_UN)
	_ = g.lockFile.Close()

	g.lockFile = nil
}

// remove destroys the isolated generation directory, releasing its lock first,
// and removes the transaction root when no other generation remains in it.
func (g *generation) remove() {
	g.releaseLock()
	_ = os.RemoveAll(g.dir)

	entries, err := os.ReadDir(g.store.txnRoot())
	if err == nil && len(entries) == 0 {
		_ = os.Remove(g.store.txnRoot())
	}
}

func openGenerationLock(genDir string) (*os.File, error) {
	lockPath := filepath.Join(genDir, genLockFileName)

	// #nosec G304 G703 -- fixed name inside a freshly created generation dir
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open generation lock: %w", err)
	}

	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX)
	if err != nil {
		_ = file.Close()

		return nil, fmt.Errorf("lock generation: %w", err)
	}

	return file, nil
}

// commitGeneration switches scrollback dirs, session JSON files and the index
// as one publish. Failures before the index rename roll back; the index rename
// is the commit point, after which cleanup errors are left for recovery rather
// than rolled back.
func (s *Store) commitGeneration(gen *generation, staged []stagedSession) error {
	commitErr := s.runGenerationCommit(gen, staged)

	switch {
	case commitErr == nil:
		return nil
	case gen.committed:
		// The index is live: rollback would resurrect old files under the new
		// index. Release locks and let the next recovery finish cleanup.
		gen.releaseLock()

		return commitErr
	case errors.Is(commitErr, errSimulatedGenerationCrash):
		// Mimic process death: release locks but keep disk state.
		gen.releaseLock()

		return commitErr
	default:
		_ = s.rollbackManifest(gen.manifest)
		gen.remove()

		return commitErr
	}
}

func (s *Store) runGenerationCommit(gen *generation, staged []stagedSession) error {
	err := s.ensureLayout()
	if err != nil {
		return err
	}

	gen.manifest.Sessions = plannedSessions(staged)

	err = s.advancePhase(gen, phaseStaged)
	if err != nil {
		return err
	}

	err = s.publishScrollback(gen, staged)
	if err != nil {
		return err
	}

	err = s.advancePhase(gen, phaseScrollbackLive)
	if err != nil {
		return err
	}

	err = s.publishSessionFiles(gen, staged)
	if err != nil {
		return err
	}

	err = s.advancePhase(gen, phaseSessionsLive)
	if err != nil {
		return err
	}

	return s.commitIndexAndFinish(gen, staged)
}

// advancePhase persists a phase record and runs the crash hook for that
// phase.
func (s *Store) advancePhase(gen *generation, phase string) error {
	err := gen.setPhase(phase)
	if err != nil {
		return err
	}

	return s.crashHook(phase)
}

// commitIndexAndFinish is the commit point: the index rename publishes the
// generation, after which only backup cleanup remains.
func (s *Store) commitIndexAndFinish(gen *generation, staged []stagedSession) error {
	err := s.publishIndex(gen, staged)
	if err != nil {
		return err
	}

	err = gen.setPhase(phaseIndexed)
	if err != nil {
		// Index is live; the next recovery finishes the cleanup.
		return err
	}

	err = s.crashHook(phaseIndexed)
	if err != nil {
		return err
	}

	err = s.finishManifest(gen.manifest)
	if err != nil {
		return err
	}

	gen.remove()

	return nil
}

func (s *Store) publishScrollback(gen *generation, staged []stagedSession) error {
	for _, one := range staged {
		err := s.runPromoteHook(one.plan.Name, promoteTargetScrollback)
		if err != nil {
			return err
		}

		err = s.promoteScrollback(gen, one)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *Store) publishSessionFiles(gen *generation, staged []stagedSession) error {
	for _, one := range staged {
		livePath := s.sessionPath(one.plan.Name)

		err := s.runPromoteHook(one.plan.Name, promoteTargetJSON)
		if err != nil {
			return err
		}

		err = backupLegacyAntex(livePath)
		if err != nil {
			return err
		}

		err = promoteFile(
			gen.stagedJSONPath(one.plan.SafeName),
			livePath,
			jsonBackupPath(livePath, gen.id),
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// publishIndex stages the final index file, records its content hash and
// renames it into place. The rename is the generation commit point.
func (s *Store) publishIndex(gen *generation, staged []stagedSession) error {
	indexData, err := s.buildGenerationIndex(staged)
	if err != nil {
		return err
	}

	err = os.WriteFile(gen.stagedIndexPath(), indexData, defaultFilePerm)
	if err != nil {
		return fmt.Errorf("stage index: %w", err)
	}

	gen.manifest.IndexSHA = sha256Hex(indexData)

	err = s.advancePhase(gen, phaseIndexReady)
	if err != nil {
		return err
	}

	liveIndex := s.indexPath()

	err = os.Rename(gen.stagedIndexPath(), liveIndex)
	if err != nil && !s.liveIndexMatches(gen.manifest.IndexSHA) {
		return fmt.Errorf("publish index: %w", err)
	}

	syncDir(s.baseDir)

	gen.committed = true

	return s.crashHook("index-published")
}

func plannedSessions(staged []stagedSession) []plannedSession {
	plans := make([]plannedSession, 0, len(staged))
	for _, one := range staged {
		plans = append(plans, one.plan)
	}

	return plans
}

func (s *Store) buildGenerationIndex(staged []stagedSession) ([]byte, error) {
	idx, err := s.loadIndexUnlocked()
	if err != nil {
		return nil, err
	}

	for _, one := range staged {
		panes := 0
		for _, w := range one.snap.Windows {
			panes += len(w.Panes)
		}

		previous := idx.Sessions[one.snap.SessionName]

		idx.Sessions[one.snap.SessionName] = snapshot.Record{
			SessionName:  one.snap.SessionName,
			File:         s.sessionPath(one.snap.SessionName),
			CapturedAt:   one.snap.CapturedAt.UTC(),
			LastAccessed: previous.LastAccessed,
			Windows:      len(one.snap.Windows),
			Panes:        panes,
		}
	}

	idx.Updated = time.Now().UTC()

	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("marshal index: %w", err)
	}

	return append(data, '\n'), nil
}

func (s *Store) promoteScrollback(gen *generation, one stagedSession) error {
	scrollRoot := filepath.Clean(filepath.Join(s.baseDir, scrollbackDir))

	liveDir := filepath.Clean(filepath.Join(scrollRoot, one.plan.SafeName))

	err := ensureUnderDir(scrollRoot, liveDir, one.plan.Name)
	if err != nil {
		return err
	}

	backupDir := scrollbackBackupPath(liveDir, gen.id)

	err = backupLivePath(liveDir, backupDir)
	if err != nil {
		return err
	}

	if !one.plan.HasScrollback {
		// The new generation carries no scrollback for this session; the
		// previous directory is preserved in backupDir until commit.
		err = os.RemoveAll(liveDir)
		if err != nil {
			return fmt.Errorf("remove superseded scrollback dir: %w", err)
		}

		return nil
	}

	stageDir := filepath.Join(gen.dir, scrollbackDir, one.plan.SafeName)

	_, err = os.Stat(stageDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			// Already promoted (a resumed commit is replaying the step).
			return nil
		}

		return fmt.Errorf("stat staged scrollback dir: %w", err)
	}

	err = os.Rename(stageDir, liveDir)
	if err != nil {
		return fmt.Errorf("publish scrollback dir: %w", err)
	}

	return nil
}

// backupLivePath renames the current live artifact to backupPath unless a
// backup already exists (the step is being replayed) or there is no live
// artifact yet.
func backupLivePath(livePath, backupPath string) error {
	_, err := os.Stat(backupPath)
	if err == nil {
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat backup artifact: %w", err)
	}

	_, err = os.Lstat(livePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("stat live artifact: %w", err)
	}

	err = os.Rename(livePath, backupPath)
	if err != nil {
		return fmt.Errorf("back up live artifact: %w", err)
	}

	return nil
}

// promoteFile is idempotent: a missing staged file means the stage already
// reached the live location. The previous live artifact, when any, is kept in
// backupPath until the generation finishes.
func promoteFile(stagedPath, livePath, backupPath string) error {
	err := backupLivePath(livePath, backupPath)
	if err != nil {
		return err
	}

	_, err = os.Stat(stagedPath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}

		return fmt.Errorf("stat staged file: %w", err)
	}

	err = os.Rename(stagedPath, livePath)
	if err != nil {
		return fmt.Errorf("publish staged file: %w", err)
	}

	return nil
}

func (s *Store) runPromoteHook(sessionName, target string) error {
	hook := s.genHook
	if hook == nil || hook.beforePromote == nil {
		return nil
	}

	err := hook.beforePromote(sessionName, target)
	if err != nil {
		if errors.Is(err, errSimulatedGenerationCrash) {
			return err
		}

		return fmt.Errorf("promote %s for %q: %w", target, sessionName, err)
	}

	return nil
}

func (s *Store) crashHook(phase string) error {
	hook := s.genHook
	if hook == nil || hook.afterPhase == nil {
		return nil
	}

	err := hook.afterPhase(phase)
	if err != nil {
		if errors.Is(err, errSimulatedGenerationCrash) {
			return err
		}

		return fmt.Errorf("generation phase %s: %w", phase, err)
	}

	return nil
}

// rollbackManifest restores every live artifact of a generation from its
// pre-publish backups. Artifacts that were never promoted are left untouched,
// and it never removes a backup that is the only remaining copy of the
// previous generation.
func (s *Store) rollbackManifest(manifest generationManifest) error {
	var rollbackErrs []error

	for _, plan := range manifest.Sessions {
		livePath := s.sessionPath(plan.Name)
		backupPath := jsonBackupPath(livePath, manifest.ID)

		err := rollbackArtifact(livePath, backupPath, plan.HadJSON, false)
		if err != nil {
			rollbackErrs = append(rollbackErrs, err)
		}

		scrollRoot := filepath.Clean(filepath.Join(s.baseDir, scrollbackDir))
		liveDir := filepath.Clean(filepath.Join(scrollRoot, plan.SafeName))
		backupDir := scrollbackBackupPath(liveDir, manifest.ID)

		err = rollbackArtifact(liveDir, backupDir, plan.HadScrollback, true)
		if err != nil {
			rollbackErrs = append(rollbackErrs, err)
		}
	}

	return errors.Join(rollbackErrs...)
}

// rollbackArtifact undoes the promotion of one artifact. When a backup exists
// it is the previous generation and becomes live again. Without a backup, an
// artifact the generation introduced (hadPrevious == false) is removed while
// an untouched previous-generation artifact is left in place.
func rollbackArtifact(livePath, backupPath string, hadPrevious, dir bool) error {
	_, err := os.Lstat(backupPath)
	if err == nil {
		return restoreBackup(livePath, backupPath, dir)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat backup artifact: %w", err)
	}

	if hadPrevious {
		return nil
	}

	return removeArtifact(livePath, dir)
}

// restoreBackup undoes one artifact promotion. When a backup exists it becomes
// the live artifact again.
func restoreBackup(livePath, backupPath string, dir bool) error {
	err := removeArtifact(livePath, dir)
	if err != nil {
		return err
	}

	err = os.Rename(backupPath, livePath)
	if err != nil {
		return fmt.Errorf("restore previous artifact: %w", err)
	}

	return nil
}

func removeArtifact(path string, dir bool) error {
	if dir {
		err := os.RemoveAll(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove published artifact: %w", err)
		}

		return nil
	}

	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove published artifact: %w", err)
	}

	return nil
}

// finishManifest completes a committed generation by dropping the previous
// generation backups.
func (s *Store) finishManifest(manifest generationManifest) error {
	var finishErrs []error

	for _, plan := range manifest.Sessions {
		livePath := s.sessionPath(plan.Name)

		err := os.Remove(jsonBackupPath(livePath, manifest.ID))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			finishErrs = append(finishErrs, fmt.Errorf("remove session backup: %w", err))
		}

		scrollRoot := filepath.Clean(filepath.Join(s.baseDir, scrollbackDir))
		liveDir := filepath.Clean(filepath.Join(scrollRoot, plan.SafeName))

		err = os.RemoveAll(scrollbackBackupPath(liveDir, manifest.ID))
		if err != nil {
			finishErrs = append(finishErrs, fmt.Errorf("remove scrollback backup: %w", err))
		}
	}

	return errors.Join(finishErrs...)
}

func (s *Store) liveIndexMatches(wantSHA string) bool {
	if wantSHA == "" {
		return false
	}

	data, err := os.ReadFile(s.indexPath())
	if err != nil {
		return false
	}

	return sha256Hex(data) == wantSHA
}

func sha256Hex(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"

	err := os.WriteFile(tmp, data, perm)
	if err != nil {
		return fmt.Errorf("write temp file: %w", err)
	}

	err = os.Rename(tmp, path)
	if err != nil {
		return fmt.Errorf("rename temp file: %w", err)
	}

	return nil
}

func syncDir(dir string) {
	// #nosec G304 -- directory handle fsync for generation journal durability
	file, err := os.Open(dir)
	if err != nil {
		return
	}

	defer func() { _ = file.Close() }()

	_ = file.Sync()
}

func (s *Store) recoverGenerationsLocked() error {
	txnRoot := s.txnRoot()

	entries, err := os.ReadDir(txnRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return s.sweepLegacyArtifactsLocked()
		}

		return fmt.Errorf("read transaction dir: %w", err)
	}

	knownIDs := make([]string, 0, len(entries))

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		generationID := entry.Name()
		genDir := filepath.Join(txnRoot, generationID)

		converged, convergeErr := s.recoverOneGeneration(generationID, genDir)
		if convergeErr != nil {
			return convergeErr
		}

		if converged {
			knownIDs = append(knownIDs, generationID)
		}
	}

	err = s.sweepOrphanBackupsLocked(knownIDs)
	if err != nil {
		return err
	}

	err = s.sweepLegacyArtifactsLocked()
	if err != nil {
		return err
	}

	remaining, readErr := os.ReadDir(txnRoot)
	if readErr == nil && len(remaining) == 0 {
		_ = os.Remove(txnRoot)
	}

	return nil
}

// recoverOneGeneration converges a single leftover generation directory.
// Directories still owned by a live process (locked) are skipped.
func (s *Store) recoverOneGeneration(generationID, genDir string) (bool, error) {
	lockPath := filepath.Join(genDir, genLockFileName)

	// #nosec G304 -- generation ids come from our own transaction directory
	lockFile, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return false, fmt.Errorf("open generation lock: %w", err)
	}

	lockErr := syscall.Flock(int(lockFile.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	if lockErr != nil {
		_ = lockFile.Close()

		if errors.Is(lockErr, syscall.EWOULDBLOCK) {
			// The owning process is still alive.
			return false, nil
		}

		return false, fmt.Errorf("lock leftover generation: %w", lockErr)
	}

	defer func() {
		_ = syscall.Flock(int(lockFile.Fd()), syscall.LOCK_UN)
		_ = lockFile.Close()
	}()

	manifestPath := filepath.Join(genDir, manifestFileName)

	// #nosec G304 -- internal manifest written by the generation machinery
	manifestData, readErr := os.ReadFile(manifestPath)

	var manifest generationManifest

	if readErr != nil || json.Unmarshal(manifestData, &manifest) != nil || manifest.ID == "" {
		// Without a manifest nothing was allowed to reach the live tree
		// safely; the generation id still scopes any backup sweep.
		_ = os.RemoveAll(genDir)

		return true, nil
	}

	manifest.ID = generationID

	if s.generationShouldFinish(manifest) {
		err = s.finishManifest(manifest)
		if err != nil {
			return false, err
		}
	} else {
		err = s.rollbackManifest(manifest)
		if err != nil {
			return false, err
		}
	}

	_ = os.RemoveAll(genDir)

	return true, nil
}

func (s *Store) generationShouldFinish(manifest generationManifest) bool {
	switch manifest.Phase {
	case phaseIndexed:
		return true
	case phaseIndexReady:
		return s.liveIndexMatches(manifest.IndexSHA)
	default:
		return false
	}
}

// sweepOrphanBackupsLocked reconciles backup artifacts for generation ids
// whose directories have already been removed. A backup is restored when the
// live artifact is missing, and discarded when newer live data is present.
func (s *Store) sweepOrphanBackupsLocked(ids []string) error {
	if len(ids) == 0 {
		return nil
	}

	err := s.sweepOrphanSessionBackupsLocked(ids)
	if err != nil {
		return err
	}

	return s.sweepOrphanScrollbackBackupsLocked(ids)
}

func (s *Store) sweepOrphanSessionBackupsLocked(ids []string) error {
	sessionsRoot := filepath.Join(s.baseDir, sessionsDirName)

	entries, err := os.ReadDir(sessionsRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read sessions dir: %w", err)
	}

	for _, entry := range entries {
		name := entry.Name()

		generationID, ok := matchGenerationBackup(name, ".json", ids)
		if !ok {
			continue
		}

		liveName := strings.TrimSuffix(name, "."+generationID+".bak")

		err := reconcileOrphanBackup(
			filepath.Join(sessionsRoot, liveName),
			filepath.Join(sessionsRoot, name),
			false,
		)
		if err != nil {
			return err
		}
	}

	return nil
}

func (s *Store) sweepOrphanScrollbackBackupsLocked(ids []string) error {
	scrollRoot := filepath.Join(s.baseDir, scrollbackDir)

	entries, err := os.ReadDir(scrollRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read scrollback dir: %w", err)
	}

	for _, entry := range entries {
		name := entry.Name()

		generationID, ok := matchGenerationBackup(name, "", ids)
		if !ok {
			continue
		}

		liveName := strings.TrimSuffix(name, "."+generationID+".bak")

		err := reconcileOrphanBackup(
			filepath.Join(scrollRoot, liveName),
			filepath.Join(scrollRoot, name),
			true,
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// matchGenerationBackup reports the known generation id embedded in a backup
// name such as "name.json.<id>.bak" (liveSuffix ".json") or "name.<id>.bak".
// Generation ids contain dots, so the id is matched against the known set
// rather than parsed from the last dot.
func matchGenerationBackup(name, liveSuffix string, ids []string) (string, bool) {
	for _, generationID := range ids {
		suffix := liveSuffix + "." + generationID + ".bak"
		if strings.HasSuffix(name, suffix) {
			return generationID, true
		}
	}

	return "", false
}

func reconcileOrphanBackup(livePath, backupPath string, dir bool) error {
	_, err := os.Lstat(livePath)
	if err == nil {
		removeErr := removeArtifact(backupPath, dir)
		if removeErr != nil {
			return removeErr
		}

		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat live artifact: %w", err)
	}

	err = os.Rename(backupPath, livePath)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("restore orphan backup: %w", err)
	}

	return nil
}

// sweepLegacyArtifactsLocked removes staging residue produced by older
// lazy-tmux versions: fixed-name .tmp files/directories and fixed-name .bak
// directories. Named repair backups are intentionally left alone.
func (s *Store) sweepLegacyArtifactsLocked() error {
	err := s.sweepLegacySessionArtifactsLocked()
	if err != nil {
		return err
	}

	return s.sweepLegacyScrollbackArtifactsLocked()
}

func (s *Store) sweepLegacySessionArtifactsLocked() error {
	sessionsRoot := filepath.Join(s.baseDir, sessionsDirName)

	entries, err := os.ReadDir(sessionsRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read sessions dir: %w", err)
	}

	for _, entry := range entries {
		name := entry.Name()

		switch {
		case strings.HasSuffix(name, ".json.tmp"):
			err := os.Remove(filepath.Join(sessionsRoot, name))
			if err != nil && !errors.Is(err, os.ErrNotExist) {
				return fmt.Errorf("remove staged session file: %w", err)
			}
		case strings.HasSuffix(name, ".json.bak"):
			liveName := strings.TrimSuffix(name, ".bak")

			err := reconcileOrphanBackup(
				filepath.Join(sessionsRoot, liveName),
				filepath.Join(sessionsRoot, name),
				false,
			)
			if err != nil {
				return err
			}
		}
	}

	err = os.Remove(s.indexPath() + ".tmp")
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove staged index file: %w", err)
	}

	return nil
}

func (s *Store) sweepLegacyScrollbackArtifactsLocked() error {
	scrollRoot := filepath.Join(s.baseDir, scrollbackDir)

	entries, err := os.ReadDir(scrollRoot)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("read scrollback dir: %w", err)
	}

	for _, entry := range entries {
		name := entry.Name()

		switch {
		case strings.HasSuffix(name, ".tmp"):
			err := os.RemoveAll(filepath.Join(scrollRoot, name))
			if err != nil {
				return fmt.Errorf("remove staged scrollback dir: %w", err)
			}
		case strings.HasSuffix(name, ".bak"):
			liveName := strings.TrimSuffix(name, ".bak")

			err := reconcileOrphanBackup(
				filepath.Join(scrollRoot, liveName),
				filepath.Join(scrollRoot, name),
				true,
			)
			if err != nil {
				return err
			}
		}
	}

	return nil
}
