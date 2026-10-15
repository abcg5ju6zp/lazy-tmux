package tmux

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/alchemmist/lazy-tmux/internal/integration/agent"
	"github.com/alchemmist/lazy-tmux/internal/snapshot"
	"github.com/charmbracelet/x/term"
)

var (
	errEmptySessionName     = errors.New("empty session name")
	errSnapshotHasNoWindows = errors.New("session snapshot has no windows")
	errEmptyTTYPath         = errors.New("empty tty path")
	errUnsupportedTTYPath   = errors.New("unsupported tty path")
	errTTYNotCharDevice     = errors.New("tty path is not a character device")
	errNoWindowsAfterCreate = errors.New("no windows found after session creation")
	errInvalidPaneResponse  = errors.New("invalid pane response")
)

const fieldSep = "|"

const (
	defaultRestoreSettleTimeout = 5 * time.Second
	restoreSettlePollInterval   = 100 * time.Millisecond
)

func splitFieldsN(line string, n int) []string {
	return strings.SplitN(line, fieldSep, n)
}

var (
	ErrSessionNotFound = errors.New("tmux session not found")
	ErrSessionExists   = errors.New("tmux session already exists")
)

//nolint:gochecknoglobals // test seam: tests capture pane-tty writes
var paneTTYWriter = writePaneTTY

type commandResult struct {
	stdout string
	err    error
}

type commandRunner interface {
	runCommand(args ...string) commandResult
}

type execRunner struct{}

func (execRunner) runCommand(args ...string) commandResult {
	// #nosec G204 -- argv[0] is the tmux binary, user-configured on purpose (--tmux-bin)
	cmd := exec.CommandContext(context.Background(), args[0], args[1:]...)

	out, err := cmd.CombinedOutput()
	if err != nil {
		return commandResult{"", fmt.Errorf(
			"tmux %s: %w (%s)",
			strings.Join(args[1:], " "),
			err,
			strings.TrimSpace(string(out)),
		)}
	}

	return commandResult{string(out), nil}
}

type Client struct {
	bin           string
	runner        commandRunner
	settleTimeout time.Duration

	allowlist    []*regexp.Regexp
	allowlistSet bool

	denylist []*regexp.Regexp

	resolver RestoreCommandResolver
}

type RestoreCommandResolver interface {
	Resolve(pane snapshot.Pane) string
}

func NewClient(bin string) *Client {
	if strings.TrimSpace(bin) == "" {
		bin = "tmux"
	}

	return &Client{bin: bin, runner: execRunner{}, settleTimeout: defaultRestoreSettleTimeout}
}

func NewClientWithRunner(bin string, cmdRunner commandRunner) *Client {
	if strings.TrimSpace(bin) == "" {
		bin = "tmux"
	}

	tmuxClient := &Client{bin: bin, settleTimeout: defaultRestoreSettleTimeout}
	if cmdRunner != nil {
		tmuxClient.runner = cmdRunner
	} else {
		tmuxClient.runner = execRunner{}
	}

	return tmuxClient
}

func (client *Client) SetRestoreTimeout(timeout time.Duration) {
	client.settleTimeout = timeout
}

func (client *Client) SetRestoreAllowlist(list []string) {
	if list == nil {
		client.allowlistSet = false
		client.allowlist = nil

		return
	}

	client.allowlistSet = true
	client.allowlist = compileCommandPatterns(list)
}

func (client *Client) SetRestoreDenylist(list []string) {
	if len(list) == 0 {
		client.denylist = nil

		return
	}

	client.denylist = compileCommandPatterns(list)
}

func compileCommandPatterns(list []string) []*regexp.Regexp {
	patterns := make([]*regexp.Regexp, 0, len(list))

	for _, item := range list {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}

		re, err := regexp.Compile("^(?:" + item + ")$")
		if err != nil {
			continue
		}

		patterns = append(patterns, re)
	}

	return patterns
}

func (client *Client) SetRestoreResolver(resolver RestoreCommandResolver) {
	client.resolver = resolver
}

func sessionTarget(name string) string {
	name = strings.TrimSpace(name)
	if strings.HasPrefix(name, "=") {
		return name
	}

	return "=" + name
}

func sessionWindowTarget(name string, windowIndex int) string {
	return fmt.Sprintf("%s:%d", sessionTarget(name), windowIndex)
}

func sessionPaneTarget(name string, windowIndex, paneIndex int) string {
	return fmt.Sprintf("%s:%d.%d", sessionTarget(name), windowIndex, paneIndex)
}

func PaneTarget(name string, windowIndex, paneIndex int) string {
	return sessionPaneTarget(name, windowIndex, paneIndex)
}

func sessionWindowBaseTarget(name string) string {
	return sessionTarget(name) + ":"
}

func (client *Client) Run(args ...string) error {
	// #nosec G204 -- client.bin is the tmux binary, user-configured on purpose (--tmux-bin)
	cmd := exec.CommandContext(context.Background(), client.bin, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	err := cmd.Run()
	if err != nil {
		return fmt.Errorf("run tmux: %w", err)
	}

	return nil
}

func (client *Client) Output(args ...string) (string, error) {
	allArgs := append([]string{client.bin}, args...)
	res := client.runner.runCommand(allArgs...)

	return res.stdout, res.err
}

func (client *Client) SessionExists(name string) bool {
	_, err := client.Output("has-session", "-t", sessionTarget(name))

	return err == nil
}

func (client *Client) ListSessions() ([]string, error) {
	out, err := client.Output("list-sessions", "-F", "#{session_name}")
	if err != nil {
		if isNoServerError(err) {
			return nil, nil
		}

		return nil, err
	}

	lines := splitLines(out)
	sort.Strings(lines)

	return lines, nil
}

func isNoServerError(err error) bool {
	if err == nil {
		return false
	}

	msg := strings.ToLower(err.Error())

	switch {
	case strings.Contains(msg, "no server running"):
		return true
	case strings.Contains(msg, "error connecting to") &&
		strings.Contains(msg, "no such file or directory"):
		return true
	default:
		return false
	}
}

func (client *Client) SessionsLastAttached() map[string]time.Time {
	out, err := client.Output(
		"list-sessions",
		"-F",
		"#{session_last_attached}"+fieldSep+"#{session_name}",
	)
	if err != nil {
		return nil
	}

	result := make(map[string]time.Time)

	for _, line := range splitLines(out) {
		fields := splitFieldsN(line, 2)
		if len(fields) != 2 {
			continue
		}

		name := fields[1]
		if strings.TrimSpace(name) == "" {
			continue
		}

		secs, parseErr := strconv.ParseInt(strings.TrimSpace(fields[0]), 10, 64)
		if parseErr != nil || secs <= 0 {
			continue
		}

		result[name] = time.Unix(secs, 0).UTC()
	}

	return result
}

func (client *Client) CurrentSession() (string, error) {
	out, err := client.Output("display-message", "-p", "#S")
	if err != nil {
		return "", err
	}

	return strings.TrimSpace(out), nil
}

func (client *Client) SocketPath() string {
	out, err := client.Output("display-message", "-p", "#{socket_path}")
	if err != nil {
		return "default"
	}

	v := strings.TrimSpace(out)
	if v == "" {
		return "default"
	}

	return v
}

func (client *Client) SwitchClient(session string) error {
	if os.Getenv("TMUX") == "" {
		return nil
	}

	_, err := client.Output("switch-client", "-t", sessionTarget(session))

	return err
}

func (client *Client) SynchronizeWindowSize(target string) error {
	tmuxTarget := sessionTarget(target)
	_, err := client.Output("resize-window", "-A", "-t", tmuxTarget)
	if err != nil {
		return err
	}

	_, err = client.Output("set-option", "-wu", "-t", tmuxTarget, "window-size")

	return err
}

func (client *Client) InsideTmux() bool {
	return strings.TrimSpace(os.Getenv("TMUX")) != ""
}

//nolint:gochecknoglobals // test seam, see comment above
var attachExec = syscall.Exec

//nolint:gochecknoglobals // test seam, see comment above
var hasControllingTTY = func() bool {
	return term.IsTerminal(os.Stdout.Fd())
}

func (client *Client) AttachSession(target string) error {
	if !hasControllingTTY() {
		return nil
	}

	bin, err := exec.LookPath(client.bin)
	if err != nil {
		return fmt.Errorf("locate tmux %q: %w", client.bin, err)
	}

	args := []string{bin, "attach-session", "-t", sessionTarget(target)}

	return attachExec(bin, args, os.Environ())
}

func (client *Client) KillWindow(session string, windowIndex int) error {
	_, err := client.Output("kill-window", "-t", sessionWindowTarget(session, windowIndex))

	return err
}

func (client *Client) KillSession(session string) error {
	_, err := client.Output("kill-session", "-t", sessionTarget(session))

	return err
}

func (client *Client) RenameWindow(session string, windowIndex int, name string) error {
	_, err := client.Output("rename-window", "-t", sessionWindowTarget(session, windowIndex), name)

	return err
}

func (client *Client) RenameSession(session, name string) error {
	_, err := client.Output("rename-session", "-t", sessionTarget(session), name)

	return err
}

func (client *Client) NewSession(name string) error {
	_, err := client.Output("new-session", "-d", "-s", name)

	return err
}

func (client *Client) NewWindow(session, name string) error {
	args := []string{"new-window", "-d", "-t", sessionWindowBaseTarget(session)}
	if strings.TrimSpace(name) != "" {
		args = append(args, "-n", name)
	}

	_, err := client.Output(args...)

	return err
}

func (client *Client) CaptureSession(name string) (snapshot.SessionSnapshot, error) {
	if !client.SessionExists(name) {
		return snapshot.SessionSnapshot{}, ErrSessionNotFound
	}

	paneOutput, err := client.Output(
		"list-panes",
		"-s",
		"-t",
		sessionTarget(name),
		"-F",
		"#{window_index}"+fieldSep+
			"#{window_layout}"+fieldSep+
			"#{window_active}"+fieldSep+
			"#{window_name}"+fieldSep+
			"#{pane_index}"+fieldSep+
			"#{pane_active}"+fieldSep+
			"#{pane_pid}"+fieldSep+
			"#{pane_tty}"+fieldSep+
			"#{pane_current_command}"+fieldSep+
			"#{pane_current_path}"+fieldSep+
			"#{pane_id}"+fieldSep+
			"#{socket_path}"+fieldSep+
			"#{@lazy_tmux_restore_pending}"+fieldSep+
			"#{?@lazy_tmux_restore_ack,#{@lazy_tmux_restore_ack},#{@antex_restore_ack}}"+fieldSep+
			"#{@lazy_tmux_agent_binding}"+fieldSep+
			"#{@antex_binding}",
	)
	if err != nil {
		return snapshot.SessionSnapshot{}, err
	}

	processes, err := loadProcessSnapshot()
	if err != nil {
		return snapshot.SessionSnapshot{}, err
	}

	windows := parseCapturedPanes(paneOutput, processes)
	client.acknowledgeRestoredPanes(paneOutput, windows)

	sort.Slice(windows, func(i, j int) bool { return windows[i].Index < windows[j].Index })

	currentWin, currentPane := currentWindowPane(windows)

	return snapshot.SessionSnapshot{
		Version:     snapshot.FormatVersion,
		SessionName: name,
		CapturedAt:  nowUTC(),
		CurrentWin:  currentWin,
		CurrentPane: currentPane,
		Windows:     windows,
	}, nil
}

func parseCapturedPanes(output string, processes processSnapshot) []snapshot.Window {
	byIndex := make(map[int]*snapshot.Window)
	for _, line := range splitLines(output) {
		parts := splitFieldsN(line, capturePaneLineFields)
		if len(parts) != capturePaneLineFields {
			continue
		}

		windowIndex, _ := strconv.Atoi(parts[0])
		window, ok := byIndex[windowIndex]
		if !ok {
			window = &snapshot.Window{
				Index:      windowIndex,
				Name:       parts[3],
				Layout:     parts[1],
				IsActive:   parts[2] == "1",
				ActivePane: 0,
				Panes:      nil,
			}
			byIndex[windowIndex] = window
		}

		paneIndex, _ := strconv.Atoi(parts[4])
		panePID, _ := strconv.Atoi(strings.TrimSpace(parts[6]))
		restoreCmd := processes.foregroundCommand(panePID)
		pane := snapshot.Pane{
			Index:       paneIndex,
			CurrentPath: parts[9],
			CurrentCmd:  parts[8],
			RestoreCmd:  strings.TrimSpace(restoreCmd),
			Scrollback:  nil,
			Agent:       nil,
			IsActive:    parts[5] == "1",
			Meta:        nil,
		}
		pane = attachAgentBindings(
			pane,
			parts[14],
			parts[15],
			panePID,
			parts[10],
			parts[11],
			processes,
		)
		pane = preserveRestoreIntent(pane, parts[12], parts[13])
		if pane.IsActive {
			window.ActivePane = pane.Index
		}
		window.Panes = append(window.Panes, pane)
	}

	windows := make([]snapshot.Window, 0, len(byIndex))
	for _, window := range byIndex {
		sort.Slice(window.Panes, func(i, j int) bool {
			return window.Panes[i].Index < window.Panes[j].Index
		})
		windows = append(windows, *window)
	}
	sort.Slice(windows, func(i, j int) bool { return windows[i].Index < windows[j].Index })

	return windows
}

func loadProcessSnapshot() (processSnapshot, error) {
	cmd := exec.CommandContext( // #nosec G204 -- fixed ps binary and format flags
		context.Background(),
		"ps",
		"-ax",
		"-o",
		"pid=",
		"-o",
		"ppid=",
		"-o",
		"stat=",
		"-o",
		"command=",
	)
	out, err := cmd.Output()
	if err != nil {
		return processSnapshot{}, fmt.Errorf("get process list: %w", err)
	}

	return newProcessSnapshot(splitLines(string(out))), nil
}

func currentWindowPane(windows []snapshot.Window) (int, int) {
	for _, window := range windows {
		if window.IsActive {
			return window.Index, window.ActivePane
		}
	}

	if len(windows) > 0 {
		return windows[0].Index, windows[0].ActivePane
	}

	return 0, 0
}

func findWindow(windows []snapshot.Window, index int) (snapshot.Window, bool) {
	for _, window := range windows {
		if window.Index == index {
			return window, true
		}
	}

	return snapshot.Window{}, false
}

func paneExists(panes []snapshot.Pane, index int) bool {
	for _, pane := range panes {
		if pane.Index == index {
			return true
		}
	}

	return false
}

func clampPane(window snapshot.Window, paneIndex int) int {
	if paneExists(window.Panes, paneIndex) {
		return paneIndex
	}

	if paneExists(window.Panes, window.ActivePane) {
		return window.ActivePane
	}

	if len(window.Panes) > 0 {
		return window.Panes[0].Index
	}

	return paneIndex
}

func resolveRestoreFocus(
	sessionSnapshot snapshot.SessionSnapshot,
	windows []snapshot.Window,
) (int, int) {
	if window, ok := findWindow(windows, sessionSnapshot.CurrentWin); ok {
		return window.Index, clampPane(window, sessionSnapshot.CurrentPane)
	}

	win, pane := currentWindowPane(windows)
	if window, ok := findWindow(windows, win); ok {
		return win, clampPane(window, pane)
	}

	return win, pane
}

func (client *Client) RestoreSession(
	ctx context.Context,
	sessionSnapshot snapshot.SessionSnapshot,
) error {
	journal := NewRestoreJournal(sessionSnapshot.SessionName, false)

	return client.RestoreSessionJournaled(ctx, sessionSnapshot, journal, nil)
}

// RestoreJournalCommit persists the generation journal after each confirmed
// step. It must be safe to call repeatedly.
type RestoreJournalCommit func() error

// RestoreSessionJournaled restores a snapshot under a stable generation.
//
// Each mutating step is recorded in the journal only after the corresponding
// tmux command succeeds, so a retry or a process restart adopts the live
// session/windows/panes instead of stacking a second set on top. Steps that
// have not started are skipped once ctx is canceled; a successful mutation is
// still recorded so that generation cleanup can remove it.
func (client *Client) RestoreSessionJournaled(
	ctx context.Context,
	sessionSnapshot snapshot.SessionSnapshot,
	journal *RestoreJournal,
	commit RestoreJournalCommit,
) error {
	if sessionSnapshot.SessionName == "" {
		return errEmptySessionName
	}

	if len(sessionSnapshot.Windows) == 0 {
		return errSnapshotHasNoWindows
	}

	if journal == nil {
		journal = NewRestoreJournal(sessionSnapshot.SessionName, false)
	}
	if journal.Windows == nil {
		journal.Windows = make(map[int]*WindowJournal)
	}

	sessionName := sessionSnapshot.SessionName

	windows := make([]snapshot.Window, len(sessionSnapshot.Windows))
	copy(windows, sessionSnapshot.Windows)
	sort.Slice(windows, func(i, j int) bool { return windows[i].Index < windows[j].Index })

	var (
		pending []snapshot.Window
		err     error
	)

	pending, err = client.ensureRestoreSession(ctx, sessionName, windows, journal, commit)
	if err != nil {
		return err
	}

	for _, window := range pending {
		err = restoreCanceled(ctx)
		if err != nil {
			return err
		}

		err = client.ensureWindow(ctx, sessionName, window, journal, commit)
		if err != nil {
			return err
		}
	}

	err = restoreCanceled(ctx)
	if err != nil {
		return err
	}

	return client.finalizeRestoredSession(ctx, sessionSnapshot, commit)
}

func restoreCanceled(ctx context.Context) error {
	err := ctx.Err()
	if err != nil {
		return fmt.Errorf("restore canceled: %w", err)
	}

	return nil
}

func newSessionArgs(sessionName string, w snapshot.Window) []string {
	args := []string{"new-session", "-d", "-s", sessionName, "-n", w.Name}
	if path := firstPanePath(w); path != "" {
		args = append(args, "-c", path)
	}

	return args
}

func newWindowArgs(sessionName string, win snapshot.Window) []string {
	args := []string{
		"new-window",
		"-d",
		"-t",
		sessionWindowTarget(sessionName, win.Index),
		"-n",
		win.Name,
	}
	if path := firstPanePath(win); path != "" {
		args = append(args, "-c", path)
	}

	return args
}

func (client *Client) CapturePaneScrollback(target string, lines int) (string, error) {
	if lines <= 0 {
		lines = 5000
	}

	return client.Output(
		"capture-pane",
		"-p",
		"-e",
		"-J",
		"-S",
		fmt.Sprintf("-%d", lines),
		"-t",
		target,
	)
}

func (client *Client) CapturePane(target string) (snapshot.Pane, error) {
	args := []string{
		"display-message",
		"-p",
	}
	if strings.TrimSpace(target) != "" {
		args = append(args, "-t", target)
	}
	args = append(
		args,
		"#{pane_current_command}"+fieldSep+
			"#{pane_current_path}"+fieldSep+
			"#{pane_pid}"+fieldSep+
			"#{pane_id}"+fieldSep+
			"#{socket_path}"+fieldSep+
			"#{@lazy_tmux_agent_binding}"+fieldSep+
			"#{@antex_binding}",
	)

	out, err := client.Output(args...)
	if err != nil {
		return snapshot.Pane{}, err
	}
	parts := splitFieldsN(strings.TrimSpace(out), livePaneLineFields)
	if len(parts) != livePaneLineFields {
		return snapshot.Pane{}, fmt.Errorf(
			"%w %q",
			errInvalidPaneResponse,
			strings.TrimSpace(out),
		)
	}

	processes, err := loadProcessSnapshot()
	if err != nil {
		return snapshot.Pane{}, err
	}
	panePID, _ := strconv.Atoi(parts[2])

	pane := snapshot.Pane{
		Index:       0,
		CurrentPath: parts[1],
		CurrentCmd:  parts[0],
		RestoreCmd:  "",
		Scrollback:  nil,
		Agent:       nil,
		IsActive:    true,
		Meta:        nil,
	}
	pane = attachAgentBindings(pane, parts[5], parts[6], panePID, parts[3], parts[4], processes)

	return pane, nil
}

// ensureWindow makes sure one snapshot window exists and is populated,
// adopting a window that tmux created even when its creation confirmation was
// lost (partial command success or process death mid restore).
func (client *Client) ensureWindow(
	ctx context.Context,
	sessionName string,
	win snapshot.Window,
	journal *RestoreJournal,
	commit RestoreJournalCommit,
) error {
	entry := journal.window(win.Index)
	liveWindows := client.liveIndexes("list-windows", sessionTarget(sessionName), "#{window_index}")
	windowLive := slices.Contains(liveWindows, win.Index)

	if windowLive && entry.Created {
		return client.ensureWindowContent(ctx, sessionName, win, journal, commit)
	}

	if !windowLive {
		// This also covers a confirmed window that disappeared: recreating it
		// is what lets the resumed generation converge instead of failing.
		err := client.runWithShellFallback(newWindowArgs(sessionName, win))
		if err != nil {
			return err
		}
	}

	entry.Created = true
	if entry.Panes == 0 {
		entry.Panes = 1
	}

	err := client.commitJournal(journal, commit)
	if err != nil {
		return err
	}

	return client.ensureWindowContent(ctx, sessionName, win, journal, commit)
}

func (client *Client) selectRestoredFocus(
	sessionSnapshot snapshot.SessionSnapshot,
	windows []snapshot.Window,
) {
	win, pane, ok := client.resolveLiveFocus(
		sessionSnapshot.SessionName,
		sessionSnapshot,
		windows,
	)
	if !ok {
		return
	}

	_, _ = client.Output(
		"select-window",
		"-t",
		sessionWindowTarget(sessionSnapshot.SessionName, win),
		";",
		"select-pane",
		"-t",
		sessionPaneTarget(sessionSnapshot.SessionName, win, pane),
	)
}

// createSessionWithFirstWindow runs new-session for the generation and moves
// the initial window to the snapshot index when base-index differs.
func (client *Client) createSessionWithFirstWindow(
	sessionName string,
	first snapshot.Window,
) error {
	err := client.runWithShellFallback(newSessionArgs(sessionName, first))
	if err != nil {
		return err
	}

	createdIdx, err := client.createdFirstWindowIndex(sessionName)
	if err != nil {
		return err
	}

	if createdIdx != first.Index {
		_, err = client.Output(
			"move-window",
			"-s", sessionWindowTarget(sessionName, createdIdx),
			"-t", sessionWindowTarget(sessionName, first.Index),
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// ensureWindowContent reconciles panes, scrollback, commands and layout for one
// window against the generation journal.
func (client *Client) ensureWindowContent(
	ctx context.Context,
	sessionName string,
	window snapshot.Window,
	journal *RestoreJournal,
	commit RestoreJournalCommit,
) error {
	entry := journal.window(window.Index)

	err := client.ensurePaneCount(ctx, sessionName, window, window.Index, journal, commit)
	if err != nil {
		return err
	}

	err = client.restoreWindowScrollback(ctx, sessionName, window, window.Index, journal, commit)
	if err != nil {
		return err
	}

	err = client.restoreWindowCommandsJournaled(
		ctx, sessionName, window, window.Index, journal, commit,
	)
	if err != nil {
		return err
	}

	if window.Layout != "" && !entry.Layout {
		_, _ = client.Output(
			"select-layout",
			"-t",
			sessionWindowTarget(sessionName, window.Index),
			window.Layout,
		)
		entry.Layout = true
		_ = client.commitJournal(journal, commit)
	}

	return nil
}

func (client *Client) ensurePaneCount(
	ctx context.Context,
	sessionName string,
	window snapshot.Window,
	windowIndex int,
	journal *RestoreJournal,
	commit RestoreJournalCommit,
) error {
	entry := journal.window(windowIndex)
	livePanes := client.liveIndexes(
		"list-panes",
		sessionWindowTarget(sessionName, windowIndex),
		"#{pane_index}",
	)

	var err error

	for len(livePanes) < len(window.Panes) {
		pane := window.Panes[len(livePanes)]
		err = restoreCanceled(ctx)
		if err != nil {
			return err
		}

		args := []string{"split-window", "-d", "-t", sessionWindowTarget(sessionName, windowIndex)}

		if pane.CurrentPath != "" {
			args = append(args, "-c", pane.CurrentPath)
		}

		err = client.runWithShellFallback(args)
		if err != nil {
			return err
		}

		livePanes = client.liveIndexes(
			"list-panes",
			sessionWindowTarget(sessionName, windowIndex),
			"#{pane_index}",
		)

		if entry.Panes < len(livePanes) {
			entry.Panes = len(livePanes)
			err = client.commitJournal(journal, commit)
			if err != nil {
				return err
			}
		}
	}

	// Adopt panes tmux created without a recorded confirmation so that they are
	// never split a second time on resume.
	if entry.Panes < len(livePanes) {
		entry.Panes = len(livePanes)
		err = client.commitJournal(journal, commit)
		if err != nil {
			return err
		}
	}

	return nil
}

func firstPanePath(w snapshot.Window) string {
	if len(w.Panes) == 0 {
		return ""
	}

	pane := w.Panes[0]

	path := pane.CurrentPath
	if path == "" {
		home, err := os.UserHomeDir()
		if err == nil {
			path = home
		}
	}

	return filepath.Clean(path)
}

func (client *Client) effectiveRestoreCommand(pane snapshot.Pane) string {
	command, _ := client.checkedRestoreCommand(pane)

	return command
}

func (client *Client) checkedRestoreCommand(pane snapshot.Pane) (string, error) {
	if resolver, ok := client.resolver.(interface {
		ResolveChecked(pane snapshot.Pane) (string, error)
	}); ok {
		command, err := resolver.ResolveChecked(pane)
		if err != nil {
			return "", fmt.Errorf("resolve restore command: %w", err)
		}
		if strings.TrimSpace(command) != "" {
			return command, nil
		}
	}
	if client.resolver != nil {
		if override := strings.TrimSpace(client.resolver.Resolve(pane)); override != "" {
			return override, nil
		}
	}

	return normalizedCommand(pane.RestoreCmd, pane.CurrentCmd), nil
}

func normalizedCommand(restore, current string) string {
	restore = sanitizeCommand(restore)
	if restore != "" && !isShellCommand(restore) {
		return restore
	}

	current = sanitizeCommand(current)
	if current != "" && !isShellCommand(current) {
		return current
	}

	return restore
}

func isShellCommand(cmd string) bool {
	base := executableName(cmd)
	shells := map[string]struct{}{
		"bash": {},
		"zsh":  {},
		"fish": {},
		"sh":   {},
		"ksh":  {},
	}
	_, ok := shells[base]

	return ok
}

func executableName(cmd string) string {
	fields := strings.Fields(strings.TrimSpace(cmd))
	if len(fields) == 0 {
		return ""
	}

	base := filepath.Base(fields[0])

	return strings.TrimPrefix(base, "-")
}

func sanitizeCommand(cmd string) string {
	cmd = strings.TrimSpace(cmd)
	if len(cmd) >= 2 {
		if (cmd[0] == '"' && cmd[len(cmd)-1] == '"') ||
			(cmd[0] == '\'' && cmd[len(cmd)-1] == '\'') {
			cmd = strings.TrimSpace(cmd[1 : len(cmd)-1])
		}
	}

	return cmd
}

// paneRestoreReceipt holds the pending token and ack found on a live pane.
type paneRestoreReceipt struct {
	token string
	ack   string
}

// paneRestoreReceipts reports staged restore tokens and acknowledgments for
// every live pane of the session. It lets a resumed generation distinguish a
// pane whose command was already staged from one that still needs replay.
func (client *Client) paneRestoreReceipts(sessionName string) map[string]paneRestoreReceipt {
	result := make(map[string]paneRestoreReceipt)

	out, err := client.Output(
		"list-panes",
		"-s",
		"-t",
		sessionTarget(sessionName),
		"-F",
		"#{window_index}"+fieldSep+
			"#{pane_index}"+fieldSep+
			"#{@lazy_tmux_restore_pending}"+fieldSep+
			"#{?@lazy_tmux_restore_ack,#{@lazy_tmux_restore_ack},#{@antex_restore_ack}}",
	)
	if err != nil {
		return result
	}

	for _, line := range splitLines(out) {
		parts := splitFieldsN(line, paneReceiptFields)
		if len(parts) != paneReceiptFields {
			continue
		}

		token := ""
		if intent, ok := decodeRestoreIntent(parts[2]); ok {
			token = intent.Token
		}

		result[parts[0]+"."+parts[1]] = paneRestoreReceipt{
			token: token,
			ack:   strings.TrimSpace(parts[3]),
		}
	}

	return result
}

func (client *Client) restoreWindowCommandsJournaled(
	ctx context.Context,
	sessionName string,
	window snapshot.Window,
	windowIndex int,
	journal *RestoreJournal,
	commit RestoreJournalCommit,
) error {
	if len(window.Panes) == 0 {
		return nil
	}

	entry := journal.window(windowIndex)

	panes := make([]snapshot.Pane, len(window.Panes))
	copy(panes, window.Panes)
	sort.Slice(panes, func(i, j int) bool { return panes[i].Index < panes[j].Index })

	var receipts map[string]paneRestoreReceipt
	if commit != nil {
		receipts = client.paneRestoreReceipts(sessionName)
	}

	for _, pane := range panes {
		err := client.restorePaneCommand(
			ctx, sessionName, windowIndex, pane, receipts, entry, journal, commit,
		)
		if err != nil {
			return err
		}
	}

	return nil
}

// restorePaneCommand restores a single pane unless its command was already
// confirmed for the generation.
func (client *Client) restorePaneCommand(
	ctx context.Context,
	sessionName string,
	windowIndex int,
	pane snapshot.Pane,
	receipts map[string]paneRestoreReceipt,
	entry *WindowJournal,
	journal *RestoreJournal,
	commit RestoreJournalCommit,
) error {
	if _, sent := entry.Commands[pane.Index]; sent {
		return nil
	}

	key := fmt.Sprintf("%d.%d", windowIndex, pane.Index)
	if receipt, ok := receipts[key]; ok && receipt.token != "" {
		// The pending token is still on the live pane: the command was already
		// staged, so keep the token instead of launching a second agent. The
		// ack is completed later once the pane is ready.
		entry.Commands[pane.Index] = receipt.token

		return client.commitJournal(journal, commit)
	}

	target := sessionPaneTarget(sessionName, windowIndex, pane.Index)

	marker, err := client.stagePaneRestoreCommand(ctx, target, pane)
	if err != nil {
		return err
	}
	if marker == "" {
		return nil
	}

	entry.Commands[pane.Index] = marker

	return client.commitJournal(journal, commit)
}

// stagePaneRestoreCommand resolves, guards, stages and sends one pane restore
// command. It returns the confirmation marker, or an empty string when this
// pane has no command to launch.
func (client *Client) stagePaneRestoreCommand(
	ctx context.Context,
	target string,
	pane snapshot.Pane,
) (string, error) {
	cmd, resolveErr := client.checkedRestoreCommand(pane)
	if resolveErr != nil {
		message := "lazy-tmux: automatic resume skipped: " + resolveErr.Error()
		cmd = "printf '%s\\n' '" + strings.ReplaceAll(message, "'", "'\"'\"'") + "'"
	}

	if strings.TrimSpace(cmd) == "" {
		return "", nil
	}

	if resolveErr == nil && !client.commandAllowed(cmd) {
		return "", nil
	}

	var (
		token      string
		stagedCmd  string
		err        error
	)

	err = restoreCanceled(ctx)
	if err != nil {
		return "", err
	}

	token, stagedCmd, err = client.stageAgentRestore(target, pane, cmd)
	if err != nil {
		return "", err
	}

	_, err = client.Output("send-keys", "-t", target, stagedCmd, "C-m")
	if err != nil {
		return "", err
	}

	if token != "" {
		return token, nil
	}

	return commandConfirmed, nil
}

func (client *Client) commandAllowed(command string) bool {
	command = strings.TrimSpace(command)

	if matchesAny(client.denylist, command) {
		return false
	}

	if !client.allowlistSet {
		return true
	}

	return matchesAny(client.allowlist, command)
}

func matchesAny(patterns []*regexp.Regexp, command string) bool {
	for _, re := range patterns {
		if re.MatchString(command) {
			return true
		}
	}

	return false
}

func (client *Client) expectedPaneCommands(windows []snapshot.Window) map[string]string {
	want := make(map[string]string)

	for _, window := range windows {
		for _, pane := range window.Panes {
			cmd := client.effectiveRestoreCommand(pane)

			exe := executableName(cmd)
			if cmd != "" &&
				(pane.Meta["antex.session_id_source"] == antexBindingSource || pane.Agent != nil) {
				exe = agent.PaneKind(pane)
			}
			if exe == "" || !client.commandAllowed(cmd) {
				continue
			}

			want[fmt.Sprintf("%d.%d", window.Index, pane.Index)] = exe
		}
	}

	return want
}

func (client *Client) paneCommands(sessionName string) map[string]string {
	result := make(map[string]string)

	out, err := client.Output(
		"list-panes", "-s", "-t", sessionTarget(sessionName),
		"-F", "#{window_index} #{pane_index} #{pane_current_command}",
	)
	if err != nil {
		return result
	}

	for _, line := range splitLines(out) {
		fields := strings.Fields(line)
		if len(fields) < paneCommandFields {
			continue
		}

		result[fields[0]+"."+fields[1]] = fields[2]
	}

	return result
}

func (client *Client) waitForRestoredCommands(
	ctx context.Context,
	sessionName string,
	windows []snapshot.Window,
) {
	if client.settleTimeout <= 0 {
		return
	}

	want := client.expectedPaneCommands(windows)
	if len(want) == 0 {
		return
	}

	deadline := time.Now().Add(client.settleTimeout)

	for {
		if ctx.Err() != nil {
			return
		}

		got := client.paneCommands(sessionName)

		pending := false

		for key, exe := range want {
			if got[key] != exe {
				pending = true

				break
			}
		}

		if !pending || time.Now().After(deadline) {
			return
		}

		select {
		case <-ctx.Done():
			return
		case <-time.After(restoreSettlePollInterval):
		}
	}
}

func (client *Client) restoreWindowScrollback(
	ctx context.Context,
	sessionName string,
	window snapshot.Window,
	windowIndex int,
	journal *RestoreJournal,
	commit RestoreJournalCommit,
) error {
	if len(window.Panes) == 0 {
		return nil
	}

	entry := journal.window(windowIndex)

	panes := make([]snapshot.Pane, len(window.Panes))
	copy(panes, window.Panes)
	sort.Slice(panes, func(i, j int) bool { return panes[i].Index < panes[j].Index })

	for _, pane := range panes {
		if slices.Contains(entry.Scrollback, pane.Index) {
			continue
		}

		if pane.Scrollback == nil || strings.TrimSpace(pane.Scrollback.Content) == "" {
			continue
		}

		err := restoreCanceled(ctx)
		if err != nil {
			return err
		}

		target := sessionPaneTarget(sessionName, windowIndex, pane.Index)

		tty, err := client.Output("display-message", "-p", "-t", target, "#{pane_tty}")
		if err != nil {
			continue
		}

		err = paneTTYWriter(strings.TrimSpace(tty), pane.Scrollback.Content)
		if err != nil {
			continue
		}

		entry.Scrollback = append(entry.Scrollback, pane.Index)
		slices.Sort(entry.Scrollback)

		err = client.commitJournal(journal, commit)
		if err != nil {
			return err
		}
	}

	return nil
}

func writePaneTTY(path, content string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errEmptyTTYPath
	}

	if !strings.HasPrefix(path, "/dev/pts/") && !strings.HasPrefix(path, "/dev/tty") {
		return fmt.Errorf("%w: %s", errUnsupportedTTYPath, path)
	}

	fi, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("stat tty: %w", err)
	}

	if fi.Mode()&os.ModeCharDevice == 0 {
		return fmt.Errorf("%w: %s", errTTYNotCharDevice, path)
	}

	ttyFile, err := os.OpenFile(
		path,
		os.O_WRONLY,
		0,
	) // #nosec G304 -- validated above: /dev/* character device
	if err != nil {
		return fmt.Errorf("open tty: %w", err)
	}

	defer func() { _ = ttyFile.Close() }()

	_, err = io.WriteString(ttyFile, content)
	if err != nil {
		return fmt.Errorf("write to tty: %w", err)
	}

	return nil
}

func processTreeLines(lines []string, rootPID int) []string {
	snapshot := newProcessSnapshot(lines)
	processes := snapshot.descendants(rootPID)

	tree := make([]string, 0, len(processes))
	for _, process := range processes {
		tree = append(
			tree,
			fmt.Sprintf("%d %d %s %s", process.pid, process.ppid, process.stat, process.cmd),
		)
	}

	return tree
}

type processSnapshot struct {
	processes map[int]psProcess
	children  map[int][]int
}

func newProcessSnapshot(lines []string) processSnapshot {
	processes := make(map[int]psProcess)
	for _, line := range lines {
		pid, ppid, stat, command, ok := parsePSLine(line)
		if ok {
			processes[pid] = psProcess{pid: pid, ppid: ppid, stat: stat, cmd: command}
		}
	}

	children := make(map[int][]int)
	for _, process := range processes {
		children[process.ppid] = append(children[process.ppid], process.pid)
	}
	for parent := range children {
		sort.Ints(children[parent])
	}

	return processSnapshot{processes: processes, children: children}
}

func (snapshot processSnapshot) foregroundCommand(rootPID int) string {
	processes := snapshot.descendants(rootPID)
	nonShell := make([]psProcess, 0, len(processes))
	var shells []psProcess
	for _, process := range processes {
		if isShellCommand(process.cmd) {
			if strings.Contains(process.stat, "+") {
				shells = append(shells, process)
			}

			continue
		}

		nonShell = append(nonShell, process)
	}
	if command := pickFromCandidates(nonShell); command != "" {
		return command
	}

	return pickFromCandidates(shells)
}

func (snapshot processSnapshot) descendants(rootPID int) []psProcess {
	seen := map[int]bool{rootPID: true}
	queue := []int{rootPID}
	for len(queue) > 0 {
		parent := queue[0]
		queue = queue[1:]
		for _, child := range snapshot.children[parent] {
			if seen[child] {
				continue
			}
			seen[child] = true
			queue = append(queue, child)
		}
	}

	result := make([]psProcess, 0, len(seen)-1)
	pids := make([]int, 0, len(seen)-1)
	for pid := range seen {
		if pid == rootPID {
			continue
		}
		pids = append(pids, pid)
	}
	sort.Ints(pids)
	for _, pid := range pids {
		process, ok := snapshot.processes[pid]
		if !ok {
			continue
		}
		result = append(result, process)
	}

	return result
}

type psProcess struct {
	pid  int
	ppid int
	stat string
	cmd  string
}

func collectCandidateProcesses(lines []string, panePID int) ([]psProcess, []psProcess) {
	nonShell := make([]psProcess, 0, len(lines))

	var shells []psProcess

	for _, line := range lines {
		pid, ppid, stat, cmd, ok := parsePSLine(line)
		if !ok {
			continue
		}

		if pid == panePID {
			continue
		}

		process := psProcess{
			pid:  pid,
			ppid: ppid,
			stat: stat,
			cmd:  cmd,
		}

		if isShellCommand(cmd) {
			if strings.Contains(stat, "+") {
				shells = append(shells, process)
			}

			continue
		}

		nonShell = append(nonShell, process)
	}

	return nonShell, shells
}

func pickForegroundCommand(lines []string, panePID int) string {
	nonShell, shells := collectCandidateProcesses(lines, panePID)

	if cmd := pickFromCandidates(nonShell); cmd != "" {
		return cmd
	}

	return pickFromCandidates(shells)
}

func pickFromCandidates(allProcesses []psProcess) string {
	if len(allProcesses) == 0 {
		return ""
	}

	allPIDs := make(map[int]struct{}, len(allProcesses))
	for _, p := range allProcesses {
		allPIDs[p.pid] = struct{}{}
	}

	var rootProcesses []psProcess

	for _, p := range allProcesses {
		if _, parentInSet := allPIDs[p.ppid]; !parentInSet {
			rootProcesses = append(rootProcesses, p)
		}
	}

	if len(rootProcesses) > 0 {
		for _, p := range rootProcesses {
			if strings.Contains(p.stat, "+") {
				return p.cmd
			}
		}

		return rootProcesses[0].cmd
	}

	for _, p := range allProcesses {
		if strings.Contains(p.stat, "+") {
			return p.cmd
		}
	}

	if len(allProcesses) > 0 {
		return allProcesses[0].cmd
	}

	return ""
}

const (
	livePaneLineFields    = 7
	capturePaneLineFields = 16
	paneCommandFields     = 3
	paneReceiptFields     = 4
	psLineFields          = 4
)

func parsePSLine(line string) (int, int, string, string, bool) {
	fields := strings.Fields(strings.TrimSpace(line))
	if len(fields) < psLineFields {
		return 0, 0, "", "", false
	}

	pid, err := strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, "", "", false
	}

	ppid, err := strconv.Atoi(fields[1])
	if err != nil {
		return 0, 0, "", "", false
	}

	stat := fields[2]

	cmd := strings.Join(fields[3:], " ")
	if strings.TrimSpace(cmd) == "" {
		return 0, 0, "", "", false
	}

	return pid, ppid, stat, cmd, true
}

func splitLines(in string) []string {
	s := bufio.NewScanner(strings.NewReader(in))
	out := make([]string, 0)

	for s.Scan() {
		line := strings.TrimSpace(s.Text())
		if line == "" {
			continue
		}

		out = append(out, line)
	}

	return out
}

func (client *Client) runWithShellFallback(args []string) error {
	_, err := client.Output(args...)
	if err == nil {
		return nil
	}

	// Older tmux servers or unusual shells may reject path/start-command
	// options; retry with progressively smaller argument sets.
	minimal := stripOptionPair(args, "-c")
	if len(minimal) > 0 {
		_, err2 := client.Output(minimal...)
		if err2 == nil {
			return nil
		}
	}

	return err
}

func stripOptionPair(args []string, opt string) []string {
	out := make([]string, 0, len(args))

	for idx := 0; idx < len(args); idx++ {
		if args[idx] == opt {
			idx++

			continue
		}

		out = append(out, args[idx])
	}

	return out
}

func (client *Client) resolveLiveFocus(
	session string,
	sessionSnapshot snapshot.SessionSnapshot,
	windows []snapshot.Window,
) (int, int, bool) {
	liveWindows := client.liveIndexes("list-windows", sessionTarget(session), "#{window_index}")
	if len(liveWindows) == 0 {
		return 0, 0, false
	}

	wantWin, wantPane := resolveRestoreFocus(sessionSnapshot, windows)

	win := wantWin
	if !slices.Contains(liveWindows, win) {
		win = liveWindows[0]
	}

	pane := wantPane
	livePanes := client.liveIndexes(
		"list-panes",
		sessionWindowTarget(session, win),
		"#{pane_index}",
	)
	if len(livePanes) > 0 && !slices.Contains(livePanes, pane) {
		pane = livePanes[0]
	}

	return win, pane, true
}

func (client *Client) liveIndexes(listCmd, target, format string) []int {
	out, err := client.Output(listCmd, "-t", target, "-F", format)
	if err != nil {
		return nil
	}

	var indexes []int
	for _, line := range splitLines(out) {
		n, convErr := strconv.Atoi(strings.TrimSpace(line))
		if convErr != nil {
			continue
		}

		indexes = append(indexes, n)
	}

	return indexes
}

func (client *Client) createdFirstWindowIndex(session string) (int, error) {
	out, err := client.Output("list-windows", "-t", sessionTarget(session), "-F", "#{window_index}")
	if err != nil {
		return 0, err
	}

	lines := splitLines(out)
	if len(lines) == 0 {
		return 0, errNoWindowsAfterCreate
	}

	idx, err := strconv.Atoi(lines[0])
	if err != nil {
		return 0, fmt.Errorf("parse window index: %w", err)
	}

	return idx, nil
}
