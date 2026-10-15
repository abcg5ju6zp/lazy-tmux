package tmux

import (
	"context"
	"crypto/rand"
	"slices"
	"sort"
	"time"

	"github.com/alchemmist/lazy-tmux/internal/snapshot"
)

// RestoreStatus is the lifecycle state of a restore generation.
type RestoreStatus string

const (
	// RestoreStatusRunning means the generation is restoring (or waiting to resume).
	RestoreStatusRunning RestoreStatus = "running"
	// RestoreStatusCanceling means cancellation was requested and cleanup is in progress.
	RestoreStatusCanceling RestoreStatus = "canceling"
	// RestoreStatusCleaning means cleanup could not finish; recorded resources are residue
	// that the next restore or a process restart must converge.
	RestoreStatusCleaning RestoreStatus = "cleaning"
)

// commandConfirmed marks a non-agent pane command as already sent in WindowJournal.Commands.
const commandConfirmed = "1"

// WindowJournal records every confirmed creation for one window of a generation.
// A step is recorded only after its tmux mutation succeeded, so retries and
// process restarts never create a second copy of the same window or pane.
type WindowJournal struct {
	// Created is true once the window itself was confirmed (created or adopted).
	Created bool `json:"created"`
	// Panes is the number of panes confirmed for this window.
	Panes int `json:"panes"`
	// Scrollback lists pane indexes whose scrollback replay was confirmed.
	Scrollback []int `json:"scrollback,omitempty"`
	// Commands maps pane index to the staged agent token (or commandConfirmed)
	// once its restore command was confirmed as sent.
	Commands map[int]string `json:"commands,omitempty"`
	// Layout is true once the saved layout was applied.
	Layout bool `json:"layout,omitempty"`
}

// RestoreJournal is the stable generation record tying snapshot load, tmux
// creation, agent pending tokens, command confirmation, scrollback replay,
// access time and client handoff together. It is persisted after each
// confirmed step so that cancellation, kill failures and process restarts
// converge to a single conclusion.
type RestoreJournal struct {
	// Generation is the stable identifier shared by every attempt of one restore.
	Generation string `json:"generation"`
	// SessionName is the tmux session being restored.
	SessionName string `json:"session_name"`
	// PreExisted records whether the session already existed before this
	// generation started. It never changes on resume or restart.
	PreExisted bool `json:"pre_existed"`
	// Status tracks running / canceling / cleaning convergence.
	Status RestoreStatus `json:"status"`

	// SessionCreated is confirmed once new-session succeeded for this generation.
	SessionCreated bool `json:"session_created"`
	// ExistsAdopted is set when a live foreign session was surfaced as
	// ErrSessionExists: the generation created no resources and must never
	// kill that session.
	ExistsAdopted bool `json:"exists_adopted,omitempty"`

	// Windows records confirmed creations keyed by window index.
	Windows map[int]*WindowJournal `json:"windows,omitempty"`

	// Accessed is confirmed after the snapshot access time was updated.
	Accessed bool `json:"accessed,omitempty"`
	// HandedOff is confirmed after the client switch/attach finished.
	HandedOff bool `json:"handed_off,omitempty"`

	// LastError records the most recent convergence failure, if any.
	LastError string `json:"last_error,omitempty"`
	// CreatedAt / UpdatedAt track the generation for diagnostics.
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`

	// Adopted is true when this journal was loaded from an earlier attempt or
	// process. It tells the engine to treat live tmux resources as partial
	// generation state instead of returning ErrSessionExists. It is not
	// persisted because it is re-established on every load.
	Adopted bool `json:"-"`
}

// NewRestoreJournal starts a fresh generation for one session.
func NewRestoreJournal(sessionName string, preExisted bool) *RestoreJournal {
	now := time.Now().UTC()

	return &RestoreJournal{
		Generation:     rand.Text(),
		SessionName:    sessionName,
		PreExisted:     preExisted,
		Status:         RestoreStatusRunning,
		SessionCreated: false,
		ExistsAdopted:  false,
		Windows:        make(map[int]*WindowJournal),
		Accessed:       false,
		HandedOff:      false,
		LastError:      "",
		CreatedAt:      now,
		UpdatedAt:      now,
		Adopted:        false,
	}
}

// HasCreatedResources reports whether any tmux resource was confirmed.
func (journal *RestoreJournal) HasCreatedResources() bool {
	if journal.SessionCreated {
		return true
	}
	for _, entry := range journal.Windows {
		if entry.Created {
			return true
		}
	}

	return false
}

func (journal *RestoreJournal) window(index int) *WindowJournal {
	entry := journal.Windows[index]
	if entry == nil {
		entry = &WindowJournal{
			Created:    false,
			Panes:      0,
			Scrollback: nil,
			Commands:   make(map[int]string),
			Layout:     false,
		}
		journal.Windows[index] = entry
	}
	if entry.Commands == nil {
		entry.Commands = make(map[int]string)
	}

	return entry
}

func (journal *RestoreJournal) touch() {
	journal.UpdatedAt = time.Now().UTC()
}

// createdWindowIndexes returns confirmed window indexes in descending order,
// which is the safe kill order when removing windows by index.
func (journal *RestoreJournal) createdWindowIndexes() []int {
	indexes := make([]int, 0, len(journal.Windows))
	for index, entry := range journal.Windows {
		if entry.Created {
			indexes = append(indexes, index)
		}
	}
	slices.SortFunc(indexes, func(a, b int) int { return b - a })

	return indexes
}

// ownsWholeSession reports whether this generation created the session itself,
// in which case killing the session removes every generation resource at once.
// A session that pre-existed is never owned wholesale.
func (journal *RestoreJournal) ownsWholeSession() bool {
	return journal.SessionCreated && !journal.PreExisted
}

// CleanupRestoreJournal removes the tmux resources confirmed for a canceled
// generation. A generation that created the session owns it and kills the
// whole session; a generation running against a pre-existing session only
// removes the windows it added and never kills the session itself.
//
// commit is invoked after each successful removal so that a failure or a
// process death leaves an accurate, resumable residue record.
func (client *Client) CleanupRestoreJournal(
	journal *RestoreJournal,
	commit RestoreJournalCommit,
) error {
	name := journal.SessionName

	if !client.SessionExists(name) {
		return nil
	}

	if journal.ownsWholeSession() {
		err := client.KillSession(name)
		if err != nil {
			return err
		}

		journal.SessionCreated = false
		journal.Windows = make(map[int]*WindowJournal)

		return client.commitJournal(journal, commit)
	}

	var err error

	liveWindows := client.liveIndexes("list-windows", sessionTarget(name), "#{window_index}")
	for _, index := range journal.createdWindowIndexes() {
		if !slices.Contains(liveWindows, index) {
			delete(journal.Windows, index)

			continue
		}

		err = client.KillWindow(name, index)
		if err != nil {
			return err
		}

		delete(journal.Windows, index)
		err = client.commitJournal(journal, commit)
		if err != nil {
			return err
		}
	}

	return nil
}

func (client *Client) commitJournal(journal *RestoreJournal, commit RestoreJournalCommit) error {
	if commit == nil {
		return nil
	}

	journal.touch()

	return commit()
}

// finalizeRestoredSession applies focus, waits for restored commands and
// completes pending agent acknowledgments. It only mutates confirmation state
// (acks), so rerunning it after a restart simply finishes what was pending.
func (client *Client) finalizeRestoredSession(
	ctx context.Context,
	sessionSnapshot snapshot.SessionSnapshot,
	commit RestoreJournalCommit,
) error {
	sessionName := sessionSnapshot.SessionName
	windows := allWindowSlice(sessionSnapshot)

	client.selectRestoredFocus(sessionSnapshot, windows)

	client.waitForRestoredCommands(ctx, sessionName, windows)

	if commit != nil && ctx.Err() == nil {
		// Complete pending agent receipts that were staged by an earlier
		// attempt of this generation but never acknowledged.
		client.reconcileRestoreAcks(ctx, sessionName)
	}

	return nil
}

// allWindowSlice returns the snapshot windows sorted by index.
func allWindowSlice(sessionSnapshot snapshot.SessionSnapshot) []snapshot.Window {
	windows := make([]snapshot.Window, len(sessionSnapshot.Windows))
	copy(windows, sessionSnapshot.Windows)
	sort.Slice(windows, func(i, j int) bool { return windows[i].Index < windows[j].Index })

	return windows
}

// ensureRestoreSession makes sure the session and its first window exist,
// adopting a live session from an earlier attempt or recreating one that
// disappeared. It returns the windows that still need per-window processing.
func (client *Client) ensureRestoreSession(
	ctx context.Context,
	sessionName string,
	windows []snapshot.Window,
	journal *RestoreJournal,
	commit RestoreJournalCommit,
) ([]snapshot.Window, error) {
	if journal.SessionCreated && journal.Adopted && !client.SessionExists(sessionName) {
		// The confirmed session disappeared (external cleanup): recreate it and
		// replay every stage instead of failing the resumed generation.
		journal.SessionCreated = false
		journal.Windows = make(map[int]*WindowJournal)
	}

	if journal.SessionCreated {
		return windows, nil
	}

	if client.SessionExists(sessionName) {
		if !journal.Adopted {
			return nil, ErrSessionExists
		}

		// A restarted generation: treat the live session as partial state and
		// reconcile every window/pane against the journal.
		journal.SessionCreated = true
		err := client.commitJournal(journal, commit)
		if err != nil {
			return nil, err
		}

		return windows, nil
	}

	err := client.createSessionWithFirstWindow(sessionName, windows[0])
	if err != nil {
		return nil, err
	}

	journal.SessionCreated = true

	first := journal.window(windows[0].Index)
	first.Created = true
	first.Panes = 1

	err = client.commitJournal(journal, commit)
	if err != nil {
		return nil, err
	}

	// The first window and its shell pane exist as soon as new-session
	// succeeds; populate it even when the restore was already canceled so the
	// confirmed session is never left half initialized.
	err = client.ensureWindowContent(ctx, sessionName, windows[0], journal, commit)
	if err != nil {
		return nil, err
	}

	return windows[1:], nil
}
