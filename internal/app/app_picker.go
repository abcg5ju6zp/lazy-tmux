package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/alchemmist/lazy-tmux/internal/config"
	"github.com/alchemmist/lazy-tmux/internal/integration"
	"github.com/alchemmist/lazy-tmux/internal/integration/agent"
	"github.com/alchemmist/lazy-tmux/internal/picker"
	"github.com/alchemmist/lazy-tmux/internal/snapshot"
	"github.com/alchemmist/lazy-tmux/internal/store"
)

var errNoSavedSessions = errors.New("no saved sessions found")

const quickStatusWorkerLimit = 4

func recordServerID(rec snapshot.Record) string {
	return store.ServerID(rec.ServerSocket)
}

func (a *App) pickerRecords(opts PickerSortOptions) ([]snapshot.Record, error) {
	records, err := a.store.ListRecords()
	if err != nil {
		return nil, fmt.Errorf("list records: %w", err)
	}

	if len(records) == 0 {
		return nil, errNoSavedSessions
	}

	mergeLastAttached(records, a.tmux.SessionsLastAttached(), a.currentServerID())

	picker.SortSessionRecords(records, opts.Session)

	return records, nil
}

func mergeLastAttached(
	records []snapshot.Record,
	attached map[string]time.Time,
	currentServerID string,
) {
	if len(attached) == 0 {
		return
	}

	for index := range records {
		if recordServerID(records[index]) != currentServerID {
			continue
		}

		when, ok := attached[records[index].SessionName]
		if ok && when.After(records[index].LastAccessed) {
			records[index].LastAccessed = when
		}
	}
}

func (a *App) pickerSessions(opts PickerSortOptions) ([]picker.Session, error) {
	records, err := a.pickerRecords(opts)
	if err != nil {
		return nil, err
	}

	liveSessions, err := a.tmux.ListSessions()
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}

	live := make(map[string]struct{}, len(liveSessions))
	for _, name := range liveSessions {
		live[name] = struct{}{}
	}

	currentID := a.currentServerID()
	sessions := make([]picker.Session, 0, len(records))
	statusRegistry := a.integrations.Scope()

	for _, rec := range records {
		serverID := recordServerID(rec)
		foreign := serverID != currentID

		snap, err := a.pickerSnapshot(serverID, rec)
		if err != nil {
			log.Printf("picker: skip session %s: %v", rec.SessionName, err)

			continue
		}

		_, liveHere := live[rec.SessionName]
		restored := !foreign && liveHere

		session := picker.Session{
			Record:   rec,
			Windows:  snap.Windows,
			Restored: restored,
			ServerID: serverID,
			Foreign:  foreign,
			Label:    store.ServerLabel(rec.ServerSocket),
		}

		if restored {
			session.Statuses = windowStatuses(statusRegistry, snap.Windows)
		}

		sessions = append(sessions, session)
	}

	return sessions, nil
}

func (a *App) pickerSnapshot(
	serverID string,
	record snapshot.Record,
) (snapshot.SessionSnapshot, error) {
	cacheKey := serverID + "/" + record.SessionName

	a.pickerCacheMu.Lock()
	cached, ok := a.pickerCache[cacheKey]
	if ok && cached.file == record.File && cached.capturedAt.Equal(record.CapturedAt) {
		a.pickerCacheMu.Unlock()

		return cached.snapshot, nil
	}
	key := pickerCacheKey{
		serverID:    serverID,
		sessionName: record.SessionName,
		file:        record.File,
		capturedAt:  record.CapturedAt,
	}
	if load, loading := a.pickerLoads[key]; loading {
		a.pickerCacheMu.Unlock()
		<-load.done

		return load.snapshot, load.err
	}
	load := &pickerSnapshotLoad{
		done:     make(chan struct{}),
		snapshot: snapshot.SessionSnapshot{},
		err:      nil,
	}
	a.pickerLoads[key] = load
	a.pickerCacheMisses++
	a.pickerCacheMu.Unlock()

	snap, err := a.loadPickerSnapshotRecord(serverID, record)

	a.pickerCacheMu.Lock()
	if err == nil {
		a.pickerCache[cacheKey] = cachedPickerSnapshot{
			capturedAt: record.CapturedAt,
			file:       record.File,
			snapshot:   snap,
		}
	}
	load.snapshot = snap
	load.err = err
	delete(a.pickerLoads, key)
	close(load.done)
	a.pickerCacheMu.Unlock()

	return load.snapshot, load.err
}

func (a *App) loadPickerSnapshotRecord(
	serverID string,
	record snapshot.Record,
) (snapshot.SessionSnapshot, error) {
	storage, err := a.targetStore(serverID)
	if err != nil {
		return snapshot.SessionSnapshot{}, fmt.Errorf("resolve session server: %w", err)
	}

	snap, err := storage.LoadSessionMetadata(record.SessionName)
	if err != nil {
		return snapshot.SessionSnapshot{}, fmt.Errorf("load picker snapshot: %w", err)
	}

	return snap, nil
}

func windowStatuses(
	registry *integration.Registry,
	windows []snapshot.Window,
) map[int]picker.WindowStatus {
	statuses := make(map[int]picker.WindowStatus)

	for _, window := range windows {
		for paneIdx := range window.Panes {
			pane := window.Panes[paneIdx]
			if pane.Meta[agent.PendingKey] != "" || pane.Meta["antex.restore_pending"] != "" {
				statuses[window.Index] = picker.StatusRestorePending

				break
			}
			status, ok := registry.Status(pane)
			if !ok {
				if kind := agent.PaneKind(pane); kind != "" {
					if _, verified := agent.Session(pane, kind); !verified {
						statuses[window.Index] = picker.StatusNeedsSetup
					}
				}

				continue
			}

			statuses[window.Index] = toPickerStatus(status)

			break
		}
	}

	return statuses
}

func toPickerStatus(status integration.Status) picker.WindowStatus {
	switch status {
	case integration.StatusWorking:
		return picker.StatusWorking
	case integration.StatusAwaitingDecision:
		return picker.StatusAwaitingDecision
	case integration.StatusAwaitingInput:
		return picker.StatusAwaitingInput
	case integration.StatusIdle:
		return picker.StatusIdle
	case integration.StatusError:
		return picker.StatusError
	default:
		return picker.StatusNone
	}
}

func (a *App) RestoreTargetAnimated(target PickerTarget) (bool, error) {
	preExisted := a.tmux.SessionExists(strings.TrimSpace(target.SessionName))

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan struct{})

	var restoreErr error

	go func() {
		restoreErr = a.restoreSessionForTarget(ctx, target)
		close(done)
	}()

	cancelled, animErr := picker.RunRestoreAnimationWithTheme(target.SessionName, done, a.cfg.Theme)
	if cancelled {
		cancel()
		<-done

		if !preExisted {
			a.killPartialRestore(target.SessionName)
		}

		return true, nil
	}

	<-done

	if restoreErr != nil {
		return false, restoreErr
	}

	if animErr != nil {
		log.Printf("lazy-tmux: restore animation: %v", animErr)
	}

	return false, a.handoffToTarget(target, true, !preExisted)
}

func (a *App) killPartialRestore(session string) {
	name := strings.TrimSpace(session)
	if name == "" || !a.tmux.SessionExists(name) {
		return
	}

	err := a.tmux.KillSession(name)
	if err != nil {
		log.Printf("lazy-tmux: cancel restore: kill session %s: %v", name, err)
	}
}

func (a *App) RestoreTargetInteractive(target PickerTarget) error {
	preExisted := a.tmux.SessionExists(strings.TrimSpace(target.SessionName))
	err := a.restoreSessionForTarget(context.Background(), target)
	if err != nil {
		return err
	}

	return a.handoffToTarget(target, true, !preExisted)
}

func (a *App) SelectTargetWithTUI() (PickerTarget, error) {
	return a.SelectTargetWithTUISorted(DefaultPickerSortOptions())
}

func (a *App) SelectTargetWithTUISorted(opts PickerSortOptions) (PickerTarget, error) {
	actions := picker.Actions{
		DeleteWindow:  a.DeleteWindow,
		DeleteSession: a.DeleteSession,
		RenameWindow:  a.RenameWindow,
		RenameSession: a.RenameSession,
		NewSession:    a.NewSession,
		NewWindow:     a.NewWindow,
		Wakeup:        a.Wakeup,
		Sleep:         a.Sleep,
		SetTheme: func(theme string) error {
			return config.SetTheme(config.DefaultConfigPath(), theme)
		},
		Reload: func() ([]picker.Session, error) {
			sessions, err := a.pickerSessions(opts)
			if err != nil {
				if errors.Is(err, errNoSavedSessions) {
					return []picker.Session{}, nil
				}

				return nil, err
			}

			return sessions, nil
		},
	}

	target, err := picker.ChooseTargetWithLoader(opts.Window, actions, a.cfg.Theme)
	if err != nil {
		return picker.Target{}, fmt.Errorf("choose target: %w", err)
	}

	return target, nil
}

func (a *App) SelectWithTUI() (string, error) {
	target, err := a.SelectTargetWithTUI()
	if err != nil {
		return "", err
	}

	return target.SessionName, nil
}

func (a *App) SelectQuickSessionWithTUI(initialOffset int) (picker.QuickSession, error) {
	sessions, err := a.quickPickerSessions()
	if err != nil {
		return picker.QuickSession{}, err
	}

	session, err := picker.ChooseQuickSession(
		sessions,
		a.cfg.Theme,
		a.cfg.SessionPicker.NavigationModifiers,
		initialOffset,
		func() map[string]picker.QuickSession { return a.quickWorkingSessions(sessions) },
	)
	if err != nil {
		return picker.QuickSession{}, fmt.Errorf("choose quick session: %w", err)
	}

	return session, nil
}

func (a *App) quickPickerSessions() ([]picker.QuickSession, error) {
	records, err := a.store.ListRecords()
	if err != nil {
		return nil, fmt.Errorf("list records: %w", err)
	}

	liveSessions, err := a.tmux.ListSessions()
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}

	currentID := a.currentServerID()
	attached := a.tmux.SessionsLastAttached()
	mergeLastAttached(records, attached, currentID)

	byKey := make(map[string]struct{}, len(records)+len(liveSessions))
	live := make(map[string]struct{}, len(liveSessions))
	for index := range records {
		byKey[recordServerID(records[index])+"/"+records[index].SessionName] = struct{}{}
	}
	for _, name := range liveSessions {
		live[name] = struct{}{}
		if _, ok := byKey[currentID+"/"+name]; ok {
			continue
		}

		records = append(records, snapshot.Record{
			SessionName:  name,
			ServerSocket: a.serverSocket,
			File:         "",
			CapturedAt:   time.Time{},
			LastAccessed: attached[name],
			Windows:      0,
			Panes:        0,
		})
		byKey[currentID+"/"+name] = struct{}{}
	}
	sortQuickSessionRecords(records, currentID, live)

	current, _ := a.tmux.CurrentSession()
	sessions := make([]picker.QuickSession, 0, len(records))
	for _, record := range records {
		serverID := recordServerID(record)
		foreign := serverID != currentID
		_, liveHere := live[record.SessionName]
		restored := !foreign && liveHere
		sessions = append(sessions, picker.QuickSession{
			Name:     record.SessionName,
			ServerID: serverID,
			Label:    store.ServerLabel(record.ServerSocket),
			Foreign:  foreign,
			Restored: restored,
			Current:  !foreign && record.SessionName == current,
			Working:  false,
		})
	}

	return sessions, nil
}

func (a *App) quickWorkingSessions(
	sessions []picker.QuickSession,
) map[string]picker.QuickSession {
	working := make(map[string]picker.QuickSession)
	registry := a.integrations.Scope()
	statuses := parallelMap(
		sessions,
		quickStatusWorkerLimit,
		func(session picker.QuickSession) picker.QuickSession {
			if !session.Restored {
				return picker.QuickSession{}
			}
			if a.sessionHasWorkingAgent(registry, session) {
				return session
			}

			return picker.QuickSession{}
		},
	)
	for index, isWorking := range statuses {
		if isWorking.Name != "" {
			working[isWorking.ServerID+"/"+isWorking.Name] = sessions[index]
		}
	}

	return working
}

func sortQuickSessionRecords(
	records []snapshot.Record,
	currentID string,
	live map[string]struct{},
) {
	sort.Slice(records, func(indexI, indexJ int) bool {
		idI, idJ := recordServerID(records[indexI]), recordServerID(records[indexJ])
		_, liveHereI := live[records[indexI].SessionName]
		_, liveHereJ := live[records[indexJ].SessionName]
		liveI := idI == currentID && liveHereI
		liveJ := idJ == currentID && liveHereJ
		if liveI != liveJ {
			return liveI
		}
		if !records[indexI].LastAccessed.Equal(records[indexJ].LastAccessed) {
			return records[indexI].LastAccessed.After(records[indexJ].LastAccessed)
		}
		if !records[indexI].CapturedAt.Equal(records[indexJ].CapturedAt) {
			return records[indexI].CapturedAt.After(records[indexJ].CapturedAt)
		}
		if idI != idJ {
			return records[indexI].ServerSocket < records[indexJ].ServerSocket
		}

		return records[indexI].SessionName < records[indexJ].SessionName
	})
}

func (a *App) sessionHasWorkingAgent(
	registry *integration.Registry,
	session picker.QuickSession,
) bool {
	if !session.Restored {
		return false
	}

	storage, err := a.targetStore(session.ServerID)
	if err != nil {
		return false
	}

	snap, err := storage.LoadSessionMetadata(session.Name)
	if err != nil {
		return false
	}

	for windowIndex := range snap.Windows {
		for paneIndex := range snap.Windows[windowIndex].Panes {
			status, ok := registry.Status(
				snap.Windows[windowIndex].Panes[paneIndex],
			)
			if ok && status == integration.StatusWorking {
				return true
			}
		}
	}

	return false
}

func (a *App) OpenQuickSession(session picker.QuickSession) error {
	target := PickerTarget{SessionName: session.Name, ServerID: session.ServerID}
	if a.tmux.SessionExists(strings.TrimSpace(session.Name)) {
		err := a.handoffToTarget(target, true, false)
		if err != nil {
			return err
		}

		storage, err := a.targetStore(session.ServerID)
		if err != nil {
			return fmt.Errorf("resolve session server: %w", err)
		}

		err = storage.MarkSessionAccessed(session.Name, time.Now().UTC())
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("mark session accessed: %w", err)
		}

		return nil
	}

	return a.RestoreTargetInteractive(target)
}

func (a *App) SelectWithFZF() (PickerTarget, error) {
	return a.SelectWithFZFSorted(DefaultPickerSortOptions())
}

func (a *App) SelectWithFZFSorted(opts PickerSortOptions) (PickerTarget, error) {
	records, err := a.pickerRecords(opts)
	if err != nil {
		return PickerTarget{}, err
	}

	target, err := picker.ChooseSessionFZF(records)
	if err != nil {
		return PickerTarget{}, fmt.Errorf("choose session fzf: %w", err)
	}

	return target, nil
}

func (a *App) SelectTargetWithFZFSorted(opts PickerSortOptions) (PickerTarget, error) {
	sessions, err := a.pickerSessions(opts)
	if err != nil {
		return PickerTarget{}, err
	}

	target, err := picker.ChooseWindowFZF(sessions, opts.Window)
	if err != nil {
		return PickerTarget{}, fmt.Errorf("choose window fzf: %w", err)
	}

	return target, nil
}
