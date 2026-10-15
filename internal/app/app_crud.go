package app

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/alchemmist/lazy-tmux/internal/snapshot"
	"github.com/alchemmist/lazy-tmux/internal/store"
)

func (a *App) DeleteWindow(serverID, session string, windowIndex int) error {
	storage, err := a.targetStore(serverID)
	if err != nil {
		return fmt.Errorf("resolve session server: %w", err)
	}

	if a.isCurrentServer(serverID) && a.tmux.SessionExists(session) {
		handled, err := a.deleteLiveWindow(storage, session, windowIndex)
		if handled {
			return err
		}
	}

	return a.deleteSnapshotWindow(storage, session, windowIndex)
}

func (a *App) isCurrentServer(serverID string) bool {
	serverID = strings.TrimSpace(serverID)

	return serverID == "" || serverID == a.currentServerID()
}

func (a *App) deleteLiveWindow(
	storage *store.Store,
	session string,
	windowIndex int,
) (bool, error) {
	err := a.tmux.KillWindow(session, windowIndex)
	if err != nil {
		var exitErr *exec.ExitError
		if !errors.As(err, &exitErr) {
			return true, fmt.Errorf("kill window: %w", err)
		}

		return false, nil
	}

	if !a.tmux.SessionExists(session) {
		err := storage.DeleteSession(session)
		if err != nil {
			return true, fmt.Errorf("delete session: %w", err)
		}

		return true, nil
	}

	return true, a.saveLiveSession(storage, session)
}

// saveLiveSession captures the current server's live session into the given
// namespace store. Callers must only use the bound current-server store here.
func (a *App) saveLiveSession(storage *store.Store, session string) error {
	snap, err := a.tmux.CaptureSession(session)
	if err != nil {
		return fmt.Errorf("capture session: %w", err)
	}
	if a.cfg.Scrollback.Enabled {
		a.captureShellScrollback(&snap)
	}
	a.integrations.Scope().Enrich(&snap)

	err = storage.SaveSession(snap)
	if err != nil {
		return fmt.Errorf("save session: %w", err)
	}

	return nil
}

func (a *App) deleteSnapshotWindow(
	storage *store.Store,
	session string,
	windowIndex int,
) error {
	snap, err := storage.LoadSession(session)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("session %q not found: %w", session, os.ErrNotExist)
		}

		return fmt.Errorf("load session: %w", err)
	}

	windows := make([]snapshot.Window, 0, len(snap.Windows))
	removed := false

	for _, write := range snap.Windows {
		if write.Index == windowIndex {
			removed = true

			continue
		}

		windows = append(windows, write)
	}

	if !removed {
		return errWindowNotInSnapshot
	}

	if len(windows) == 0 {
		err := storage.DeleteSession(session)
		if err != nil {
			return fmt.Errorf("delete session: %w", err)
		}

		return nil
	}

	snap.Windows = windows

	err = storage.SaveSession(snap)
	if err != nil {
		return fmt.Errorf("save session: %w", err)
	}

	return nil
}

// Forget removes a stored snapshot. With an empty server id the name is
// resolved in the current server namespace and then in the legacy namespace;
// snapshots of other servers must be addressed explicitly through the picker.
func (a *App) Forget(serverID, session string) error {
	if strings.TrimSpace(session) == "" {
		return errSessionNameEmpty
	}

	session = strings.TrimSpace(session)

	if strings.TrimSpace(serverID) == "" {
		storage, err := a.storeForName(session)
		if err != nil {
			return fmt.Errorf("resolve session storage: %w", err)
		}

		return forgetFromStorage(storage, session)
	}

	storage, err := a.targetStore(serverID)
	if err != nil {
		return fmt.Errorf("resolve session server: %w", err)
	}

	return forgetFromStorage(storage, session)
}

func forgetFromStorage(storage *store.Store, session string) error {
	err := storage.DeleteSession(session)
	if err != nil {
		return fmt.Errorf("delete session storage: %w", err)
	}

	return nil
}

func (a *App) DeleteSession(serverID, session string) error {
	session = strings.TrimSpace(session)
	// Only the server that owns a live session may kill it. A foreign
	// namespace record must never reach the current server's same-named
	// session.
	if a.isCurrentServer(serverID) && a.tmux.SessionExists(session) {
		err := a.tmux.KillSession(session)
		if err != nil {
			return fmt.Errorf("kill session: %w", err)
		}
	}

	storage, err := a.targetStore(serverID)
	if err != nil {
		return fmt.Errorf("resolve session server: %w", err)
	}

	err = storage.DeleteSession(session)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}

	return nil
}

func (a *App) RenameWindow(
	serverID, session string,
	windowIndex int,
	name string,
) error {
	if strings.TrimSpace(name) == "" {
		return errWindowNameEmpty
	}

	storage, err := a.targetStore(serverID)
	if err != nil {
		return fmt.Errorf("resolve session server: %w", err)
	}

	if a.isCurrentServer(serverID) && a.tmux.SessionExists(session) {
		err := a.tmux.RenameWindow(session, windowIndex, name)
		if err != nil {
			return fmt.Errorf("rename window: %w", err)
		}
	}

	snap, err := storage.LoadSession(session)
	if err != nil {
		return fmt.Errorf("load session: %w", err)
	}

	updated := false

	for i := range snap.Windows {
		if snap.Windows[i].Index == windowIndex {
			snap.Windows[i].Name = name
			updated = true

			break
		}
	}

	if !updated {
		return errWindowNotInSnapshot
	}

	err = storage.SaveSession(snap)
	if err != nil {
		return fmt.Errorf("save session: %w", err)
	}

	return nil
}

func (a *App) RenameSession(serverID, session, name string) error {
	if strings.TrimSpace(name) == "" {
		return errSessionNameEmpty
	}

	if strings.TrimSpace(session) == "" {
		return errSourceSessionEmpty
	}

	if session == name {
		return nil
	}

	storage, err := a.targetStore(serverID)
	if err != nil {
		return fmt.Errorf("resolve session server: %w", err)
	}

	err = ensureRenameTargetFree(storage, session, name)
	if err != nil {
		return err
	}

	snap, err := storage.LoadSession(session)
	if err != nil {
		return fmt.Errorf("load session: %w", err)
	}

	snap.SessionName = name

	if a.isCurrentServer(serverID) && a.tmux.SessionExists(session) {
		err = a.tmux.RenameSession(session, name)
		if err != nil {
			return fmt.Errorf("rename tmux session: %w", err)
		}
	}

	err = storage.SaveSession(snap)
	if err != nil {
		return fmt.Errorf("save session: %w", err)
	}

	err = storage.DeleteSession(session)
	if err != nil {
		return fmt.Errorf("delete old session: %w", err)
	}

	return nil
}

func ensureRenameTargetFree(storage *store.Store, session, name string) error {
	srcPath, err := storage.SessionPath(session)
	if err != nil {
		return fmt.Errorf("get source session path: %w", err)
	}

	dstPath, err := storage.SessionPath(name)
	if err != nil {
		return fmt.Errorf("get dest session path: %w", err)
	}

	if srcPath == dstPath {
		return nil
	}

	exists, err := storage.SessionExists(name)
	if err != nil {
		return fmt.Errorf("check session exists: %w", err)
	}

	if exists {
		return fmt.Errorf("session %q %w", name, errAlreadyExists)
	}

	return nil
}

func (a *App) NewSession(name string) error {
	if strings.TrimSpace(name) == "" {
		return errSessionNameEmpty
	}

	exists, err := a.store.SessionExists(name)
	if err != nil {
		return fmt.Errorf("check session exists: %w", err)
	} else if exists {
		return fmt.Errorf("session %q %w", name, errAlreadyInStorage)
	}

	err = a.tmux.NewSession(name)
	if err != nil {
		return fmt.Errorf("create new session: %w", err)
	}

	snap, err := a.tmux.CaptureSession(name)
	if err != nil {
		_ = a.tmux.KillSession(name)

		return fmt.Errorf("capture session: %w", err)
	}

	err = a.store.SaveSession(snap)
	if err != nil {
		_ = a.tmux.KillSession(name)

		return fmt.Errorf("save session: %w", err)
	}

	return nil
}

func (a *App) NewWindow(serverID, session, name string) error {
	if strings.TrimSpace(session) == "" {
		return errSessionNameEmpty
	}

	storage, err := a.targetStore(serverID)
	if err != nil {
		return fmt.Errorf("resolve session server: %w", err)
	}

	if a.isCurrentServer(serverID) && a.tmux.SessionExists(session) {
		err := a.tmux.NewWindow(session, name)
		if err != nil {
			return fmt.Errorf("create new window: %w", err)
		}

		saveErr := a.saveLiveSession(storage, session)
		if saveErr != nil {
			return saveErr
		}

		return nil
	}

	snap, err := storage.LoadSession(session)
	if err != nil {
		return fmt.Errorf("load session: %w", err)
	}

	idx := nextWindowIndex(snap.Windows)
	if strings.TrimSpace(name) == "" {
		name = fmt.Sprintf("window-%d", idx)
	}

	snap.Windows = append(snap.Windows, snapshot.Window{
		Index:      idx,
		Name:       name,
		Layout:     "",
		IsActive:   false,
		ActivePane: 0,
		Panes: []snapshot.Pane{
			{
				Index:       0,
				IsActive:    false,
				CurrentPath: "",
				CurrentCmd:  "",
				RestoreCmd:  "",
				Scrollback:  nil,
				Agent:       nil,
				Meta:        nil,
			},
		},
	})

	err = storage.SaveSession(snap)
	if err != nil {
		return fmt.Errorf("save session: %w", err)
	}

	return nil
}

func nextWindowIndex(windows []snapshot.Window) int {
	maxIdx := -1
	for _, w := range windows {
		if w.Index > maxIdx {
			maxIdx = w.Index
		}
	}

	return maxIdx + 1
}

func (a *App) Wakeup(serverID, session string) error {
	if strings.TrimSpace(session) == "" {
		return errSessionNameEmpty
	}

	session = strings.TrimSpace(session)

	// Live state belongs to the current server only: a same-named session on
	// another server never makes this snapshot "awake" here.
	if a.tmux.SessionExists(session) {
		return fmt.Errorf("session %q %w", session, errAlreadyAwake)
	}

	target := PickerTarget{SessionName: session, ServerID: strings.TrimSpace(serverID)}

	return a.RestoreTarget(target, false)
}

func (a *App) Sleep(serverID, session string) error {
	if strings.TrimSpace(session) == "" {
		return errSessionNameEmpty
	}

	session = strings.TrimSpace(session)

	if !a.isCurrentServer(serverID) {
		return fmt.Errorf("session %q %w", session, errNotRunning)
	}

	if !a.tmux.SessionExists(session) {
		return fmt.Errorf("session %q %w", session, errNotRunning)
	}
	err := a.SaveSession(session)
	if err != nil {
		return err
	}
	err = a.tmux.KillSession(session)
	if err != nil {
		return fmt.Errorf("kill session: %w", err)
	}

	return nil
}
