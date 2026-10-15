package picker

import (
	"errors"

	"github.com/alchemmist/lazy-tmux/internal/snapshot"
)

var errTUIDisabled = errors.New("TUI picker disabled in fzf-only build")

type Target struct {
	SessionName string
	WindowIndex *int
	// ServerID identifies the tmux server namespace the record belongs to.
	// Empty means the command's current server.
	ServerID string
}

type QuickSession struct {
	Name     string
	ServerID string
	Label    string
	Foreign  bool
	Restored bool
	Current  bool
	Working  bool
}

type Session struct {
	Record   snapshot.Record
	Windows  []snapshot.Window
	Restored bool
	// ServerID is the namespace id of Record.ServerSocket.
	ServerID string
	// Foreign marks records owned by another tmux server (or legacy data).
	// They can be browsed and restored but never count as live here.
	Foreign bool
	// Label is the short socket marker shown next to foreign sessions.
	Label string

	Statuses map[int]WindowStatus
}

type WindowStatus int

const (
	StatusNone WindowStatus = iota
	StatusWorking
	StatusAwaitingDecision
	StatusAwaitingInput
	StatusIdle
	StatusError
	StatusNeedsSetup
	StatusRestorePending
)

type Actions struct {
	DeleteWindow  func(serverID, session string, windowIndex int) error
	DeleteSession func(serverID, session string) error
	RenameWindow  func(serverID, session string, windowIndex int, name string) error
	RenameSession func(serverID, session, name string) error
	NewSession    func(name string) error
	NewWindow     func(serverID, session, name string) error
	Reload        func() ([]Session, error)
	Wakeup        func(serverID, session string) error
	Sleep         func(serverID, session string) error
	SetTheme      func(theme string) error
}
