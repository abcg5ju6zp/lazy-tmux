package tmux

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"

	"github.com/alchemmist/lazy-tmux/internal/integration/agent"
	"github.com/alchemmist/lazy-tmux/internal/snapshot"
)

const (
	pendingRestoreOption = "@lazy_tmux_restore_pending"
	restoreAckOption     = "@lazy_tmux_restore_ack"
	pendingRestoreMeta   = agent.PendingKey
)

type pendingRestore struct {
	Token string        `json:"token"`
	Pane  snapshot.Pane `json:"pane"`
}

func (client *Client) stageAgentRestore(
	target string,
	pane snapshot.Pane,
	command string,
) (string, string, error) {
	if agent.PaneKind(pane) == "" {
		return "", command, nil
	}
	pane.Scrollback = nil
	intent := pendingRestore{Token: rand.Text(), Pane: pane}
	data, err := json.Marshal(intent)
	if err != nil {
		return "", "", fmt.Errorf("encode restore intent: %w", err)
	}
	args := []string{
		"set-option",
		"-p",
		"-t",
		target,
		pendingRestoreOption,
		base64.StdEncoding.EncodeToString(data),
		";", "set-option", "-p", "-t", target, restoreAckOption, "",
	}
	if agent.PaneKind(pane) == snapshot.AgentAntex {
		args = append(args, ";", "set-option", "-p", "-t", target, "@antex_restore_ack", "")
	}
	_, err = client.Output(args...)
	if err != nil {
		return "", "", err
	}

	return intent.Token, "env LAZY_TMUX_RESTORE_TOKEN=" + intent.Token + " " + command, nil
}

func decodeRestoreIntent(encoded string) (pendingRestore, bool) {
	var intent pendingRestore
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil || json.Unmarshal(data, &intent) != nil || intent.Token == "" {
		return intent, false
	}

	return intent, true
}

func preserveRestoreIntent(pane snapshot.Pane, encoded, ack string) snapshot.Pane {
	intent, ok := decodeRestoreIntent(encoded)
	if !ok || intent.Token == ack ||
		(pane.Meta["antex.session_id_source"] == antexBindingSource || pane.Agent != nil) ||
		!agentStartupCommand(pane.CurrentCmd) {
		return pane
	}
	pane.CurrentCmd = intent.Pane.CurrentCmd
	pane.RestoreCmd = intent.Pane.RestoreCmd
	pane.CurrentPath = intent.Pane.CurrentPath
	pane.Agent = intent.Pane.Agent
	pane.Meta = intent.Pane.Meta
	if pane.Meta == nil {
		pane.Meta = make(map[string]string)
	}
	pane.Meta[pendingRestoreMeta] = "1"

	return pane
}

func (client *Client) acknowledgeRestoredPanes(output string, windows []snapshot.Window) {
	ready := make(map[string]bool)
	for _, window := range windows {
		for _, pane := range window.Panes {
			if pane.Meta[pendingRestoreMeta] == "" &&
				((pane.Meta["antex.session_id_source"] == antexBindingSource || pane.Agent != nil) ||
					!agentStartupCommand(pane.CurrentCmd)) {
				ready[strconv.Itoa(window.Index)+"."+strconv.Itoa(pane.Index)] = true
			}
		}
	}
	for _, line := range splitLines(output) {
		parts := splitFieldsN(line, capturePaneLineFields)
		if len(parts) != capturePaneLineFields || !ready[parts[0]+"."+parts[4]] {
			continue
		}
		intent, ok := decodeRestoreIntent(parts[12])
		if ok && intent.Token != parts[13] {
			_, _ = client.Output(
				"set-option",
				"-p",
				"-t",
				parts[10],
				restoreAckOption,
				intent.Token,
			)
		}
	}
}

// reconcileRestoreAcks completes acknowledgments for panes whose restore
// command was staged by an earlier attempt of the generation but whose
// confirmation (agent hook or capture) never ran. It uses the same readiness
// rules as CaptureSession, so a token is acknowledged only when the pane is
// demonstrably past startup.
func (client *Client) reconcileRestoreAcks(ctx context.Context, sessionName string) {
	if ctx.Err() != nil {
		return
	}

	out, err := client.Output(
		"list-panes",
		"-s",
		"-t",
		sessionTarget(sessionName),
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
		return
	}

	//nolint:contextcheck // the ps-based process snapshot helper is context-free by design
	processes, err := loadProcessSnapshot()
	if err != nil {
		return
	}

	client.acknowledgeRestoredPanes(out, parseCapturedPanes(out, processes))
}

func agentStartupCommand(command string) bool {
	name := executableName(command)

	return strings.TrimSpace(command) == "" || isShellCommand(command) ||
		agent.Kind(command) != "" ||
		name == "env"
}
