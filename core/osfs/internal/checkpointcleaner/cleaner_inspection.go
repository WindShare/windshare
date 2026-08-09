package checkpointcleaner

import (
	"context"
	"path"
	"slices"
	"strings"

	"github.com/windshare/windshare/core/osfs/internal/legacyresume"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

func (run *cleanupRun) inspectRootEntries(
	ctx context.Context,
	report *CheckpointCleanupReport,
) (bool, error) {
	kind, exact, err := run.root.ClassifyExactEntry(legacyresume.ControlDirectory)
	if err != nil {
		return false, err
	}
	controlPresent := kind != outputcap.EntryAbsent
	if controlPresent && (!exact || kind != outputcap.EntryDirectory) {
		addAttention(report, legacyresume.ControlDirectory, cleanupDetailConflict)
		controlPresent = false
	}
	names, err := boundedNames(run.root)
	if err != nil {
		return false, err
	}
	for _, name := range names {
		if err := run.observeEntry(ctx, report); err != nil {
			return false, err
		}
		if strings.EqualFold(name, legacyresume.ControlDirectory) {
			if name != legacyresume.ControlDirectory {
				addAttention(report, name, cleanupDetailConflict)
			}
			continue
		}
		switch {
		case legacyresume.IsBootstrapCandidate(name),
			hasFoldPrefix(name, legacyresume.BootstrapCandidatePrefix),
			hasFoldPrefix(name, legacyLookingRootPrefix):
			addAttention(report, name, cleanupDetailUnknown)
		default:
			addRetained(report, name, cleanupDetailPublished)
		}
	}
	return controlPresent, nil
}

type legacyControlEntryRole uint8

const (
	legacyControlUnknown legacyControlEntryRole = iota
	legacyControlCheckpointNamespace
	legacyControlOwnershipRecord
	legacyControlTemporary
	legacyControlCoordinatorLock
	legacyControlSessionsDirectory
)

type legacyControlEntryClass struct {
	role         legacyControlEntryRole
	expectedKind outputcap.EntryKind
}

func classifyLegacyControlEntry(name string) legacyControlEntryClass {
	switch {
	case name == legacyresume.CheckpointDirectory:
		return legacyControlEntryClass{
			role: legacyControlCheckpointNamespace, expectedKind: outputcap.EntryDirectory,
		}
	case name == legacyresume.ControlRecord:
		return legacyControlEntryClass{
			role: legacyControlOwnershipRecord, expectedKind: outputcap.EntryRegularFile,
		}
	case legacyresume.IsControlTemporary(name):
		return legacyControlEntryClass{
			role: legacyControlTemporary, expectedKind: outputcap.EntryRegularFile,
		}
	case name == legacyresume.CoordinatorLock:
		return legacyControlEntryClass{
			role: legacyControlCoordinatorLock, expectedKind: outputcap.EntryRegularFile,
		}
	case name == legacyresume.SessionsDirectory:
		return legacyControlEntryClass{
			role: legacyControlSessionsDirectory, expectedKind: outputcap.EntryDirectory,
		}
	default:
		return legacyControlEntryClass{}
	}
}

func (run *cleanupRun) inspectControlEntries(
	ctx context.Context,
	report *CheckpointCleanupReport,
) error {
	names, err := boundedNames(run.control)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := run.inspectControlEntry(ctx, name, report); err != nil {
			return err
		}
	}
	slices.Sort(run.legacy.controlTemporary)
	return nil
}

func (run *cleanupRun) inspectControlEntry(
	ctx context.Context,
	name string,
	report *CheckpointCleanupReport,
) error {
	if err := run.observeEntry(ctx, report); err != nil {
		return err
	}
	kind, exact, err := run.control.ClassifyExactEntry(name)
	if err != nil {
		return err
	}
	relative := path.Join(legacyresume.ControlDirectory, name)
	if !exact {
		addAttention(report, relative, cleanupDetailConflict)
		return nil
	}
	classified := classifyLegacyControlEntry(name)
	if classified.role == legacyControlUnknown {
		addAttention(report, relative, cleanupDetailUnknown)
		return nil
	}
	if classified.role == legacyControlCheckpointNamespace {
		if kind != classified.expectedKind {
			addAttention(report, relative, cleanupDetailConflict)
			return nil
		}
		return run.openAndInspectCheckpointNamespace(ctx, report)
	}
	run.recordLegacyControlEntry(classified.role, name)
	if kind != classified.expectedKind {
		addAttention(report, relative, cleanupDetailConflict)
		return nil
	}
	run.approve(relative, kind)
	return nil
}

func (run *cleanupRun) recordLegacyControlEntry(role legacyControlEntryRole, name string) {
	switch role {
	case legacyControlOwnershipRecord:
		run.legacy.controlRecord = true
	case legacyControlTemporary:
		run.legacy.controlTemporary = append(run.legacy.controlTemporary, name)
	case legacyControlCoordinatorLock:
		run.legacy.coordinatorLock = true
	case legacyControlSessionsDirectory:
		run.legacy.sessionsDirectory = true
	}
}

func (run *cleanupRun) openAndInspectCheckpointNamespace(
	ctx context.Context,
	report *CheckpointCleanupReport,
) error {
	namespace, err := run.control.OpenDirectory(legacyresume.CheckpointDirectory, true)
	if err != nil {
		return err
	}
	run.namespace = namespace
	allowed := map[string]outputcap.EntryKind{
		legacyresume.CheckpointOwnership: outputcap.EntryRegularFile,
		legacyresume.CheckpointLeases:    outputcap.EntryDirectory,
		legacyresume.CheckpointIntents:   outputcap.EntryDirectory,
		FileCheckpointCleanupState:       outputcap.EntryRegularFile,
		FileCheckpointCleanupLock:        outputcap.EntryRegularFile,
	}
	names, err := boundedNames(run.namespace)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := run.observeEntry(ctx, report); err != nil {
			return err
		}
		expectedKind, known := allowed[name]
		kind, exact, err := run.namespace.ClassifyExactEntry(name)
		if err != nil {
			return err
		}
		if !known {
			addAttention(report, path.Join(legacyresume.ControlDirectory, legacyresume.CheckpointDirectory, name), cleanupDetailConflict)
			continue
		}
		if !exact || kind != expectedKind {
			addAttention(report, path.Join(legacyresume.ControlDirectory, legacyresume.CheckpointDirectory, name), cleanupDetailConflict)
		}
	}
	return nil
}

func (run *cleanupRun) openExistingCleanerState() (cleanerState, []byte, bool, error) {
	if run.namespace == nil {
		return cleanerState{}, nil, false, nil
	}
	kind, exact, err := run.namespace.ClassifyExactEntry(FileCheckpointCleanupState)
	if err != nil {
		return cleanerState{}, nil, false, err
	}
	if kind == outputcap.EntryAbsent {
		return cleanerState{}, nil, false, nil
	}
	if !exact || kind != outputcap.EntryRegularFile {
		return cleanerState{}, nil, false, ErrCheckpointCleanerState
	}
	if err := run.acquireCleanupLock(); err != nil {
		return cleanerState{}, nil, false, err
	}
	return run.loadState()
}
