package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/alchemmist/lazy-tmux/internal/snapshot"
)

var (
	errEmptySessionName         = errors.New("empty session name")
	errEmptyScrollbackRef       = errors.New("empty scrollback ref")
	errScrollbackRefOutsideBase = errors.New("invalid scrollback ref outside base dir")
	errInvalidScrollbackSession = errors.New("invalid session name for scrollback")
	errPathOutsideBase          = errors.New("invalid path outside base dir")
)

const (
	indexFileName      = "index.json"
	sessionsDirName    = "sessions"
	scrollbackDir      = "scrollback"
	generationLockName = ".index.lock"
	defaultDirPerm     = 0o755
	defaultFilePerm    = 0o644
	scrollbackDirPerm  = 0o700
	scrollbackFilePerm = 0o600
)

type Store struct {
	baseDir string
	// genHook is used by internal tests to inject failures and crash points.
	genHook *generationHooks
}

func New(baseDir string) *Store {
	return &Store{baseDir: baseDir}
}

func DefaultDataDir() string {
	if v := strings.TrimSpace(os.Getenv("LAZY_TMUX_DATA_DIR")); v != "" {
		return v
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ".lazy-tmux"
	}

	return filepath.Join(home, ".local", "share", "lazy-tmux")
}

// SaveSession publishes one generation covering a single session. Manual saves
// take the same generation boundary as background batches, so they can never
// expose a mixed generation and never affect other sessions.
func (s *Store) SaveSession(sessionSnapshot snapshot.SessionSnapshot) error {
	if sessionSnapshot.SessionName == "" {
		return errEmptySessionName
	}

	return s.SaveSessions([]snapshot.SessionSnapshot{sessionSnapshot})
}

func (s *Store) DeleteSession(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errEmptySessionName
	}

	return s.withGenerationLock(syscall.LOCK_EX, func() error {
		err := s.recoverGenerationsLocked()
		if err != nil {
			return err
		}

		return s.deleteSessionUnlocked(name)
	})
}

func (s *Store) LoadSession(name string) (snapshot.SessionSnapshot, error) {
	var out snapshot.SessionSnapshot

	err := s.withGenerationLock(syscall.LOCK_SH, func() error {
		loaded, loadErr := s.loadSessionUnlocked(name, true)
		out = loaded

		return loadErr
	})

	return out, err
}

func (s *Store) LoadSessionMetadata(name string) (snapshot.SessionSnapshot, error) {
	var out snapshot.SessionSnapshot

	err := s.withGenerationLock(syscall.LOCK_SH, func() error {
		loaded, loadErr := s.loadSessionUnlocked(name, false)
		out = loaded

		return loadErr
	})

	return out, err
}

func (s *Store) SessionPath(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errEmptySessionName
	}

	return s.sessionPath(name), nil
}

func (s *Store) SessionExists(name string) (bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, errEmptySessionName
	}

	exists := false

	err := s.withGenerationLock(syscall.LOCK_SH, func() error {
		path := s.sessionPath(name)

		_, statErr := os.Stat(path)
		if statErr != nil {
			if errors.Is(statErr, os.ErrNotExist) {
				return nil
			}

			return fmt.Errorf("stat session file: %w", statErr)
		}

		exists = true

		return nil
	})

	return exists, err
}

func (s *Store) ListRecords() ([]snapshot.Record, error) {
	var records []snapshot.Record

	err := s.withGenerationLock(syscall.LOCK_SH, func() error {
		idx, loadErr := s.loadIndexUnlocked()
		if loadErr != nil {
			return loadErr
		}

		records = make([]snapshot.Record, 0, len(idx.Sessions))
		for _, r := range idx.Sessions {
			records = append(records, r)
		}

		return nil
	})
	if err != nil {
		return nil, err
	}

	sort.Slice(records, func(i, j int) bool {
		if records[i].CapturedAt.Equal(records[j].CapturedAt) {
			return records[i].SessionName < records[j].SessionName
		}

		return records[i].CapturedAt.After(records[j].CapturedAt)
	})

	return records, nil
}

func (s *Store) LatestRecord() (snapshot.Record, error) {
	recs, err := s.ListRecords()
	if err != nil {
		return snapshot.Record{}, err
	}

	if len(recs) == 0 {
		return snapshot.Record{}, os.ErrNotExist
	}

	return recs[0], nil
}

func (s *Store) ScrollbackExists(name string) (bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, errEmptySessionName
	}

	safeName, err := safeScrollbackSessionName(name)
	if err != nil {
		return false, err
	}

	scrollRoot := filepath.Clean(filepath.Join(s.baseDir, scrollbackDir))
	sessionDir := filepath.Clean(filepath.Join(scrollRoot, safeName))

	_, err = os.Stat(sessionDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}

		return false, fmt.Errorf("stat scrollback dir: %w", err)
	}

	return true, nil
}

func (s *Store) IndexEntryExists(name string) (bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, errEmptySessionName
	}

	exists := false

	err := s.withGenerationLock(syscall.LOCK_SH, func() error {
		idx, loadErr := s.loadIndexUnlocked()
		if loadErr != nil {
			return loadErr
		}

		_, exists = idx.Sessions[name]

		return nil
	})

	return exists, err
}

func (s *Store) MarkSessionAccessed(name string, accessTime time.Time) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errEmptySessionName
	}

	if accessTime.IsZero() {
		accessTime = time.Now().UTC()
	}

	return s.withGenerationLock(syscall.LOCK_EX, func() error {
		err := s.recoverGenerationsLocked()
		if err != nil {
			return err
		}

		idx, err := s.loadIndexUnlocked()
		if err != nil {
			return err
		}

		rec, ok := idx.Sessions[name]
		if !ok {
			return os.ErrNotExist
		}

		rec.LastAccessed = accessTime.UTC()
		idx.Sessions[name] = rec
		idx.Updated = time.Now().UTC()

		return writeJSONAtomic(s.indexPath(), idx)
	})
}

// withGenerationLock serializes snapshot generations. Publishers take an
// exclusive lock; readers (index list, session load) take a shared lock so a
// generation never exposes a mix of pre- and post-publish artifacts. Locks are
// per open file description, so separate fds contend even inside one process.
func (s *Store) withGenerationLock(how int, run func() error) error {
	err := os.MkdirAll(s.baseDir, defaultDirPerm)
	if err != nil {
		return fmt.Errorf("create data dir: %w", err)
	}

	lockPath := filepath.Join(s.baseDir, generationLockName)

	// #nosec G304 -- fixed lock name under the configured data directory
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("open generation lock: %w", err)
	}

	defer func() { _ = file.Close() }()

	err = syscall.Flock(int(file.Fd()), how)
	if err != nil {
		return fmt.Errorf("acquire generation lock: %w", err)
	}

	defer func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN) }()

	return run()
}

func (s *Store) deleteSessionUnlocked(name string) error {
	path := s.sessionPath(name)

	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove session file: %w", err)
	}

	safeName, err := safeScrollbackSessionName(name)
	if err != nil {
		return err
	}

	scrollRoot := filepath.Clean(filepath.Join(s.baseDir, scrollbackDir))
	sessionDir := filepath.Clean(filepath.Join(scrollRoot, safeName))

	err = ensureUnderDir(scrollRoot, sessionDir, name)
	if err != nil {
		return err
	}

	err = os.RemoveAll(sessionDir)
	if err != nil {
		return fmt.Errorf("remove scrollback dir: %w", err)
	}

	idx, err := s.loadIndexUnlocked()
	if err != nil {
		return err
	}

	if idx.Sessions != nil {
		delete(idx.Sessions, name)
	}

	idx.Updated = time.Now().UTC()

	return writeJSONAtomic(s.indexPath(), idx)
}

func (s *Store) loadSessionUnlocked(name string, hydrate bool) (snapshot.SessionSnapshot, error) {
	var out snapshot.SessionSnapshot
	path := s.sessionPath(name)

	data, err := os.ReadFile(
		path,
	) // #nosec G304 -- sessionPath sanitizes the name under the data dir
	if err != nil {
		return out, fmt.Errorf("read session file: %w", err)
	}

	err = validateSnapshotVersion(data)
	if err != nil {
		return out, err
	}
	err = json.Unmarshal(data, &out)
	if err != nil {
		return out, fmt.Errorf("unmarshal session: %w", err)
	}

	if hydrate {
		err = s.hydrateScrollback(&out)
		if err != nil {
			return out, err
		}
	}

	return out, nil
}

func (s *Store) ensureLayout() error {
	err := os.MkdirAll(filepath.Join(s.baseDir, sessionsDirName), defaultDirPerm)
	if err != nil {
		return fmt.Errorf("create sessions dir: %w", err)
	}

	err = os.MkdirAll(filepath.Join(s.baseDir, scrollbackDir), scrollbackDirPerm)
	if err != nil {
		return fmt.Errorf("create scrollback dir: %w", err)
	}

	return nil
}

func (s *Store) loadIndexUnlocked() (snapshot.Index, error) {
	p := s.indexPath()

	fileContent, err := os.ReadFile(p) // #nosec G304 -- fixed index path under the data dir
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return snapshot.Index{
				Version:  snapshot.FormatVersion,
				Updated:  time.Now().UTC(),
				Sessions: map[string]snapshot.Record{},
			}, nil
		}

		return snapshot.Index{}, fmt.Errorf("read index file: %w", err)
	}

	var idx snapshot.Index

	err = json.Unmarshal(fileContent, &idx)
	if err != nil {
		return snapshot.Index{}, fmt.Errorf("decode index: %w", err)
	}

	if idx.Sessions == nil {
		idx.Sessions = map[string]snapshot.Record{}
	}

	if idx.Version == 0 {
		idx.Version = snapshot.FormatVersion
	}

	return idx, nil
}

func (s *Store) indexPath() string {
	return filepath.Join(s.baseDir, indexFileName)
}

func (s *Store) sessionPath(name string) string {
	return filepath.Join(s.baseDir, sessionsDirName, sanitizeName(name)+".json")
}

func sanitizeName(name string) string {
	replacer := strings.NewReplacer("/", "_", "\\", "_", " ", "_", ":", "_")

	out := replacer.Replace(strings.TrimSpace(name))
	if out == "" {
		return "session"
	}

	return out
}

type scrollbackEntry struct {
	FileName string
	Content  string
	Ref      string
	Bytes    int
	Lines    int
}

func (s *Store) planScrollbackUnlocked(
	sessionSnapshot *snapshot.SessionSnapshot,
) (string, []scrollbackEntry, error) {
	safeName, err := safeScrollbackSessionName(sessionSnapshot.SessionName)
	if err != nil {
		return "", nil, err
	}

	entries := make([]scrollbackEntry, 0)

	for windowIndex := range sessionSnapshot.Windows {
		for pi := range sessionSnapshot.Windows[windowIndex].Panes {
			pane := &sessionSnapshot.Windows[windowIndex].Panes[pi]
			if pane.Scrollback == nil {
				continue
			}

			content := pane.Scrollback.Content
			if strings.TrimSpace(content) == "" {
				pane.Scrollback = nil

				continue
			}

			fileName := fmt.Sprintf(
				"w%d_p%d.log",
				sessionSnapshot.Windows[windowIndex].Index,
				pane.Index,
			)
			ref := filepath.Join(scrollbackDir, safeName, fileName)
			pane.Scrollback.Ref = ref
			pane.Scrollback.Bytes = len(content)
			pane.Scrollback.Lines = countLines(content)
			pane.Scrollback.Content = ""

			entries = append(entries, scrollbackEntry{
				FileName: fileName,
				Content:  content,
				Ref:      ref,
				Bytes:    len(content),
				Lines:    countLines(content),
			})
		}
	}

	return safeName, entries, nil
}

func (s *Store) hydrateScrollback(sessionSnapshot *snapshot.SessionSnapshot) error {
	baseRoot, err := filepath.Abs(filepath.Clean(filepath.Join(s.baseDir, scrollbackDir)))
	if err != nil {
		return fmt.Errorf("get base root: %w", err)
	}

	for wi := range sessionSnapshot.Windows {
		for pi := range sessionSnapshot.Windows[wi].Panes {
			pane := &sessionSnapshot.Windows[wi].Panes[pi]
			if pane.Scrollback == nil || strings.TrimSpace(pane.Scrollback.Ref) == "" {
				continue
			}

			path, err := safeScrollbackPath(baseRoot, s.baseDir, pane.Scrollback.Ref)
			if err != nil {
				return err
			}

			fileContent, err := os.ReadFile(
				path,
			) // #nosec G304 -- ref validated against the base dir above
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					continue
				}

				return fmt.Errorf("read scrollback file: %w", err)
			}

			pane.Scrollback.Content = string(fileContent)
			if pane.Scrollback.Bytes == 0 {
				pane.Scrollback.Bytes = len(fileContent)
			}

			if pane.Scrollback.Lines == 0 {
				pane.Scrollback.Lines = countLines(string(fileContent))
			}
		}
	}

	return nil
}

func safeScrollbackPath(baseRoot, baseDir, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", errEmptyScrollbackRef
	}

	candidate := filepath.Clean(filepath.Join(baseDir, ref))

	candidateAbs, err := filepath.Abs(candidate)
	if err != nil {
		return "", fmt.Errorf("get absolute path: %w", err)
	}

	baseEval, err := filepath.EvalSymlinks(baseRoot)
	if err != nil {
		baseEval = baseRoot
	}

	candidateDirEval, err := filepath.EvalSymlinks(filepath.Dir(candidateAbs))
	if err != nil {
		candidateDirEval = filepath.Dir(candidateAbs)
	}

	candidateEval := filepath.Join(candidateDirEval, filepath.Base(candidateAbs))

	finalEval, err := filepath.EvalSymlinks(candidateEval)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			finalEval = candidateEval
		} else {
			return "", fmt.Errorf("eval symlinks: %w", err)
		}
	}

	rel, err := filepath.Rel(baseEval, finalEval)
	if err != nil {
		return "", fmt.Errorf("get relative path: %w", err)
	}

	if rel == "." {
		return finalEval, nil
	}

	cleanRel := filepath.Clean(rel)
	if filepath.IsAbs(cleanRel) || cleanRel == ".." ||
		strings.HasPrefix(cleanRel, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("%w: %s", errScrollbackRefOutsideBase, ref)
	}

	return finalEval, nil
}

func safeScrollbackSessionName(sessionName string) (string, error) {
	name := sanitizeName(sessionName)
	if name == "" || name == "." || name == ".." {
		return "", fmt.Errorf("%w: %q", errInvalidScrollbackSession, sessionName)
	}

	if strings.ContainsRune(name, filepath.Separator) {
		return "", fmt.Errorf("%w: %q", errInvalidScrollbackSession, sessionName)
	}

	if filepath.Separator != '/' && strings.Contains(name, "/") {
		return "", fmt.Errorf("%w: %q", errInvalidScrollbackSession, sessionName)
	}

	if filepath.Separator != '\\' && strings.Contains(name, "\\") {
		return "", fmt.Errorf("%w: %q", errInvalidScrollbackSession, sessionName)
	}

	return name, nil
}

func ensureUnderDir(baseDir, child, ref string) error {
	baseAbs, err := filepath.Abs(filepath.Clean(baseDir))
	if err != nil {
		return fmt.Errorf("get base absolute path: %w", err)
	}

	childAbs, err := filepath.Abs(filepath.Clean(child))
	if err != nil {
		return fmt.Errorf("get child absolute path: %w", err)
	}

	rel, err := filepath.Rel(baseAbs, childAbs)
	if err != nil {
		return fmt.Errorf("get relative path: %w", err)
	}

	cleanRel := filepath.Clean(rel)
	if filepath.IsAbs(cleanRel) || cleanRel == ".." ||
		strings.HasPrefix(cleanRel, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("%w: %s", errPathOutsideBase, ref)
	}

	return nil
}

func countLines(s string) int {
	if s == "" {
		return 0
	}

	return strings.Count(s, "\n") + 1
}

func writeJSONAtomic(path string, v any) error {
	jsonData, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal json: %w", err)
	}

	return writeFileAtomic(path, append(jsonData, '\n'), defaultFilePerm)
}
