package picker

import (
	"strings"
	"testing"

	"github.com/alchemmist/lazy-tmux/internal/snapshot"
)

func TestForeignSessionRowsLabeledAndTargetScoped(t *testing.T) {
	t.Parallel()

	sessions := []Session{
		{
			Record:   snapshot.Record{SessionName: "shared"},
			ServerID: "srv-current",
		},
		{
			Record:   snapshot.Record{SessionName: "shared"},
			ServerID: "srv-other",
			Foreign:  true,
			Label:    "work",
		},
	}

	rows := filteredTreeRows(sessions, "", nil, 100)
	if len(rows) != 2 {
		t.Fatalf("expected both sessions as rows, got %d: %+v", len(rows), rows)
	}

	var foreignRow *pickerRow
	for index := range rows {
		if rows[index].target.ServerID == "srv-other" {
			foreignRow = &rows[index]

			break
		}
	}
	if foreignRow == nil {
		t.Fatalf("foreign row missing: %+v", rows)
	}
	if !strings.Contains(foreignRow.item, "shared") ||
		!strings.Contains(foreignRow.item, "@work") {
		t.Fatalf("foreign row must carry @work marker: %q", foreignRow.item)
	}
	if foreignRow.target.SessionName != "shared" || foreignRow.target.ServerID != "srv-other" {
		t.Fatalf("foreign target not scoped: %+v", foreignRow.target)
	}
	if foreignRow.state != sessionStateIcon(false, true) {
		t.Fatalf("foreign row must show the offline state icon: %q", foreignRow.state)
	}

	current := rows[0]
	if current.target.ServerID != "srv-current" {
		t.Fatalf("current row should keep its server target: %+v", current)
	}
	if strings.Contains(current.item, "@") {
		t.Fatalf("current row should not carry a foreign marker: %q", current.item)
	}
}
