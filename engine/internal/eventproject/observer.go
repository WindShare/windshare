package eventproject

import (
	"github.com/windshare/windshare/core/liveshare"
	"github.com/windshare/windshare/core/session/sessionruntime"
	appobs "github.com/windshare/windshare/engine/observation"
)

func ProjectSharingSubject(summary liveshare.SelectedRootSummary) (appobs.SharingSubjectSelected, error) {
	count := summary.SelectedCount()
	if count == 0 {
		return appobs.SharingSubjectSelected{}, ErrInvalidProjection
	}
	var subject appobs.SharingSubject
	var err error
	if count > 1 {
		subject, err = appobs.NewMultipleSubject(count)
	} else {
		root, ok := summary.SingleRoot()
		if !ok || root.Name() == "" {
			return appobs.SharingSubjectSelected{}, ErrInvalidProjection
		}
		name := appobs.NewDisplayName(root.Name())
		switch root.Kind() {
		case liveshare.SelectedRootKindFile:
			size, ok := root.FileSize()
			if !ok {
				return appobs.SharingSubjectSelected{}, ErrInvalidProjection
			}
			subject, err = appobs.NewFileSubject(name, size)
		case liveshare.SelectedRootKindDirectory:
			if _, ok := root.FileSize(); ok {
				return appobs.SharingSubjectSelected{}, ErrInvalidProjection
			}
			subject, err = appobs.NewDirectorySubject(name)
		default:
			return appobs.SharingSubjectSelected{}, ErrInvalidProjection
		}
	}
	if err != nil {
		return appobs.SharingSubjectSelected{}, ErrInvalidProjection
	}
	event, err := appobs.NewSharingSubjectSelected(subject)
	if err != nil {
		return appobs.SharingSubjectSelected{}, ErrInvalidProjection
	}
	return event, nil
}

func ProjectSenderTerminalSend(
	value sessionruntime.SenderTerminalSendObserved,
) (appobs.SenderTerminalSendObserved, error) {
	session, err := ProtocolSessionID(value.ProtocolSessionID)
	if err != nil {
		return appobs.SenderTerminalSendObserved{}, ErrInvalidProjection
	}
	lane, err := LaneIdentity(value.Lane)
	if err != nil {
		return appobs.SenderTerminalSendObserved{}, ErrInvalidProjection
	}
	transport, ok := projectSenderTerminalSendTransport(value.TransportDisposition)
	if !ok {
		return appobs.SenderTerminalSendObserved{}, ErrInvalidProjection
	}
	outcome, ok := projectSenderTerminalSendOutcome(value.Outcome)
	if !ok {
		return appobs.SenderTerminalSendObserved{}, ErrInvalidProjection
	}
	decision, ok := projectSenderTerminalSendDecision(value.Decision)
	if !ok {
		return appobs.SenderTerminalSendObserved{}, ErrInvalidProjection
	}
	event, err := appobs.NewSenderTerminalSendObserved(
		session, lane, value.Settled, transport, outcome, decision,
	)
	if err != nil {
		return appobs.SenderTerminalSendObserved{}, ErrInvalidProjection
	}
	return event, nil
}

func ProjectSenderSessionTerminated(
	value sessionruntime.SenderSessionTerminated,
) (appobs.SenderSessionTerminated, error) {
	session, err := ProtocolSessionID(value.ProtocolSessionID)
	if err != nil {
		return appobs.SenderSessionTerminated{}, ErrInvalidProjection
	}
	trigger, ok := projectSenderSessionTerminalTrigger(value.Trigger)
	if !ok {
		return appobs.SenderSessionTerminated{}, ErrInvalidProjection
	}
	provenance, ok := projectSenderSessionTerminalProvenance(value.Provenance)
	if !ok {
		return appobs.SenderSessionTerminated{}, ErrInvalidProjection
	}
	event, err := appobs.NewSenderSessionTerminated(session, trigger, provenance, value.Failure)
	if err != nil {
		return appobs.SenderSessionTerminated{}, ErrInvalidProjection
	}
	return event, nil
}

func ProjectCatalogStorage(value liveshare.CatalogStorageTrace) (appobs.CatalogStorageObserved, error) {
	operation, ok := projectCatalogStorageOperation(value.Operation)
	if !ok {
		return appobs.CatalogStorageObserved{}, ErrInvalidProjection
	}
	cause, ok := projectCatalogStorageCause(value.Cause)
	if !ok {
		return appobs.CatalogStorageObserved{}, ErrInvalidProjection
	}
	event, err := appobs.NewCatalogStorageObserved(
		operation,
		cause,
		appobs.CatalogUsage{
			ActiveScans: value.RecoveredUsage.ActiveScans, ScanWork: value.RecoveredUsage.ScanWork,
			Entries: value.RecoveredUsage.Entries, MemoryBytes: value.RecoveredUsage.MemoryBytes,
			SpillBytes: value.RecoveredUsage.SpillBytes,
		},
		value.LegacyRootsRemoved,
	)
	if err != nil {
		return appobs.CatalogStorageObserved{}, ErrInvalidProjection
	}
	return event, nil
}

func ProjectRootPrefetch(value liveshare.RootPrefetchTrace) (appobs.RootPrefetchObserved, error) {
	decision, ok := projectRootPrefetchDecision(value.Decision)
	if !ok {
		return appobs.RootPrefetchObserved{}, ErrInvalidProjection
	}
	event, err := appobs.NewRootPrefetchObserved(
		decision, value.Attempt, value.EntryCount, value.OmittedCount,
	)
	if err != nil {
		return appobs.RootPrefetchObserved{}, ErrInvalidProjection
	}
	return event, nil
}
