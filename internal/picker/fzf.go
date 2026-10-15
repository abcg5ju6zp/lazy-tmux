package picker

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/alchemmist/lazy-tmux/internal/snapshot"
	"github.com/alchemmist/lazy-tmux/internal/store"
	"github.com/charmbracelet/x/ansi"
)

var (
	errFzfTimedOut      = errors.New("fzf selection timed out")
	errNoSelection      = errors.New("no selection made")
	errInvalidFzfOutput = errors.New("invalid fzf output")
)

const fzfSelectionTimeout = 30 * time.Second

var (
	ErrNoSessions = errors.New("no sessions available")
	ErrNoWindows  = errors.New("no windows available")
)

const (
	fzfNameMax = 40
	fzfWinName = 24
	fzfCmdMax  = 30
	fzfColGap  = "  "
	fzfMetaSep = " · "
	// Hidden tab fields on a window line: display, session, index, server id.
	fzfWindowFields = 4
)

const fzfMetaColor = "#9e9e9e"

const fzfTimeLayout = "2006-01-02 15:04"

const (
	hexByteMask   = 0xff
	hexRedShift   = 16
	hexGreenShift = 8
)

func fzfFg(hexColor, text string) string {
	value, err := strconv.ParseInt(strings.TrimPrefix(hexColor, "#"), 16, 32)
	if err != nil {
		return text
	}

	red := (value >> hexRedShift) & hexByteMask
	green := (value >> hexGreenShift) & hexByteMask
	blue := value & hexByteMask

	return fmt.Sprintf("\x1b[38;2;%d;%d;%dm%s\x1b[0m", red, green, blue, text)
}

func padCell(text string, width int) string {
	gap := width - ansi.StringWidth(text)
	if gap <= 0 {
		return text
	}

	return text + strings.Repeat(" ", gap)
}

func clampCell(s string, maxWidth int) string {
	if ansi.StringWidth(s) <= maxWidth {
		return s
	}

	return ansi.Truncate(s, maxWidth, "…")
}

func runFZF(input *bytes.Buffer, withNth string) (string, error) {
	args := []string{
		"fzf",
		"--ansi",
		"--delimiter", "\t",
		"--with-nth", withNth,
	}

	interactive := isTerminal(os.Stdout)
	if interactive {
		args = append(args,
			"--prompt", "lazy-tmux> ",
			"--height", "100%",
			"--layout", "reverse",
			"--info", "hidden",
		)
	} else {
		args = append(args, "--filter", "")
	}

	ctx := context.Background()

	if !interactive {
		var cancel context.CancelFunc

		ctx, cancel = context.WithTimeout(context.Background(), fzfSelectionTimeout)
		defer cancel()
	}

	cmd := exec.CommandContext(
		ctx,
		args[0],
		args[1:]...,
	) // #nosec G204 -- argv[0] is the fzf binary with args built above
	cmd.Stdin = input

	out, err := cmd.Output()
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return "", errFzfTimedOut
		}

		return "", fmt.Errorf("fzf selection canceled or failed: %w", err)
	}

	selected := strings.TrimSpace(string(out))
	selected = strings.SplitN(selected, "\n", 2)[0]

	if strings.TrimSpace(selected) == "" {
		return "", errNoSelection
	}

	return selected, nil
}

func sessionFZFLines(records []snapshot.Record) []string {
	type row struct{ name, when, count, server string }

	rows := make([]row, len(records))

	var nameW, countW int

	for index, rec := range records {
		name := clampCell(rec.SessionName, fzfNameMax)
		rows[index] = row{
			name:   name,
			when:   rec.CapturedAt.Local().Format(fzfTimeLayout),
			count:  fmt.Sprintf("%dw", rec.Windows),
			server: recordServer(rec).ServerID,
		}

		nameW = maxInt(nameW, ansi.StringWidth(name))
		countW = maxInt(countW, ansi.StringWidth(rows[index].count))
	}

	lines := make([]string, len(records))
	for index, item := range rows {
		meta := fzfFg(fzfMetaColor, padLeft(item.count, countW)+fzfMetaSep+item.when)
		display := padCell(item.name, nameW) + fzfColGap + meta
		lines[index] = strings.Join(
			[]string{display, records[index].SessionName, item.server},
			"\t",
		)
	}

	return lines
}

func recordServer(r snapshot.Record) Target {
	return Target{SessionName: r.SessionName, ServerID: store.ServerID(r.ServerSocket)}
}

// ChooseSessionFZF returns the chosen session target, including the owning
// server namespace.
func ChooseSessionFZF(records []snapshot.Record) (Target, error) {
	if len(records) == 0 {
		return Target{}, ErrNoSessions
	}

	var input bytes.Buffer

	for _, line := range sessionFZFLines(records) {
		input.WriteString(line)
		input.WriteByte('\n')
	}

	selected, err := runFZF(&input, "1")
	if err != nil {
		return Target{}, err
	}

	return parseSessionSelection(selected)
}

func parseSessionSelection(line string) (Target, error) {
	parts := strings.Split(line, "\t")
	if len(parts) < 2 {
		return Target{}, errInvalidFzfOutput
	}

	name := strings.TrimSpace(parts[len(parts)-2])
	if name == "" {
		return Target{}, errInvalidFzfOutput
	}

	return Target{SessionName: name, ServerID: strings.TrimSpace(parts[len(parts)-1])}, nil
}

func windowFZFLines(sessions []Session, windowSort []WindowSortKey) []string {
	var rows []windowFZFRow

	for _, session := range sessions {
		windows := make([]snapshot.Window, len(session.Windows))
		copy(windows, session.Windows)
		sortWindows(windows, windowSort)

		captured := session.Record.CapturedAt.Local().Format(fzfTimeLayout)
		for _, window := range windows {
			rows = append(rows, buildWindowRow(session, window, captured))
		}
	}

	var sessW, idxW, nameW, cmdW int

	for _, rowData := range rows {
		sessW = maxInt(sessW, ansi.StringWidth(clampCell(rowData.session, fzfNameMax)))
		idxW = maxInt(idxW, len(strconv.Itoa(rowData.index)))
		nameW = maxInt(nameW, ansi.StringWidth(rowData.name))
		cmdW = maxInt(cmdW, ansi.StringWidth(rowData.cmd))
	}

	lines := make([]string, len(rows))

	for index, item := range rows {
		head := strings.Join([]string{
			padCell(clampCell(item.session, fzfNameMax), sessW),
			padLeft(strconv.Itoa(item.index), idxW),
			padCell(item.name, nameW),
		}, fzfColGap)

		meta := fzfFg(fzfMetaColor, padCell(item.cmd, cmdW)+fzfMetaSep+item.when)
		display := head + fzfColGap + meta
		lines[index] = fmt.Sprintf(
			"%s\t%s\t%d\t%s",
			display, item.session, item.index, item.server,
		)
	}

	return lines
}

func buildWindowRow(session Session, window snapshot.Window, captured string) windowFZFRow {
	name := window.Name
	if name == "" {
		name = "-"
	}

	cmd := windowPreviewCommand(window)
	if cmd == "" {
		cmd = "-"
	}

	return windowFZFRow{
		session: session.Record.SessionName,
		index:   window.Index,
		name:    clampCell(name, fzfWinName),
		cmd:     clampCell(cmd, fzfCmdMax),
		when:    captured,
		server:  session.ServerID,
	}
}

type windowFZFRow struct {
	session, name, cmd, when, server string
	index                            int
}

func parseWindowSelection(line string) (Target, error) {
	parts := strings.Split(line, "\t")
	if len(parts) < fzfWindowFields {
		return Target{}, errInvalidFzfOutput
	}

	session := parts[len(parts)-3]
	if strings.TrimSpace(session) == "" {
		return Target{}, errInvalidFzfOutput
	}

	index, err := strconv.Atoi(strings.TrimSpace(parts[len(parts)-2]))
	if err != nil {
		return Target{}, fmt.Errorf("invalid window index in fzf output: %w", err)
	}

	return Target{
		SessionName: session,
		WindowIndex: &index,
		ServerID:    strings.TrimSpace(parts[len(parts)-1]),
	}, nil
}

func ChooseWindowFZF(sessions []Session, windowSort []WindowSortKey) (Target, error) {
	if len(sessions) == 0 {
		return Target{}, ErrNoSessions
	}

	lines := windowFZFLines(sessions, windowSort)
	if len(lines) == 0 {
		return Target{}, ErrNoWindows
	}

	var input bytes.Buffer
	for _, line := range lines {
		input.WriteString(line)
		input.WriteByte('\n')
	}

	selected, err := runFZF(&input, "1")
	if err != nil {
		return Target{}, err
	}

	return parseWindowSelection(selected)
}

func padLeft(text string, width int) string {
	gap := width - ansi.StringWidth(text)
	if gap <= 0 {
		return text
	}

	return strings.Repeat(" ", gap) + text
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}

	return b
}
