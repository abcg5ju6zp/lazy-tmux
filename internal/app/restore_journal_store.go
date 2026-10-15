package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/alchemmist/lazy-tmux/internal/tmux"
)

const (
	restoreJournalDirName  = "restores"
	restoreJournalLockName = ".restore-locks"
	restoreJournalFilePerm = 0o644
)

func (a *App) restoreJournalDir() string {
	return filepath.Join(a.store.DataDir(), restoreJournalDirName)
}

func (a *App) restoreJournalPath(name string) string {
	return filepath.Join(a.restoreJournalDir(), safeJournalName(name)+".json")
}

// safeJournalName mirrors store.sanitizeName for journal file naming.
func safeJournalName(name string) string {
	replacer := strings.NewReplacer("/", "_", "\\", "_", " ", "_", ":", "_")

	out := replacer.Replace(strings.TrimSpace(name))
	if out == "" {
		return "session"
	}

	return out
}

func writeRestoreJournalAtomic(path string, journal tmux.RestoreJournal) error {
	data, err := json.MarshalIndent(journal, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal restore journal: %w", err)
	}

	tmp := path + ".tmp"
	err = os.WriteFile(tmp, append(data, '\n'), restoreJournalFilePerm)
	if err != nil {
		return fmt.Errorf("write restore journal: %w", err)
	}

	err = os.Rename(tmp, path)
	if err != nil {
		return fmt.Errorf("rename restore journal: %w", err)
	}

	return nil
}

func (a *App) saveRestoreJournal(journal tmux.RestoreJournal) error {
	if strings.TrimSpace(journal.SessionName) == "" {
		return errSessionNameEmpty
	}

	err := os.MkdirAll(a.restoreJournalDir(), 0o750)
	if err != nil {
		return fmt.Errorf("create restores dir: %w", err)
	}

	return writeRestoreJournalAtomic(a.restoreJournalPath(journal.SessionName), journal)
}

func (a *App) loadRestoreJournal(name string) (tmux.RestoreJournal, bool, error) {
	var journal tmux.RestoreJournal

	data, err := os.ReadFile(
		a.restoreJournalPath(name),
	) // #nosec G304 -- sanitized name under data dir
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return journal, false, nil
		}

		return journal, false, fmt.Errorf("read restore journal: %w", err)
	}

	err = json.Unmarshal(data, &journal)
	if err != nil {
		return journal, false, fmt.Errorf("decode restore journal: %w", err)
	}

	if journal.Windows == nil {
		journal.Windows = make(map[int]*tmux.WindowJournal)
	}

	return journal, true, nil
}

func (a *App) deleteRestoreJournal(name string) error {
	err := os.Remove(a.restoreJournalPath(name))
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove restore journal: %w", err)
	}

	return nil
}

func (a *App) listRestoreJournals() ([]tmux.RestoreJournal, error) {
	entries, err := os.ReadDir(a.restoreJournalDir())
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, nil
		}

		return nil, fmt.Errorf("list restore journals: %w", err)
	}

	journals := make([]tmux.RestoreJournal, 0, len(entries))

	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}

		data, err := os.ReadFile(
			filepath.Join(a.restoreJournalDir(), entry.Name()),
		) // #nosec G304 -- fixed directory contents
		if err != nil {
			continue
		}

		var journal tmux.RestoreJournal
		err = json.Unmarshal(data, &journal)
		if err != nil || journal.SessionName == "" {
			continue
		}
		if journal.Windows == nil {
			journal.Windows = make(map[int]*tmux.WindowJournal)
		}

		journals = append(journals, journal)
	}

	return journals, nil
}

// lockRestore serializes generations for one session across processes.
// flock is released automatically when the process dies, which is what makes
// restart takeover safe.
func (a *App) lockRestore(name string) (func(), error) {
	lockDir := filepath.Join(a.store.DataDir(), restoreJournalLockName)
	err := os.MkdirAll(lockDir, 0o700)
	if err != nil {
		return nil, fmt.Errorf("create restore lock dir: %w", err)
	}

	lockPath := filepath.Join(lockDir, safeJournalName(name)+".lock")

	// #nosec G304 -- sanitized lock name under the configured data directory
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open restore lock: %w", err)
	}

	err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX)
	if err != nil {
		_ = file.Close()

		return nil, fmt.Errorf("lock restore: %w", err)
	}

	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}
