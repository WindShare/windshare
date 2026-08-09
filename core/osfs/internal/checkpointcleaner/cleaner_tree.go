package checkpointcleaner

import (
	"context"
	"errors"
	"path"

	"github.com/windshare/windshare/core/osfs/internal/legacyresume"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

type legacyDataDirectory uint8

const (
	legacyFiles legacyDataDirectory = iota + 1
	legacyAnchors
	legacyStages
)

func (run *cleanupRun) inspectAndLockLegacySessions(
	ctx context.Context,
	report *CheckpointCleanupReport,
) error {
	sessions, err := run.control.OpenDirectory(legacyresume.SessionsDirectory, true)
	if err != nil {
		return err
	}
	defer sessions.Close()
	names, err := boundedNames(sessions)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := run.observeEntry(ctx, report); err != nil {
			return err
		}
		relative := path.Join(legacyresume.ControlDirectory, legacyresume.SessionsDirectory, name)
		kind, exact, err := sessions.ClassifyExactEntry(name)
		if err != nil {
			return err
		}
		if !legacyresume.IsIntentDirectory(name) {
			addAttention(report, relative, cleanupDetailUnknown)
			continue
		}
		if !exact || kind != outputcap.EntryDirectory {
			addAttention(report, relative, cleanupDetailConflict)
			continue
		}
		run.approve(relative, kind)
		intent, err := sessions.OpenDirectory(name, true)
		if err != nil {
			return err
		}
		inspectErr := run.inspectLegacyIntent(ctx, intent, relative, report)
		if closeErr := intent.Close(); inspectErr != nil || closeErr != nil {
			return errors.Join(inspectErr, closeErr)
		}
	}
	return nil
}

func (run *cleanupRun) inspectLegacyIntent(
	ctx context.Context,
	intent outputcap.Directory,
	relative string,
	report *CheckpointCleanupReport,
) error {
	names, err := boundedNames(intent)
	if err != nil {
		return err
	}
	sessionCount := 0
	for _, name := range names {
		if err := run.observeEntry(ctx, report); err != nil {
			return err
		}
		entryPath := path.Join(relative, name)
		kind, exact, err := intent.ClassifyExactEntry(name)
		if err != nil {
			return err
		}
		if name == legacyresume.CheckpointDirectory {
			// A nested retired checkpoint root has an independent ownership marker.
			// This session cleanup cannot inherit authority to delete it from its path.
			if !exact || kind != outputcap.EntryDirectory {
				addAttention(report, entryPath, cleanupDetailConflict)
			} else {
				addAttention(report, entryPath, cleanupDetailSeparateOwnership)
			}
			continue
		}
		candidate := legacyresume.IsSessionCandidate(name)
		if !candidate && !legacyresume.IsSessionDirectory(name) {
			addAttention(report, entryPath, cleanupDetailUnknown)
			continue
		}
		sessionCount++
		if sessionCount > 1 {
			addAttention(report, entryPath, cleanupDetailConflict)
			continue
		}
		if !exact || kind != outputcap.EntryDirectory {
			addAttention(report, entryPath, cleanupDetailConflict)
			continue
		}
		run.approve(entryPath, kind)
		session, err := intent.OpenDirectory(name, true)
		if err != nil {
			return err
		}
		inspectErr := run.inspectLegacySession(ctx, session, entryPath, candidate, report)
		if closeErr := session.Close(); inspectErr != nil || closeErr != nil {
			return errors.Join(inspectErr, closeErr)
		}
	}
	return nil
}

type legacySessionEntryRole uint8

const (
	legacySessionEntryUnknown legacySessionEntryRole = iota
	legacySessionEntryLock
	legacySessionEntryRecord
	legacySessionEntryDataDirectory
)

type legacySessionEntryClass struct {
	role          legacySessionEntryRole
	dataDirectory legacyDataDirectory
}

func classifyLegacySessionEntry(name string) legacySessionEntryClass {
	switch {
	case name == legacyresume.SessionLock:
		return legacySessionEntryClass{role: legacySessionEntryLock}
	case name == legacyresume.HeaderRecord || legacyresume.IsHeaderTemporary(name):
		return legacySessionEntryClass{role: legacySessionEntryRecord}
	case name == legacyresume.FilesDirectory:
		return legacySessionEntryClass{
			role: legacySessionEntryDataDirectory, dataDirectory: legacyFiles,
		}
	case name == legacyresume.AnchorsDirectory:
		return legacySessionEntryClass{
			role: legacySessionEntryDataDirectory, dataDirectory: legacyAnchors,
		}
	case name == legacyresume.StagesDirectory:
		return legacySessionEntryClass{
			role: legacySessionEntryDataDirectory, dataDirectory: legacyStages,
		}
	default:
		return legacySessionEntryClass{}
	}
}

func (run *cleanupRun) inspectLegacySession(
	ctx context.Context,
	session outputcap.Directory,
	relative string,
	candidate bool,
	report *CheckpointCleanupReport,
) error {
	names, err := boundedNames(session)
	if err != nil {
		return err
	}
	lockPresent := false
	for _, name := range names {
		entryHasLock, err := run.inspectLegacySessionEntry(ctx, session, relative, name, report)
		if err != nil {
			return err
		}
		lockPresent = lockPresent || entryHasLock
	}
	// An old writer could crash after installing a header but before its lock.
	// This cleaner intentionally cannot decode that header, so only an empty
	// candidate is safe without a session lock; non-empty partials need attention.
	if !lockPresent && (!candidate || len(names) != 0) {
		addAttention(report, path.Join(relative, legacyresume.SessionLock), cleanupDetailConflict)
	}
	return nil
}

func (run *cleanupRun) inspectLegacySessionEntry(
	ctx context.Context,
	session outputcap.Directory,
	relative string,
	name string,
	report *CheckpointCleanupReport,
) (bool, error) {
	if err := run.observeEntry(ctx, report); err != nil {
		return false, err
	}
	kind, exact, err := session.ClassifyExactEntry(name)
	if err != nil {
		return false, err
	}
	entryPath := path.Join(relative, name)
	if !exact {
		addAttention(report, entryPath, cleanupDetailConflict)
		return false, nil
	}
	classified := classifyLegacySessionEntry(name)
	switch classified.role {
	case legacySessionEntryLock:
		return true, run.inspectLegacySessionLock(session, name, entryPath, kind, report)
	case legacySessionEntryRecord:
		run.inspectLegacySessionRecord(entryPath, kind, report)
		return false, nil
	case legacySessionEntryDataDirectory:
		return false, run.inspectLegacyDataDirectory(
			ctx,
			session,
			name,
			entryPath,
			kind,
			classified.dataDirectory,
			report,
		)
	default:
		addAttention(report, entryPath, cleanupDetailUnknown)
		return false, nil
	}
}

func (run *cleanupRun) inspectLegacySessionLock(
	session outputcap.Directory,
	name string,
	entryPath string,
	kind outputcap.EntryKind,
	report *CheckpointCleanupReport,
) error {
	if kind != outputcap.EntryRegularFile {
		addAttention(report, entryPath, cleanupDetailConflict)
		return nil
	}
	run.approve(entryPath, kind)
	return run.acquireExistingSessionLock(session, name, entryPath)
}

func (run *cleanupRun) inspectLegacySessionRecord(
	entryPath string,
	kind outputcap.EntryKind,
	report *CheckpointCleanupReport,
) {
	if kind != outputcap.EntryRegularFile {
		addAttention(report, entryPath, cleanupDetailConflict)
		return
	}
	run.approve(entryPath, kind)
}

func (run *cleanupRun) inspectLegacyDataDirectory(
	ctx context.Context,
	session outputcap.Directory,
	name string,
	relative string,
	kind outputcap.EntryKind,
	directoryKind legacyDataDirectory,
	report *CheckpointCleanupReport,
) error {
	if kind != outputcap.EntryDirectory {
		addAttention(report, relative, cleanupDetailConflict)
		return nil
	}
	run.approve(relative, kind)
	directory, err := session.OpenDirectory(name, true)
	if err != nil {
		return err
	}
	defer directory.Close()
	shards, err := boundedNames(directory)
	if err != nil {
		return err
	}
	for _, shardName := range shards {
		if err := run.observeEntry(ctx, report); err != nil {
			return err
		}
		shardPath := path.Join(relative, shardName)
		kind, exact, err := directory.ClassifyExactEntry(shardName)
		if err != nil {
			return err
		}
		if !legacyresume.IsShard(shardName) {
			addAttention(report, shardPath, cleanupDetailUnknown)
			continue
		}
		if !exact || kind != outputcap.EntryDirectory {
			addAttention(report, shardPath, cleanupDetailConflict)
			continue
		}
		run.approve(shardPath, kind)
		shard, err := directory.OpenDirectory(shardName, true)
		if err != nil {
			return err
		}
		inspectErr := run.inspectLegacyShard(ctx, shard, shardName, shardPath, directoryKind, report)
		if closeErr := shard.Close(); inspectErr != nil || closeErr != nil {
			return errors.Join(inspectErr, closeErr)
		}
	}
	return nil
}

func (run *cleanupRun) inspectLegacyShard(
	ctx context.Context,
	shard outputcap.Directory,
	shardName string,
	relative string,
	directoryKind legacyDataDirectory,
	report *CheckpointCleanupReport,
) error {
	names, err := boundedNames(shard)
	if err != nil {
		return err
	}
	for _, name := range names {
		if err := run.observeEntry(ctx, report); err != nil {
			return err
		}
		entryPath := path.Join(relative, name)
		kind, exact, err := shard.ClassifyExactEntry(name)
		if err != nil {
			return err
		}
		known := false
		switch directoryKind {
		case legacyFiles:
			known = legacyresume.IsFileRecord(shardName, name) || legacyresume.IsFileRecordTemporary(shardName, name)
		case legacyAnchors:
			known = legacyresume.IsAnchor(shardName, name)
		case legacyStages:
			known = legacyresume.IsStage(shardName, name)
		}
		if !known {
			addAttention(report, entryPath, cleanupDetailUnknown)
			continue
		}
		if !exact || kind != outputcap.EntryRegularFile {
			addAttention(report, entryPath, cleanupDetailConflict)
			continue
		}
		run.approve(entryPath, kind)
	}
	return nil
}

func (run *cleanupRun) acquireExistingSessionLock(
	directory outputcap.Directory,
	name string,
	entryPath string,
) error {
	lock, created, err := directory.AcquireLock(name, true)
	if errors.Is(err, outputcap.ErrNamespaceLockBusy) {
		return ErrCheckpointCleanerBusy
	}
	if err != nil {
		return err
	}
	if created || lock == nil || lock.File() == nil {
		return errors.Join(ErrCheckpointCleanerOwnership, closeLock(lock))
	}
	size, sizeErr := lock.File().Size()
	if sizeErr != nil || size != 0 {
		return errors.Join(ErrCheckpointCleanerOwnership, sizeErr, lock.Close())
	}
	parent, err := directory.Duplicate()
	if err != nil {
		return errors.Join(err, lock.Close())
	}
	run.sessionLocks = append(run.sessionLocks, cleanupLockRef{
		parent: parent, name: name, path: entryPath, lock: lock,
	})
	return nil
}

func (run *cleanupRun) observeEntry(ctx context.Context, report *CheckpointCleanupReport) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	run.observed++
	if run.observed > maxCleanerEntries {
		return ErrCheckpointCleanerLimit
	}
	report.Scanned++
	return nil
}
