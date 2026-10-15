package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/alchemmist/lazy-tmux/internal/config"
	"github.com/alchemmist/lazy-tmux/internal/integration"
	"github.com/alchemmist/lazy-tmux/internal/integration/antex"
	"github.com/alchemmist/lazy-tmux/internal/integration/claude"
	"github.com/alchemmist/lazy-tmux/internal/integration/codex"
	"github.com/alchemmist/lazy-tmux/internal/snapshot"
	"github.com/alchemmist/lazy-tmux/internal/store"
	"github.com/alchemmist/lazy-tmux/internal/tmux"
)

const saveAllWorkerLimit = 4

var (
	errSessionNameEmpty    = errors.New("session name is empty")
	errWindowNameEmpty     = errors.New("window name is empty")
	errSourceSessionEmpty  = errors.New("source session is empty")
	errWindowNotInSnapshot = errors.New("window not found in snapshot")
	errAlreadyExists       = errors.New("already exists")
	errAlreadyInStorage    = errors.New("already exists in storage")
	errAlreadyAwake        = errors.New("is already awake")
	errNotRunning          = errors.New("is not running")
	errDaemonRunning       = errors.New("daemon already running")
)

type tmuxSessionManager interface {
	ListSessions() ([]string, error)
	SessionsLastAttached() map[string]time.Time
	CurrentSession() (string, error)
	SessionExists(name string) bool
	SocketPath() string
}

type tmuxSessionCapturer interface {
	CaptureSession(name string) (snapshot.SessionSnapshot, error)
	CapturePaneScrollback(target string, lines int) (string, error)
}

type tmuxSessionRestorer interface {
	RestoreSession(ctx context.Context, snap snapshot.SessionSnapshot) error
	SwitchClient(target string) error
	SynchronizeWindowSize(target string) error
	AttachSession(target string) error
	InsideTmux() bool
}

type tmuxSessionMutator interface {
	NewSession(name string) error
	NewWindow(session, name string) error
	KillWindow(session string, windowIndex int) error
	KillSession(session string) error
	RenameWindow(session string, windowIndex int, name string) error
	RenameSession(session, name string) error
}

type tmuxClient interface {
	tmuxSessionManager
	tmuxSessionCapturer
	tmuxSessionRestorer
	tmuxSessionMutator
}

type App struct {
	cfg               config.Config
	store             *store.Store
	serverSocket      string
	tmux              tmuxClient
	integrations      *integration.Registry
	saveAllFn         func() error
	pickerCacheMu     sync.Mutex
	pickerCache       map[string]cachedPickerSnapshot
	pickerLoads       map[pickerCacheKey]*pickerSnapshotLoad
	pickerCacheMisses int
}

type cachedPickerSnapshot struct {
	capturedAt time.Time
	file       string
	snapshot   snapshot.SessionSnapshot
}

type pickerCacheKey struct {
	serverID    string
	sessionName string
	file        string
	capturedAt  time.Time
}

type pickerSnapshotLoad struct {
	done     chan struct{}
	snapshot snapshot.SessionSnapshot
	err      error
}

func New(cfg config.Config) *App {
	client := tmux.NewClient(config.ExpandHome(cfg.TmuxBin))
	client.SetRestoreTimeout(cfg.RestoreTimeout)
	client.SetRestoreAllowlist(cfg.RestoreAllowlist)
	client.SetRestoreDenylist(cfg.RestoreDenylist)

	registry := buildRegistry(cfg.Integrations, ClaudeStatusDir(cfg.DataDir))
	client.SetRestoreResolver(registry)

	serverSocket := client.ServerIdentity()

	return &App{
		cfg:               cfg,
		store:             store.NewForServer(cfg.DataDir, serverSocket),
		serverSocket:      serverSocket,
		tmux:              client,
		integrations:      registry,
		pickerCacheMu:     sync.Mutex{},
		pickerCache:       make(map[string]cachedPickerSnapshot),
		pickerLoads:       make(map[pickerCacheKey]*pickerSnapshotLoad),
		pickerCacheMisses: 0,
	}
}

// ServerSocket returns the tmux server socket path this app is bound to.
func (a *App) ServerSocket() string {
	return a.serverSocket
}

func ClaudeStatusDir(dataDir string) string {
	return filepath.Join(dataDir, "claude-status")
}

func buildRegistry(cfg config.IntegrationsConfig, statusDir string) *integration.Registry {
	if !cfg.Enabled {
		return integration.NewRegistry()
	}

	var items []integration.Integration

	if cfg.Codex.Enabled {
		items = append(items, codex.New(config.ExpandHome(cfg.Codex.Home)))
	}
	if cfg.Claude.Enabled {
		items = append(items, claude.New(config.ExpandHome(cfg.Claude.Home), statusDir))
	}

	if cfg.Antex.Enabled {
		items = append(items, antex.New(config.ExpandHome(cfg.Antex.Home)))
	}

	return integration.NewRegistry(items...)
}

func (a *App) SaveAll() (int, error) {
	sessions, err := a.tmux.ListSessions()
	if err != nil {
		return 0, fmt.Errorf("list sessions: %w", err)
	}

	integrationScope := a.integrations.Scope()
	errs := parallelMap(sessions, saveAllWorkerLimit, func(session string) error {
		return a.saveSession(session, integrationScope)
	})
	for _, err := range errs {
		if err != nil {
			return 0, err
		}
	}

	return len(sessions), nil
}

func (a *App) SaveSession(session string) error {
	return a.saveSession(session, a.integrations.Scope())
}

func (a *App) SaveCurrent() error {
	name, err := a.tmux.CurrentSession()
	if err != nil {
		return fmt.Errorf("get current session: %w", err)
	}

	return a.SaveSession(name)
}

func (a *App) Restore(session string, switchClient bool) error {
	return a.RestoreTarget(PickerTarget{SessionName: session}, switchClient)
}

func (a *App) RestoreTarget(target PickerTarget, switchClient bool) error {
	preExisted := a.tmux.SessionExists(strings.TrimSpace(target.SessionName))
	err := a.restoreSessionForTarget(context.Background(), target)
	if err != nil {
		return err
	}

	if switchClient {
		return a.handoffToTarget(target, false, !preExisted)
	}

	return nil
}

func (a *App) Bootstrap(session string) error {
	target := strings.TrimSpace(session)
	if target == "" || target == "last" {
		return a.bootstrapLatest()
	}

	return a.Restore(target, true)
}

func (a *App) ListRecords() ([]snapshot.Record, error) {
	records, err := a.store.ListRecords()
	if err != nil {
		return nil, fmt.Errorf("list records: %w", err)
	}

	return records, nil
}

func (a *App) bootstrapLatest() error {
	rec, err := a.store.LatestRecord()
	if err != nil {
		if !os.IsNotExist(err) {
			return fmt.Errorf("get latest record: %w", err)
		}

		// Upgraded single-server installs may still have their snapshots
		// parked in the selectable legacy namespace.
		rec, err = a.latestLegacyRecord()
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return fmt.Errorf("get latest legacy record: %w", err)
		}
	}

	return a.RestoreTarget(PickerTarget{
		SessionName: rec.SessionName,
		ServerID:    store.ServerID(rec.ServerSocket),
	}, true)
}

func (a *App) latestLegacyRecord() (snapshot.Record, error) {
	legacy := a.legacyStore()
	if legacy == nil {
		return snapshot.Record{}, os.ErrNotExist
	}

	rec, err := legacy.LatestRecord()
	if err != nil {
		return snapshot.Record{}, fmt.Errorf("get latest legacy record: %w", err)
	}

	return rec, nil
}

func (a *App) currentServerID() string {
	return store.ServerID(a.serverSocket)
}

// targetStore resolves the namespace store of a picker target. An empty
// server id addresses the command's current server.
func (a *App) targetStore(serverID string) (*store.Store, error) {
	serverID = strings.TrimSpace(serverID)
	if serverID == "" || serverID == a.currentServerID() {
		return a.store, nil
	}

	target, err := a.store.ForServerID(serverID)
	if err != nil {
		return nil, fmt.Errorf("resolve session namespace %q: %w", serverID, err)
	}

	return target, nil
}

// legacyStore returns the selectable legacy namespace store, or nil when the
// data directory has no legacy records at all.
func (a *App) legacyStore() *store.Store {
	legacy, err := a.store.ForServerID(store.LegacyServerID)
	if err != nil {
		return nil
	}

	return legacy
}

// storeForName resolves a bare session name given on the command line: current
// server first, then selectable legacy records. Foreign servers are never
// chosen implicitly when names collide.
func (a *App) storeForName(name string) (*store.Store, error) {
	exists, err := a.store.SessionExists(name)
	if err != nil {
		return nil, fmt.Errorf("check current namespace: %w", err)
	}
	if exists {
		return a.store, nil
	}

	legacy := a.legacyStore()
	if legacy == nil {
		return a.store, nil
	}

	legacyExists, err := legacy.SessionExists(name)
	if err != nil {
		return nil, fmt.Errorf("check legacy namespace: %w", err)
	}
	if legacyExists {
		return legacy, nil
	}

	return a.store, nil
}

func (a *App) saveSession(session string, integrationScope *integration.Registry) error {
	snap, err := a.tmux.CaptureSession(session)
	if err != nil {
		return fmt.Errorf("capture session: %w", err)
	}

	if a.cfg.Scrollback.Enabled {
		a.captureShellScrollback(&snap)
	}

	integrationScope.Enrich(&snap)

	err = a.store.SaveSession(snap)
	if err != nil {
		return fmt.Errorf("save session: %w", err)
	}

	return nil
}

func (a *App) restoreSessionForTarget(ctx context.Context, target PickerTarget) error {
	session := strings.TrimSpace(target.SessionName)
	if session == "" {
		return errSessionNameEmpty
	}

	storage, err := a.restoreStorage(target, session)
	if err != nil {
		return err
	}

	snap, err := storage.LoadSession(session)
	if err != nil {
		return fmt.Errorf("load session: %w", err)
	}

	err = a.tmux.RestoreSession(ctx, snap)
	if err != nil && !errors.Is(err, tmux.ErrSessionExists) {
		return fmt.Errorf("restore session: %w", err)
	}

	err = storage.MarkSessionAccessed(session, time.Now().UTC())
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("mark session accessed: %w", err)
	}

	return nil
}

// restoreStorage selects the namespace a restore reads from. Picker targets
// address an explicit server namespace; bare command-line names resolve
// current-server first and legacy snapshots second.
func (a *App) restoreStorage(target PickerTarget, session string) (*store.Store, error) {
	if strings.TrimSpace(target.ServerID) != "" {
		storage, err := a.targetStore(target.ServerID)
		if err != nil {
			return nil, fmt.Errorf("resolve session server: %w", err)
		}

		return storage, nil
	}

	storage, err := a.storeForName(session)
	if err != nil {
		return nil, fmt.Errorf("resolve session storage: %w", err)
	}

	return storage, nil
}

func (a *App) handoffToTarget(
	target PickerTarget,
	allowAttach bool,
	synchronizeSize bool,
) error {
	session := strings.TrimSpace(target.SessionName)

	switchTarget := session
	if target.WindowIndex != nil {
		switchTarget = fmt.Sprintf("%s:%d", session, *target.WindowIndex)
	}

	if a.tmux.InsideTmux() {
		err := a.tmux.SwitchClient(switchTarget)
		if err != nil {
			return fmt.Errorf("switch client: %w", err)
		}
		if synchronizeSize {
			err = a.tmux.SynchronizeWindowSize(switchTarget)
			if err != nil {
				return fmt.Errorf("synchronize window size: %w", err)
			}
		}

		return nil
	}

	if !allowAttach {
		return nil
	}

	err := a.tmux.AttachSession(switchTarget)
	if err != nil {
		return fmt.Errorf("attach session: %w", err)
	}

	return nil
}

func (a *App) runDaemonSaveAll() error {
	if a.saveAllFn != nil {
		return a.saveAllFn()
	}

	_, err := a.SaveAll()

	return err
}

func (a *App) captureShellScrollback(snap *snapshot.SessionSnapshot) {
	lines := a.cfg.Scrollback.Lines
	if lines <= 0 {
		lines = 5000
	}

	for win := range snap.Windows {
		for pi := range snap.Windows[win].Panes {
			pane := &snap.Windows[win].Panes[pi]
			if strings.TrimSpace(pane.RestoreCmd) != "" || !isShellCommandName(pane.CurrentCmd) {
				continue
			}

			target := tmux.PaneTarget(snap.SessionName, snap.Windows[win].Index, pane.Index)

			out, err := a.tmux.CapturePaneScrollback(target, lines)
			if err != nil {
				continue
			}

			out = strings.TrimRight(out, "\n")
			if strings.TrimSpace(out) == "" {
				continue
			}

			pane.Scrollback = &snapshot.ScrollbackRef{
				Content: out + "\n",
				Ref:     "",
				Lines:   0,
				Bytes:   0,
			}
		}
	}
}

func isShellCommandName(cmd string) bool {
	fields := strings.Fields(strings.TrimSpace(cmd))
	if len(fields) == 0 {
		return false
	}

	base := strings.TrimPrefix(filepath.Base(fields[0]), "-")
	switch base {
	case "bash", "zsh", "fish", "sh", "ksh":
		return true
	default:
		return false
	}
}
