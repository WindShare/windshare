package commandprojection

import (
	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

func ProjectSharingSubject(summary liveshare.SelectedRootSummary) (clievent.SharingSubjectSelected, error) {
	count := summary.SelectedCount()
	if count == 0 {
		return clievent.SharingSubjectSelected{}, ErrInvalidProjection
	}
	var subject clievent.SharingSubject
	var err error
	if count > 1 {
		subject, err = clievent.NewMultipleSubject(count)
	} else {
		root, ok := summary.SingleRoot()
		if !ok || root.Name() == "" {
			return clievent.SharingSubjectSelected{}, ErrInvalidProjection
		}
		name := clievent.NewDisplayName(root.Name())
		switch root.Kind() {
		case liveshare.SelectedRootKindFile:
			size, ok := root.FileSize()
			if !ok {
				return clievent.SharingSubjectSelected{}, ErrInvalidProjection
			}
			subject, err = clievent.NewFileSubject(name, size)
		case liveshare.SelectedRootKindDirectory:
			if _, ok := root.FileSize(); ok {
				return clievent.SharingSubjectSelected{}, ErrInvalidProjection
			}
			subject, err = clievent.NewDirectorySubject(name)
		default:
			return clievent.SharingSubjectSelected{}, ErrInvalidProjection
		}
	}
	if err != nil {
		return clievent.SharingSubjectSelected{}, ErrInvalidProjection
	}
	event, err := clievent.NewSharingSubjectSelected(subject)
	if err != nil {
		return clievent.SharingSubjectSelected{}, ErrInvalidProjection
	}
	return event, nil
}

func ProjectSenderTerminalSend(
	value sessionruntime.SenderTerminalSendObserved,
) (clievent.SenderTerminalSendObserved, error) {
	session, err := ProtocolSessionID(value.ProtocolSessionID)
	if err != nil {
		return clievent.SenderTerminalSendObserved{}, ErrInvalidProjection
	}
	lane, err := LaneIdentity(value.Lane)
	if err != nil {
		return clievent.SenderTerminalSendObserved{}, ErrInvalidProjection
	}
	transport, ok := projectSenderTerminalSendTransport(value.TransportDisposition)
	if !ok {
		return clievent.SenderTerminalSendObserved{}, ErrInvalidProjection
	}
	outcome, ok := projectSenderTerminalSendOutcome(value.Outcome)
	if !ok {
		return clievent.SenderTerminalSendObserved{}, ErrInvalidProjection
	}
	decision, ok := projectSenderTerminalSendDecision(value.Decision)
	if !ok {
		return clievent.SenderTerminalSendObserved{}, ErrInvalidProjection
	}
	event, err := clievent.NewSenderTerminalSendObserved(
		session, lane, value.Settled, transport, outcome, decision,
	)
	if err != nil {
		return clievent.SenderTerminalSendObserved{}, ErrInvalidProjection
	}
	return event, nil
}

func ProjectSenderSessionTerminated(
	value sessionruntime.SenderSessionTerminated,
) (clievent.SenderSessionTerminated, error) {
	session, err := ProtocolSessionID(value.ProtocolSessionID)
	if err != nil {
		return clievent.SenderSessionTerminated{}, ErrInvalidProjection
	}
	trigger, ok := projectSenderSessionTerminalTrigger(value.Trigger)
	if !ok {
		return clievent.SenderSessionTerminated{}, ErrInvalidProjection
	}
	provenance, ok := projectSenderSessionTerminalProvenance(value.Provenance)
	if !ok {
		return clievent.SenderSessionTerminated{}, ErrInvalidProjection
	}
	event, err := clievent.NewSenderSessionTerminated(session, trigger, provenance)
	if err != nil {
		return clievent.SenderSessionTerminated{}, ErrInvalidProjection
	}
	return event, nil
}

func ProjectCatalogStorage(value liveshare.CatalogStorageTrace) (clievent.CatalogStorageObserved, error) {
	operation, ok := projectCatalogStorageOperation(value.Operation)
	if !ok {
		return clievent.CatalogStorageObserved{}, ErrInvalidProjection
	}
	cause, ok := projectCatalogStorageCause(value.Cause)
	if !ok {
		return clievent.CatalogStorageObserved{}, ErrInvalidProjection
	}
	event, err := clievent.NewCatalogStorageObserved(
		operation,
		cause,
		clievent.CatalogUsage{
			ActiveScans: value.RecoveredUsage.ActiveScans, ScanWork: value.RecoveredUsage.ScanWork,
			Entries: value.RecoveredUsage.Entries, MemoryBytes: value.RecoveredUsage.MemoryBytes,
			SpillBytes: value.RecoveredUsage.SpillBytes,
		},
		value.LegacyRootsRemoved,
	)
	if err != nil {
		return clievent.CatalogStorageObserved{}, ErrInvalidProjection
	}
	return event, nil
}

func ProjectRootPrefetch(value liveshare.RootPrefetchTrace) (clievent.RootPrefetchObserved, error) {
	decision, ok := projectRootPrefetchDecision(value.Decision)
	if !ok {
		return clievent.RootPrefetchObserved{}, ErrInvalidProjection
	}
	event, err := clievent.NewRootPrefetchObserved(
		decision, value.Attempt, value.EntryCount, value.OmittedCount,
	)
	if err != nil {
		return clievent.RootPrefetchObserved{}, ErrInvalidProjection
	}
	return event, nil
}
