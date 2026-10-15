package app

import (
	"context"
	"errors"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/alchemmist/lazy-tmux/internal/tmux"
)

// restoreGeneration is one in-flight restore attempt. The journal survives
// process restarts; the release function ends the cross-process lock.
type restoreGeneration struct {
	journal tmux.RestoreJournal
	release func()
}

// beginRestoreGeneration locks the session and returns the stable generation
// for this target, adopting an unfinished journal or converging a canceled one
// instead of starting a second, overlapping restore.
func (a *App) beginRestoreGeneration(target PickerTarget) (*restoreGeneration, error) {
	name := strings.TrimSpace(target.SessionName)
	if name == "" {
		return nil, errSessionNameEmpty
	}

	release, err := a.lockRestore(name)
	if err != nil {
		return nil, fmt.Errorf("lock restore: %w", err)
	}

	journal, ok, err := a.loadRestoreJournal(name)
	if err != nil {
		release()

		return nil, err
	}

	if ok {
		journal.Adopted = true

		switch journal.Status {
		case tmux.RestoreStatusCanceling, tmux.RestoreStatusCleaning:
			// Finish removing the residue of the previous attempt before a new
			// restore can create resources with the same name.
			cleanupErr := a.convergeRestoreCleanup(&journal)
			if cleanupErr != nil {
				release()

				return nil, fmt.Errorf("finish canceled restore: %w", cleanupErr)
			}

			err = a.deleteRestoreJournal(name)
			if err != nil {
				release()

				return nil, err
			}

			journal = *tmux.NewRestoreJournal(name, a.tmux.SessionExists(name))
		default:
			// A running generation from a dead or restarted process: resume it
			// from the last confirmed stage.
			journal.Status = tmux.RestoreStatusRunning
			journal.LastError = ""
		}
	} else {
		journal = *tmux.NewRestoreJournal(name, a.tmux.SessionExists(name))
	}

	err = a.saveRestoreJournal(journal)
	if err != nil {
		release()

		return nil, err
	}

	return &restoreGeneration{journal: journal, release: release}, nil
}

func (a *App) commitGeneration(generation *restoreGeneration) tmux.RestoreJournalCommit {
	return func() error {
		return a.saveRestoreJournal(generation.journal)
	}
}

// driveRestoreGeneration runs snapshot load, idempotent tmux creation and the
// access-time stage under the generation journal. Every later stage is skipped
// when an earlier confirmed stage is already recorded.
func (a *App) driveRestoreGeneration(
	ctx context.Context,
	target PickerTarget,
	generation *restoreGeneration,
) error {
	name := strings.TrimSpace(target.SessionName)

	snap, err := a.store.LoadSession(name)
	if err != nil {
		// Nothing was created: drop the journal so a missing snapshot keeps its
		// original error behavior instead of looking like a half restore.
		if !generation.journal.HasCreatedResources() {
			_ = a.deleteRestoreJournal(name)
		}

		return fmt.Errorf("load session: %w", err)
	}

	err = a.tmux.RestoreSessionJournaled(
		ctx,
		snap,
		&generation.journal,
		a.commitGeneration(generation),
	)
	if err != nil {
		if errors.Is(err, tmux.ErrSessionExists) {
			// Compatibility path: a live session this generation does not own is
			// left untouched and the restore becomes a handoff.
			generation.journal.ExistsAdopted = true
			generation.journal.SessionCreated = false
			generation.journal.Windows = make(map[int]*tmux.WindowJournal)
			_ = a.saveRestoreJournal(generation.journal)
		} else {
			return fmt.Errorf("restore session: %w", err)
		}
	}

	if !generation.journal.Accessed {
		err = restoreContextCanceled(ctx)
		if err != nil {
			return err
		}

		err = a.store.MarkSessionAccessed(name, time.Now().UTC())
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("mark session accessed: %w", err)
		}

		generation.journal.Accessed = true

		err = a.saveRestoreJournal(generation.journal)
		if err != nil {
			return fmt.Errorf("record restore progress: %w", err)
		}
	}

	return nil
}

func restoreContextCanceled(ctx context.Context) error {
	err := ctx.Err()
	if err != nil {
		return fmt.Errorf("restore canceled: %w", err)
	}

	return nil
}

// convergeRestoreCleanup removes the resources recorded for a canceled
// generation. It retries recorded removals one by one; a failure leaves the
// journal in the cleaning state so a later restore or process restart can
// continue converging instead of treating cancellation as complete.
func (a *App) convergeRestoreCleanup(journal *tmux.RestoreJournal) error {
	if journal.Status != tmux.RestoreStatusCleaning {
		journal.Status = tmux.RestoreStatusCanceling
		journal.LastError = ""
		_ = a.saveRestoreJournal(*journal)
	}

	commit := func() error { return a.saveRestoreJournal(*journal) }

	err := a.tmux.CleanupRestoreJournal(journal, commit)
	if err != nil {
		wrappedErr := fmt.Errorf("cleanup restore resources: %w", err)
		journal.Status = tmux.RestoreStatusCleaning
		journal.LastError = err.Error()
		_ = a.saveRestoreJournal(*journal)

		return wrappedErr
	}

	return nil
}

// finishRestoreGeneration records the handoff and removes the journal once the
// generation reached its single conclusion.
func (a *App) finishRestoreGeneration(
	target PickerTarget,
	generation *restoreGeneration,
	allowAttach bool,
) error {
	name := strings.TrimSpace(target.SessionName)
	journal := &generation.journal

	if journal.HandedOff {
		return a.deleteRestoreJournal(name)
	}

	// attach-session replaces the process with tmux on success, so conclude the
	// journal before exec; without a controlling TTY the call is a no-op.
	willAttach := allowAttach && !a.tmux.InsideTmux()
	if willAttach {
		journal.HandedOff = true
		_ = a.deleteRestoreJournal(name)
	}

	err := a.handoffToTarget(target, allowAttach, !journal.PreExisted)
	if err != nil {
		return err
	}

	if !willAttach {
		journal.HandedOff = true
		err = a.deleteRestoreJournal(name)
		if err != nil {
			return err
		}
	}

	return nil
}

// skipRestoreHandoff concludes a generation that was driven without a client
// switch (e.g. restore -switch=false).
func (a *App) skipRestoreHandoff(generation *restoreGeneration) error {
	generation.journal.HandedOff = true

	return a.deleteRestoreJournal(strings.TrimSpace(generation.journal.SessionName))
}

// ReconcileRestoreJournals converges journals left behind by canceled or dead
// processes: cleaning/canceling generations finish their cleanup, running
// generations resume from the last confirmed stage and finish or hand off.
// It is safe to call on process startup.
func (a *App) ReconcileRestoreJournals() error {
	journals, err := a.listRestoreJournals()
	if err != nil {
		return fmt.Errorf("list restore journals: %w", err)
	}

	var reconciled []error

	for _, journal := range journals {
		reconcileErr := a.reconcileRestoreJournal(journal.SessionName)
		if reconcileErr != nil {
			log.Printf(
				"lazy-tmux: reconcile restore %s: %v",
				journal.SessionName,
				reconcileErr,
			)

			reconciled = append(reconciled, reconcileErr)
		}
	}

	return errors.Join(reconciled...)
}

func (a *App) reconcileRestoreJournal(name string) error {
	release, err := a.lockRestore(name)
	if err != nil {
		return fmt.Errorf("lock restore: %w", err)
	}
	defer release()

	journal, ok, err := a.loadRestoreJournal(name)
	if err != nil || !ok {
		return err
	}
	journal.Adopted = true

	generation := &restoreGeneration{journal: journal, release: func() {}}

	switch journal.Status {
	case tmux.RestoreStatusCanceling, tmux.RestoreStatusCleaning:
		err = a.convergeRestoreCleanup(&generation.journal)
		if err != nil {
			return err
		}

		return a.deleteRestoreJournal(name)
	case tmux.RestoreStatusRunning:
		target := PickerTarget{SessionName: name}
		err = a.driveRestoreGeneration(context.Background(), target, generation)
		if err != nil {
			return err
		}

		return a.finishRestoreGeneration(target, generation, false)
	default:
		return nil
	}
}
