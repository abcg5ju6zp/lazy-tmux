package store

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
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
	indexFileName       = "index.json"
	sessionsDirName     = "sessions"
	scrollbackDir       = "scrollback"
	sessionLocksDirName = ".session-locks"
	defaultDirPerm      = 0o755
	defaultFilePerm     = 0o644
	scrollbackDirPerm   = 0o700
	scrollbackFilePerm  = 0o600
)

type storeLocks struct {
	sessionsMu  sync.Mutex
	sessionLock map[string]*sync.Mutex
}

func newStoreLocks() *storeLocks {
	return &storeLocks{sessionLock: make(map[string]*sync.Mutex), sessionsMu: sync.Mutex{}}
}

type Store struct {
	baseDir      string
	serverID     string
	serverSocket string
	locks        *storeLocks
}

// New binds a store to the legacy namespace. Prefer NewForServer.
func New(baseDir string) *Store {
	return NewForServer(baseDir, "")
}

// NewForServer binds a store to the tmux server identified by socketPath.
// Every bound operation only touches that server's namespace.
func NewForServer(baseDir, socketPath string) *Store {
	socketPath = strings.TrimSpace(socketPath)
	if socketPath != "" {
		socketPath = filepath.Clean(socketPath)
	}

	return &Store{
		baseDir:      baseDir,
		serverID:     ServerID(socketPath),
		serverSocket: socketPath,
		locks:        newStoreLocks(),
	}
}

func (s *Store) ServerID() string {
	return s.serverID
}

func (s *Store) ServerSocket() string {
	return s.serverSocket
}

// ForServerID returns a store bound to another namespace discovered through
// ListRecords (e.g. a snapshot belonging to another tmux server). The
// namespace must already exist on disk.
func (s *Store) ForServerID(serverID string) (*Store, error) {
	serverID = strings.TrimSpace(serverID)
	if serverID == "" || serverID == s.serverID {
		return s, nil
	}

	serverNS, err := namespaceByID(s.baseDir, serverID)
	if err != nil {
		return nil, err
	}

	return &Store{
		baseDir:      s.baseDir,
		serverID:     serverNS.id,
		serverSocket: serverNS.socket,
		locks:        s.locks,
	}, nil
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

func (s *Store) SaveSession(sessionSnapshot snapshot.SessionSnapshot) error {
	if sessionSnapshot.SessionName == "" {
		return errEmptySessionName
	}

	serverNS := s.boundNamespace()

	migrationErr := s.ensureMigrated(serverNS)
	if migrationErr != nil {
		return migrationErr
	}

	if sessionSnapshot.CapturedAt.IsZero() {
		sessionSnapshot.CapturedAt = time.Now().UTC()
	}

	unlock, err := s.lockSessionMutation(serverNS, sessionSnapshot.SessionName)
	if err != nil {
		return err
	}
	defer unlock()

	return s.saveSessionUnlocked(serverNS, sessionSnapshot)
}

func (s *Store) DeleteSession(name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errEmptySessionName
	}

	serverNS := s.boundNamespace()

	migrationErr := s.ensureMigrated(serverNS)
	if migrationErr != nil {
		return migrationErr
	}

	unlock, err := s.lockSessionMutation(serverNS, name)
	if err != nil {
		return err
	}
	defer unlock()

	return s.deleteSessionUnlocked(serverNS, name)
}

func (s *Store) LoadSession(name string) (snapshot.SessionSnapshot, error) {
	return s.loadSession(name, true)
}

func (s *Store) LoadSessionMetadata(name string) (snapshot.SessionSnapshot, error) {
	return s.loadSession(name, false)
}

func (s *Store) SessionPath(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return "", errEmptySessionName
	}

	return s.boundNamespace().sessionPath(name), nil
}

func (s *Store) SessionExists(name string) (bool, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return false, errEmptySessionName
	}

	serverNS := s.boundNamespace()

	migrationErr := s.ensureMigrated(serverNS)
	if migrationErr != nil {
		return false, migrationErr
	}

	unlock := s.lockSession(serverNS, name)
	defer unlock()

	_, err := os.Stat(serverNS.sessionPath(name))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}

		return false, fmt.Errorf("stat session file: %w", err)
	}

	return true, nil
}

// ListRecords returns records from every server namespace visible in the data
// directory. Each record carries the owning server socket so callers can tell
// current-server sessions apart from foreign and legacy ones.
func (s *Store) ListRecords() ([]snapshot.Record, error) {
	migrationErr := s.ensureMigrated(s.boundNamespace())
	if migrationErr != nil {
		return nil, migrationErr
	}

	records := make([]snapshot.Record, 0)

	if legacy := legacyNamespace(s.baseDir); dirExists(legacy.root) {
		idx, err := s.loadIndexUnlocked(legacy)
		if err != nil {
			return nil, err
		}
		records = appendIndexRecords(records, idx, "")
	}

	serversRoot := filepath.Join(s.baseDir, serversDirName)
	entries, err := os.ReadDir(serversRoot)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return sortRecords(records), nil
		}

		return nil, fmt.Errorf("read server namespaces: %w", err)
	}

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		root := filepath.Join(serversRoot, entry.Name())
		socket := readNamespaceSocket(root)
		serverNS := namespace{id: entry.Name(), socket: socket, root: root}

		idxPath := serverNS.indexPath()
		_, idxStatErr := os.Stat(idxPath)
		if idxStatErr != nil {
			if errors.Is(idxStatErr, os.ErrNotExist) {
				continue
			}

			return nil, fmt.Errorf("stat namespace index %s: %w", entry.Name(), idxStatErr)
		}

		idx, err := s.loadIndexUnlocked(serverNS)
		if err != nil {
			return nil, err
		}
		records = appendIndexRecords(records, idx, socket)
	}

	return sortRecords(records), nil
}

func appendIndexRecords(
	records []snapshot.Record,
	idx snapshot.Index,
	socket string,
) []snapshot.Record {
	for _, rec := range idx.Sessions {
		if strings.TrimSpace(rec.ServerSocket) == "" {
			rec.ServerSocket = socket
		}

		records = append(records, rec)
	}

	return records
}

func sortRecords(records []snapshot.Record) []snapshot.Record {
	sort.Slice(records, func(left, right int) bool {
		if records[left].CapturedAt.Equal(records[right].CapturedAt) {
			if records[left].ServerSocket == records[right].ServerSocket {
				return records[left].SessionName < records[right].SessionName
			}

			return records[left].ServerSocket < records[right].ServerSocket
		}

		return records[left].CapturedAt.After(records[right].CapturedAt)
	})

	return records
}

func dirExists(path string) bool {
	info, err := os.Stat(path)

	return err == nil && info.IsDir()
}

func (s *Store) LatestRecord() (snapshot.Record, error) {
	serverNS := s.boundNamespace()

	migrationErr := s.ensureMigrated(serverNS)
	if migrationErr != nil {
		return snapshot.Record{}, migrationErr
	}

	idx, err := s.loadIndexUnlocked(serverNS)
	if err != nil {
		return snapshot.Record{}, err
	}

	records := sortRecords(appendIndexRecords(nil, idx, serverNS.socket))
	if len(records) == 0 {
		return snapshot.Record{}, os.ErrNotExist
	}

	return records[0], nil
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

	serverNS := s.boundNamespace()
	sessionDir := filepath.Clean(filepath.Join(serverNS.scrollRoot(), safeName))

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

	serverNS := s.boundNamespace()

	migrationErr := s.ensureMigrated(serverNS)
	if migrationErr != nil {
		return false, migrationErr
	}

	idx, err := s.loadIndexUnlocked(serverNS)
	if err != nil {
		return false, err
	}

	_, ok := idx.Sessions[name]

	return ok, nil
}

func (s *Store) MarkSessionAccessed(name string, accessTime time.Time) error {
	name = strings.TrimSpace(name)
	if name == "" {
		return errEmptySessionName
	}

	if accessTime.IsZero() {
		accessTime = time.Now().UTC()
	}

	serverNS := s.boundNamespace()

	migrationErr := s.ensureMigrated(serverNS)
	if migrationErr != nil {
		return migrationErr
	}

	return s.withIndexLock(serverNS, func() error {
		idx, err := s.loadIndexUnlocked(serverNS)
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

		return writeJSONAtomic(serverNS.indexPath(), idx)
	})
}

func (s *Store) boundNamespace() namespace {
	return namespaceFor(s.baseDir, s.serverSocket)
}

func (s *Store) deleteSessionUnlocked(serverNS namespace, name string) error {
	path := serverNS.sessionPath(name)

	err := os.Remove(path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove session file: %w", err)
	}

	safeName, err := safeScrollbackSessionName(name)
	if err != nil {
		return err
	}

	scrollRoot := filepath.Clean(serverNS.scrollRoot())
	sessionDir := filepath.Clean(filepath.Join(scrollRoot, safeName))

	err = ensureUnderDir(scrollRoot, sessionDir, name)
	if err != nil {
		return err
	}

	err = os.RemoveAll(sessionDir)
	if err != nil {
		return fmt.Errorf("remove scrollback dir: %w", err)
	}

	return s.withIndexLock(serverNS, func() error {
		idx, err := s.loadIndexUnlocked(serverNS)
		if err != nil {
			return err
		}

		if idx.Sessions != nil {
			delete(idx.Sessions, name)
		}

		idx.Updated = time.Now().UTC()

		return writeJSONAtomic(serverNS.indexPath(), idx)
	})
}

func (s *Store) lockKey(serverNS namespace, name string) string {
	return serverNS.id + "/" + name
}

func (s *Store) lockSession(serverNS namespace, name string) func() {
	s.locks.sessionsMu.Lock()
	lock, ok := s.locks.sessionLock[s.lockKey(serverNS, name)]
	if !ok {
		lock = &sync.Mutex{}
		s.locks.sessionLock[s.lockKey(serverNS, name)] = lock
	}
	s.locks.sessionsMu.Unlock()

	lock.Lock()

	return lock.Unlock
}

func (s *Store) lockSessionMutation(serverNS namespace, name string) (func(), error) {
	unlockLocal := s.lockSession(serverNS, name)

	safeName, err := safeScrollbackSessionName(name)
	if err != nil {
		unlockLocal()

		return nil, err
	}

	lockMkdirErr := os.MkdirAll(serverNS.locksDir(), 0o700)
	if lockMkdirErr != nil {
		unlockLocal()

		return nil, fmt.Errorf("create session lock dir: %w", lockMkdirErr)
	}

	lockPath := filepath.Join(serverNS.locksDir(), safeName+".lock")
	file, err := os.OpenFile( // #nosec G304 -- sanitized lock name under the namespace directory
		lockPath,
		os.O_CREATE|os.O_RDWR,
		0o600,
	)
	if err != nil {
		unlockLocal()

		return nil, fmt.Errorf("open session lock: %w", err)
	}
	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX)
	if err != nil {
		_ = file.Close()
		unlockLocal()

		return nil, fmt.Errorf("lock session: %w", err)
	}

	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
		unlockLocal()
	}, nil
}

func (s *Store) withIndexLock(serverNS namespace, run func() error) error {
	mkdirErr := os.MkdirAll(serverNS.root, defaultDirPerm)
	if mkdirErr != nil {
		return fmt.Errorf("create namespace dir: %w", mkdirErr)
	}

	file, err := os.OpenFile( // #nosec G304 -- fixed lock name under the namespace directory
		serverNS.indexLockPath(),
		os.O_CREATE|os.O_RDWR,
		0o600,
	)
	if err != nil {
		return fmt.Errorf("open index lock: %w", err)
	}
	defer func() { _ = file.Close() }()

	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX)
	if err != nil {
		return fmt.Errorf("lock index: %w", err)
	}
	defer func() { _ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN) }()

	return run()
}

func (s *Store) loadSession(name string, hydrate bool) (snapshot.SessionSnapshot, error) {
	var out snapshot.SessionSnapshot

	serverNS := s.boundNamespace()

	migrationErr := s.ensureMigrated(serverNS)
	if migrationErr != nil {
		return out, migrationErr
	}

	unlock, err := s.lockSessionMutation(serverNS, name)
	if err != nil {
		return out, err
	}
	defer unlock()

	return s.loadSessionUnlocked(serverNS, name, hydrate)
}

func (s *Store) loadSessionUnlocked(
	serverNS namespace,
	name string,
	hydrate bool,
) (snapshot.SessionSnapshot, error) {
	var out snapshot.SessionSnapshot
	path := serverNS.sessionPath(name)

	data, err := os.ReadFile(
		path,
	) // #nosec G304 -- sessionPath sanitizes the name under the namespace dir
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
		err = s.hydrateScrollback(serverNS, &out)
		if err != nil {
			return out, err
		}
	}

	return out, nil
}

func (s *Store) updateIndex(
	serverNS namespace,
	sessionSnapshot snapshot.SessionSnapshot,
	path string,
) error {
	return s.withIndexLock(serverNS, func() error {
		idx, err := s.loadIndexUnlocked(serverNS)
		if err != nil {
			return err
		}

		panes := 0
		for _, w := range sessionSnapshot.Windows {
			panes += len(w.Panes)
		}

		idx.Sessions[sessionSnapshot.SessionName] = snapshot.Record{
			SessionName:  sessionSnapshot.SessionName,
			File:         filepath.ToSlash(filepath.Join(sessionsDirName, filepath.Base(path))),
			ServerSocket: serverNS.socket,
			CapturedAt:   sessionSnapshot.CapturedAt.UTC(),
			LastAccessed: idx.Sessions[sessionSnapshot.SessionName].LastAccessed,
			Windows:      len(sessionSnapshot.Windows),
			Panes:        panes,
		}
		idx.Updated = time.Now().UTC()

		return writeJSONAtomic(serverNS.indexPath(), idx)
	})
}

func (s *Store) ensureLayout(serverNS namespace) error {
	sessionsMkdirErr := os.MkdirAll(serverNS.sessionsDir(), defaultDirPerm)
	if sessionsMkdirErr != nil {
		return fmt.Errorf("create sessions dir: %w", sessionsMkdirErr)
	}

	scrollbackMkdirErr := os.MkdirAll(serverNS.scrollRoot(), scrollbackDirPerm)
	if scrollbackMkdirErr != nil {
		return fmt.Errorf("create scrollback dir: %w", scrollbackMkdirErr)
	}

	locksMkdirErr := os.MkdirAll(serverNS.locksDir(), defaultDirPerm)
	if locksMkdirErr != nil {
		return fmt.Errorf("create session lock dir: %w", locksMkdirErr)
	}

	identityErr := serverNS.writeIdentity()
	if identityErr != nil {
		return identityErr
	}

	return nil
}

func (s *Store) loadIndexUnlocked(serverNS namespace) (snapshot.Index, error) {
	fileContent, err := os.ReadFile(
		serverNS.indexPath(),
	) // #nosec G304 -- fixed index path under the namespace dir
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

func (s *Store) persistScrollbackUnlocked(
	serverNS namespace,
	sessionName, safeName string,
	entries []scrollbackEntry,
) error {
	scrollRoot := filepath.Clean(serverNS.scrollRoot())
	sessionDir := filepath.Clean(filepath.Join(scrollRoot, safeName))

	err := ensureUnderDir(scrollRoot, sessionDir, sessionName)
	if err != nil {
		return err
	}

	stageDir := sessionDir + ".tmp"
	_ = os.RemoveAll(stageDir)

	defer func() { _ = os.RemoveAll(stageDir) }()

	if len(entries) == 0 {
		_ = os.RemoveAll(sessionDir)
		_ = os.RemoveAll(stageDir)

		return nil
	}

	err = os.MkdirAll(stageDir, scrollbackDirPerm)
	if err != nil {
		return fmt.Errorf("create stage dir: %w", err)
	}

	for _, ent := range entries {
		path := filepath.Join(stageDir, ent.FileName)

		err := os.WriteFile(path, []byte(ent.Content), scrollbackFilePerm)
		if err != nil {
			return fmt.Errorf("write scrollback file: %w", err)
		}
	}

	err = promoteScrollbackStage(sessionDir, stageDir)
	if err != nil {
		return err
	}

	return nil
}

func promoteScrollbackStage(sessionDir, stageDir string) error {
	backupDir := sessionDir + ".bak"
	_ = os.RemoveAll(backupDir)

	hadSessionDir := false

	_, err := os.Stat(sessionDir)
	if err == nil {
		hadSessionDir = true
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("stat session dir: %w", err)
	}

	if hadSessionDir {
		err := os.Rename(sessionDir, backupDir)
		if err != nil {
			return fmt.Errorf("backup session dir: %w", err)
		}
	}

	err = os.Rename(stageDir, sessionDir)
	if err != nil {
		if hadSessionDir {
			_ = os.Rename(backupDir, sessionDir)
		}

		return fmt.Errorf("promote stage dir: %w", err)
	}

	if hadSessionDir {
		_ = os.RemoveAll(backupDir)
	}

	return nil
}

func (s *Store) hydrateScrollback(
	serverNS namespace,
	sessionSnapshot *snapshot.SessionSnapshot,
) error {
	baseRoot, err := filepath.Abs(filepath.Clean(serverNS.scrollRoot()))
	if err != nil {
		return fmt.Errorf("get base root: %w", err)
	}

	for wi := range sessionSnapshot.Windows {
		for pi := range sessionSnapshot.Windows[wi].Panes {
			pane := &sessionSnapshot.Windows[wi].Panes[pi]
			if pane.Scrollback == nil || strings.TrimSpace(pane.Scrollback.Ref) == "" {
				continue
			}

			path, err := safeScrollbackPath(baseRoot, serverNS.root, pane.Scrollback.Ref)
			if err != nil {
				return err
			}

			fileContent, err := os.ReadFile(
				path,
			) // #nosec G304 -- ref validated against the namespace dir above
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

func safeScrollbackPath(baseRoot, namespaceRoot, ref string) (string, error) {
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return "", errEmptyScrollbackRef
	}

	candidate := filepath.Clean(filepath.Join(namespaceRoot, ref))

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
	tmp, err := writeJSONTemp(path, v, defaultFilePerm)
	if err != nil {
		return err
	}

	err = os.Rename(tmp, path)
	if err != nil {
		return fmt.Errorf("rename tmp file: %w", err)
	}

	return nil
}

func writeJSONTemp(path string, v any, perm os.FileMode) (string, error) {
	jsonData, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal json: %w", err)
	}

	tmp := path + ".tmp"

	err = os.WriteFile(tmp, append(jsonData, '\n'), perm)
	if err != nil {
		return "", fmt.Errorf("write temp file: %w", err)
	}

	return tmp, nil
}

func (s *Store) saveSessionUnlocked(
	serverNS namespace,
	sessionSnapshot snapshot.SessionSnapshot,
) error {
	sessionSnapshot.Version = snapshot.FormatVersion
	sessionSnapshot.ServerSocket = serverNS.socket

	layoutErr := s.ensureLayout(serverNS)
	if layoutErr != nil {
		return layoutErr
	}

	safeName, entries, err := s.planScrollbackUnlocked(&sessionSnapshot)
	if err != nil {
		return err
	}

	path := serverNS.sessionPath(sessionSnapshot.SessionName)
	err = backupLegacyAntex(path)
	if err != nil {
		return err
	}

	jsonTmp, err := writeJSONTemp(path, sessionSnapshot, defaultFilePerm)
	if err != nil {
		return err
	}

	defer func() { _ = os.Remove(jsonTmp) }()

	err = s.persistScrollbackUnlocked(
		serverNS,
		sessionSnapshot.SessionName,
		safeName,
		entries,
	)
	if err != nil {
		return err
	}

	err = os.Rename(jsonTmp, path)
	if err != nil {
		return fmt.Errorf("rename tmp file: %w", err)
	}

	return s.updateIndex(serverNS, sessionSnapshot, path)
}
