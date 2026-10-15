//go:build !lazy_fzf

package picker

import (
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
)

var (
	errDeleteWindowUnavailable  = errors.New("delete window not available")
	errDeleteSessionUnavailable = errors.New("delete session not available")
	errSelectSessionToDelete    = errors.New("select a session to delete")
	errRenameWindowUnavailable  = errors.New("rename window not available")
	errRenameSessionUnavailable = errors.New("rename session not available")
	errNewSessionUnavailable    = errors.New("new session not available")
	errNewWindowUnavailable     = errors.New("new window not available")
	errSelectSessionForWindow   = errors.New("select a session to create a window")
	errSelectSessionToWakeup    = errors.New("select a session to wakeup")
	errWakeupUnavailable        = errors.New("wakeup not available")
	errSelectSessionToSleep     = errors.New("select a session to sleep")
	errSleepUnavailable         = errors.New("sleep not available")
)

type markState int

const (
	markNone markState = iota
	markPartial
	markFull
)

const targetKeyParts = 3

func targetKey(serverID, session string, windowIndex int) string {
	return serverID + "\x00" + session + "\x00" + strconv.Itoa(windowIndex)
}

func parseTargetKey(key string) (string, string, int, bool) {
	parts := strings.Split(key, "\x00")
	if len(parts) != targetKeyParts {
		return "", "", 0, false
	}

	index, err := strconv.Atoi(parts[2])
	if err != nil {
		return "", "", 0, false
	}

	return parts[0], parts[1], index, true
}

type sessionRef struct {
	serverID string
	name     string
}

func (m *pickerModel) sessionWindowIndices(serverID, session string) []int {
	for index := range m.sessions {
		sess := m.sessions[index]
		if sess.Record.SessionName != session || sess.ServerID != serverID {
			continue
		}

		idxs := make([]int, 0, len(sess.Windows))
		for _, win := range sess.Windows {
			idxs = append(idxs, win.Index)
		}

		return idxs
	}

	return nil
}

func (m *pickerModel) markWindow(target Target) {
	if target.WindowIndex == nil {
		return
	}

	m.marked[targetKey(target.ServerID, target.SessionName, *target.WindowIndex)] = struct{}{}
}

func (m *pickerModel) markSession(serverID, session string) {
	idxs := m.sessionWindowIndices(serverID, session)
	if len(idxs) == 0 {
		return
	}

	unmarkAll := true

	for _, idx := range idxs {
		if _, ok := m.marked[targetKey(serverID, session, idx)]; !ok {
			unmarkAll = false

			break
		}
	}

	for _, idx := range idxs {
		key := targetKey(serverID, session, idx)
		if unmarkAll {
			delete(m.marked, key)
		} else {
			m.marked[key] = struct{}{}
		}
	}
}

func (m *pickerModel) toggleMark(row pickerRow) {
	if row.synthetic {
		return
	}

	if row.target.WindowIndex == nil {
		m.markSession(row.target.ServerID, row.target.SessionName)

		return
	}

	key := targetKey(row.target.ServerID, row.target.SessionName, *row.target.WindowIndex)
	if _, ok := m.marked[key]; ok {
		delete(m.marked, key)
	} else {
		m.marked[key] = struct{}{}
	}
}

func (m pickerModel) markState(row pickerRow) markState {
	if row.target.WindowIndex != nil {
		key := targetKey(row.target.ServerID, row.target.SessionName, *row.target.WindowIndex)
		if _, ok := m.marked[key]; ok {
			return markFull
		}

		return markNone
	}

	idxs := m.sessionWindowIndices(row.target.ServerID, row.target.SessionName)
	if len(idxs) == 0 {
		return markNone
	}

	marked := 0

	for _, idx := range idxs {
		if _, ok := m.marked[targetKey(row.target.ServerID, row.target.SessionName, idx)]; ok {
			marked++
		}
	}

	switch {
	case marked == 0:
		return markNone
	case marked == len(idxs):
		return markFull
	default:
		return markPartial
	}
}

func (m *pickerModel) commitDelete() {
	if len(m.marked) == 0 {
		m.exitMode()

		return
	}

	bySession := map[sessionRef][]int{}

	for key := range m.marked {
		serverID, session, idx, ok := parseTargetKey(key)
		if !ok {
			continue
		}

		ref := sessionRef{serverID: serverID, name: session}
		bySession[ref] = append(bySession[ref], idx)
	}

	sessions := make([]sessionRef, 0, len(bySession))
	for ref := range bySession {
		sessions = append(sessions, ref)
	}
	sort.Slice(sessions, func(i, j int) bool {
		if sessions[i].serverID != sessions[j].serverID {
			return sessions[i].serverID < sessions[j].serverID
		}

		return sessions[i].name < sessions[j].name
	})

	var firstErr error

	for _, ref := range sessions {
		err := m.deleteMarkedSession(ref.serverID, ref.name, bySession[ref])
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	if firstErr != nil {
		m.setStatus(firstErr.Error())
	} else {
		m.clearStatus()
	}

	m.exitMode()
	m.reload()
	m.renderViewport()
}

func (m *pickerModel) deleteMarkedSession(serverID, session string, idxs []int) error {
	if total := len(m.sessionWindowIndices(serverID, session)); total > 0 && len(idxs) >= total {
		return m.deleteSession(serverID, session)
	}

	if m.actions.DeleteWindow == nil {
		return errDeleteWindowUnavailable
	}

	sort.Sort(sort.Reverse(sort.IntSlice(idxs)))

	var firstErr error

	for _, idx := range idxs {
		err := m.actions.DeleteWindow(serverID, session, idx)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}

	return firstErr
}

func (m *pickerModel) renameCurrentWindow() {
	row, ok := m.currentRow()
	if !ok || row.target.WindowIndex == nil {
		m.setStatus("select a window row to rename")

		return
	}

	m.pending = row.target
	m.mode = modeRenameWindow
	m.promptInput = textinput.New()
	m.promptInput.Prompt = fmt.Sprintf("Rename window %s: ", row.windowName)
	m.promptInput.SetValue(row.windowName)
	m.promptInput.CursorEnd()
	m.promptInput.Focus()
	m.resize()
}

func (m *pickerModel) renameCurrentSession() {
	row, ok := m.currentRow()
	if !ok {
		m.setStatus("select a session to rename")

		return
	}

	m.pending = row.target
	m.mode = modeRenameSession
	m.promptInput = textinput.New()
	m.promptInput.Prompt = fmt.Sprintf("Rename session %s: ", row.target.SessionName)
	m.promptInput.SetValue(row.target.SessionName)
	m.promptInput.CursorEnd()
	m.promptInput.Focus()
	m.resize()
}

func (m *pickerModel) newSession() {
	m.pending = Target{}
	m.mode = modeNewSession
	m.promptInput = textinput.New()
	m.promptInput.Prompt = "New session name: "
	m.promptInput.Focus()
	m.resize()
}

func (m *pickerModel) newWindow() {
	row, ok := m.currentRow()
	if !ok || strings.TrimSpace(row.target.SessionName) == "" {
		m.setStatus("select a session to create a window")

		return
	}

	m.pending = row.target
	m.mode = modeNewWindow
	m.promptInput = textinput.New()
	m.promptInput.Prompt = fmt.Sprintf("New window in %s: ", row.target.SessionName)
	m.promptInput.Focus()
	m.resize()
}

func (m pickerModel) handlePromptKey(msg tea.KeyPressMsg) (tea.Model, tea.Cmd) {
	switch msg.String() {
	case keyEsc, keyCtrlC:
		m.promptInput.Blur()
		m.mode = modeBrowse
		m.exitMode()

		return m, nil
	case keyEnter:
		m.commitPrompt()

		m.promptInput.Blur()
		m.mode = modeBrowse
		m.exitMode()

		return m, nil
	}

	var cmd tea.Cmd

	m.promptInput, cmd = m.promptInput.Update(msg)

	return m, cmd
}

func (m *pickerModel) commitPrompt() {
	name := strings.TrimSpace(m.promptInput.Value())

	var (
		err error
		ran bool
	)

	switch m.mode {
	case modeBrowse:
		return
	case modeRenameWindow:
		if name != "" && m.pending.WindowIndex != nil {
			err = m.renameWindow(
				m.pending.ServerID,
				m.pending.SessionName,
				*m.pending.WindowIndex,
				name,
			)
			ran = true
		}
	case modeRenameSession:
		if name != "" {
			err = m.renameSession(m.pending.ServerID, m.pending.SessionName, name)
			ran = true
		}
	case modeNewSession:
		if name != "" {
			err = m.createSession(name)
			ran = true
		}
	case modeNewWindow:
		err = m.createWindow(m.pending.ServerID, m.pending.SessionName, name)
		ran = true
	}

	if !ran {
		return
	}

	if err != nil {
		m.setStatus(err.Error())
	} else {
		m.clearStatus()
	}

	m.reload()
	m.renderViewport()
}

func (m *pickerModel) deleteSession(serverID, session string) error {
	if m.actions.DeleteSession == nil {
		return errDeleteSessionUnavailable
	}

	if strings.TrimSpace(session) == "" {
		return errSelectSessionToDelete
	}

	return m.actions.DeleteSession(serverID, session)
}

func (m *pickerModel) renameWindow(serverID, session string, windowIndex int, name string) error {
	if m.actions.RenameWindow == nil {
		return errRenameWindowUnavailable
	}

	return m.actions.RenameWindow(serverID, session, windowIndex, name)
}

func (m *pickerModel) renameSession(serverID, session, name string) error {
	if m.actions.RenameSession == nil {
		return errRenameSessionUnavailable
	}

	return m.actions.RenameSession(serverID, session, name)
}

func (m *pickerModel) createSession(name string) error {
	if m.actions.NewSession == nil {
		return errNewSessionUnavailable
	}

	return m.actions.NewSession(name)
}

func (m *pickerModel) createWindow(serverID, session, name string) error {
	if m.actions.NewWindow == nil {
		return errNewWindowUnavailable
	}

	if strings.TrimSpace(session) == "" {
		return errSelectSessionForWindow
	}

	return m.actions.NewWindow(serverID, session, name)
}

func (m *pickerModel) wakeupSession() error {
	row, ok := m.currentRow()
	if !ok {
		return errSelectSessionToWakeup
	}

	if m.actions.Wakeup == nil {
		return errWakeupUnavailable
	}

	return m.actions.Wakeup(row.target.ServerID, row.target.SessionName)
}

func (m *pickerModel) sleepSession() error {
	row, ok := m.currentRow()
	if !ok {
		return errSelectSessionToSleep
	}

	if m.actions.Sleep == nil {
		return errSleepUnavailable
	}

	return m.actions.Sleep(row.target.ServerID, row.target.SessionName)
}

func (m *pickerModel) reload() {
	m.reloadGeneration++
	m.loading = false
	if m.actions.Reload == nil {
		return
	}

	sessions, err := m.actions.Reload()
	m.applySessionLoadResult(sessions, err, sessionLoadMutation)
}

func (m *pickerModel) applySessionLoadResult(
	sessions []Session,
	err error,
	source sessionLoadSource,
) {
	if err != nil {
		if source == sessionLoadBackground {
			m.setLoaderStatus(err.Error())
		} else {
			m.setStatus(err.Error())
		}

		return
	}
	if source == sessionLoadBackground && m.statusFromLoader {
		m.clearStatus()
	}

	m.sessions = sessions
	m.applyFilter()
	m.ensureCursorVisible()
}

func (m *pickerModel) currentRow() (pickerRow, bool) {
	if len(m.visible) == 0 || m.cursor < 0 || m.cursor >= len(m.visible) {
		return pickerRow{}, false
	}

	return m.visible[m.cursor], true
}

func (m *pickerModel) setStatus(msg string) {
	m.statusMsg = strings.TrimSpace(msg)
	m.statusFromLoader = false
	m.resize()
}

func (m *pickerModel) setLoaderStatus(msg string) {
	m.statusMsg = strings.TrimSpace(msg)
	m.statusFromLoader = true
	m.resize()
}

func (m *pickerModel) clearStatus() {
	m.statusMsg = ""
	m.statusFromLoader = false
	m.resize()
}

func (m *pickerModel) statusHeight() int {
	if m.statusMsg == "" {
		return 0
	}

	return 1
}

func (m *pickerModel) moveNextSelectable() {
	if len(m.visible) == 0 {
		return
	}

	for i := m.cursor + 1; i < len(m.visible); i++ {
		if m.rowSelectable(m.visible[i]) {
			m.cursor = i

			return
		}
	}
}

func (m *pickerModel) movePrevSelectable() {
	if len(m.visible) == 0 {
		return
	}

	for i := m.cursor - 1; i >= 0; i-- {
		if m.rowSelectable(m.visible[i]) {
			m.cursor = i

			return
		}
	}
}
