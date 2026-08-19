package runtrace

import "github.com/windshare/windshare/cmd/wind/internal/clievent"

func (visitor *encodeVisitorV3) VisitTransferLifecycleObserved(event clievent.TransferLifecycleObserved) error {
	stage, err := nameOf(event.Stage())
	if err != nil {
		return err
	}
	selection, err := nameOf(event.FileSelection())
	if err != nil {
		return err
	}
	fileSettlement, err := nameOf(event.FileSettlement())
	if err != nil {
		return err
	}
	treeSettlement, err := nameOf(event.TreeSettlement())
	if err != nil {
		return err
	}
	progress, err := projectProgress(event.Progress())
	if err != nil {
		return err
	}
	correlation, err := projectSessionCorrelation(
		event.ProtocolSessionID(), clievent.LaneIdentity{}, false,
	)
	if err != nil {
		return err
	}
	payload := transferLifecyclePayloadV3{
		ReceiveOperationID: encodeTypedIdentity(event.ReceiveOperationID().Bytes()),
		TransferJobID:      encodeTypedIdentity(event.TransferJobID().Bytes()),
		Stage:              stage,
		FileSelection:      selection,
		FileSettlement:     fileSettlement,
		TreeSettlement:     treeSettlement,
		Progress:           progress,
	}
	if reason, ok := event.ItemBlock(); ok {
		projected, projectErr := namedPointer(reason)
		if projectErr != nil {
			return projectErr
		}
		payload.ItemBlockReason = projected
	}
	if failure, ok := event.Failure(); ok {
		projected, projectErr := projectFailure(failure)
		if projectErr != nil {
			return projectErr
		}
		payload.Failure = &projected
	}
	visitor.set("transfer_lifecycle", correlation, payload)
	return nil
}

func (visitor *encodeVisitorV3) VisitFilesystemOutputObserved(event clievent.FilesystemOutputObserved) error {
	payload, err := projectFilesystemOutput(event)
	if err != nil {
		return err
	}
	visitor.set("filesystem_output", nil, payload)
	return nil
}

type filesystemOutputIdentitiesV3 struct {
	receiveOperationID  *string
	receiveIntentDigest *string
	outputSessionID     *string
}

type filesystemOutputAuthorityV3 struct {
	certification   *string
	nativeLock      *filesystemNativeLockV3
	rootDisposition *string
}

func projectFilesystemOutput(
	event clievent.FilesystemOutputObserved,
) (filesystemOutputPayloadV3, error) {
	operation, err := nameOf(event.Operation())
	if err != nil {
		return filesystemOutputPayloadV3{}, err
	}
	authority, err := projectFilesystemOutputAuthority(event)
	if err != nil {
		return filesystemOutputPayloadV3{}, err
	}
	runtimeDecision, err := projectFilesystemRuntimeDecision(event)
	if err != nil {
		return filesystemOutputPayloadV3{}, err
	}
	checkpointDecision, hasCheckpointDecision := event.CheckpointDecision()
	projectedCheckpointDecision, err := projectOptionalNamedValue(
		checkpointDecision,
		hasCheckpointDecision,
	)
	if err != nil {
		return filesystemOutputPayloadV3{}, err
	}
	failure, err := projectFilesystemOutputFailure(event)
	if err != nil {
		return filesystemOutputPayloadV3{}, err
	}
	identities := projectFilesystemOutputIdentities(event)
	return filesystemOutputPayloadV3{
		Operation:           operation,
		ReceiveOperationID:  identities.receiveOperationID,
		ReceiveIntentDigest: identities.receiveIntentDigest,
		OutputSessionID:     identities.outputSessionID,
		Certification:       authority.certification,
		NativeLock:          authority.nativeLock,
		RootDisposition:     authority.rootDisposition,
		RuntimeDecision:     runtimeDecision,
		CheckpointDecision:  projectedCheckpointDecision,
		Correlation:         projectFilesystemOutputCorrelation(event),
		Counters:            projectFilesystemCounters(event.Counters()),
		Failure:             failure,
	}, nil
}

func projectFilesystemOutputIdentities(
	event clievent.FilesystemOutputObserved,
) filesystemOutputIdentitiesV3 {
	identities := filesystemOutputIdentitiesV3{}
	if receiveOperation, ok := event.ReceiveOperationID(); ok {
		encoded := encodeTypedIdentity(receiveOperation.Bytes())
		identities.receiveOperationID = &encoded
	}
	if receiveIntent, ok := event.ReceiveIntentDigest(); ok {
		encoded := receiveIntent.Hex()
		identities.receiveIntentDigest = &encoded
	}
	if outputSession, ok := event.OutputSessionID(); ok {
		encoded := encodeTypedIdentity(outputSession.Bytes())
		identities.outputSessionID = &encoded
	}
	return identities
}

func projectFilesystemOutputAuthority(
	event clievent.FilesystemOutputObserved,
) (filesystemOutputAuthorityV3, error) {
	certification, hasCertification := event.Certification()
	projectedCertification, err := projectOptionalNamedValue(certification, hasCertification)
	if err != nil {
		return filesystemOutputAuthorityV3{}, err
	}
	nativeLock, err := projectFilesystemNativeLock(event)
	if err != nil {
		return filesystemOutputAuthorityV3{}, err
	}
	rootDisposition, hasRootDisposition := event.RootDisposition()
	projectedRootDisposition, err := projectOptionalNamedValue(rootDisposition, hasRootDisposition)
	if err != nil {
		return filesystemOutputAuthorityV3{}, err
	}
	return filesystemOutputAuthorityV3{
		certification:   projectedCertification,
		nativeLock:      nativeLock,
		rootDisposition: projectedRootDisposition,
	}, nil
}

func projectOptionalNamedValue(value namedValue, present bool) (*string, error) {
	if !present {
		return nil, nil
	}
	return namedPointer(value)
}

func projectFilesystemNativeLock(
	event clievent.FilesystemOutputObserved,
) (*filesystemNativeLockV3, error) {
	scope, milestone, present := event.NativeLock()
	if !present {
		return nil, nil
	}
	scopeName, err := nameOf(scope)
	if err != nil {
		return nil, err
	}
	milestoneName, err := nameOf(milestone)
	if err != nil {
		return nil, err
	}
	return &filesystemNativeLockV3{
		Scope: scopeName, Milestone: milestoneName,
	}, nil
}

func projectFilesystemRuntimeDecision(
	event clievent.FilesystemOutputObserved,
) (*filesystemRuntimeDecisionV3, error) {
	component, operation, decision, present := event.RuntimeDecision()
	if !present {
		return nil, nil
	}
	componentName, err := nameOf(component)
	if err != nil {
		return nil, err
	}
	operationName, err := nameOf(operation)
	if err != nil {
		return nil, err
	}
	decisionName, err := nameOf(decision)
	if err != nil {
		return nil, err
	}
	return &filesystemRuntimeDecisionV3{
		Component: componentName, Operation: operationName, Decision: decisionName,
	}, nil
}

func projectFilesystemOutputCorrelation(
	event clievent.FilesystemOutputObserved,
) *filesystemCorrelationV3 {
	operationID, claimID := event.Correlation()
	if operationID == 0 && claimID == 0 {
		return nil
	}
	return &filesystemCorrelationV3{
		OperationID: optionalDecimalPointer(operationID),
		ClaimID:     optionalDecimalPointer(claimID),
	}
}

func optionalDecimalPointer(value uint64) *string {
	if value == 0 {
		return nil
	}
	return decimalPointer(value)
}

func projectFilesystemOutputFailure(
	event clievent.FilesystemOutputObserved,
) (*filesystemFailureV3, error) {
	failure, present := event.Failure()
	if !present {
		return nil, nil
	}
	stage, reconciliation, nativeClass, classified := event.FailureClassification()
	if !classified {
		return nil, errInvalidSchemaEvent
	}
	stageName, err := nameOf(stage)
	if err != nil {
		return nil, err
	}
	projectedFailure, err := projectFailure(failure)
	if err != nil {
		return nil, err
	}
	return &filesystemFailureV3{
		Stage:              stageName,
		ReconciliationStep: optionalClosedName(reconciliation),
		NativeErrorClass:   optionalClosedName(nativeClass),
		Failure:            projectedFailure,
	}, nil
}

func optionalClosedName(value namedValue) *string {
	name, present := value.Name()
	if !present {
		return nil
	}
	return &name
}

func projectFilesystemCounters(counters clievent.FilesystemOutputCounters) filesystemCountersV3 {
	return filesystemCountersV3{
		NodeClaims:             decimal(counters.NodeClaims),
		DirectoryClaims:        decimal(counters.DirectoryClaims),
		FileClaims:             decimal(counters.FileClaims),
		ActiveFileClaims:       decimal(counters.ActiveFileClaims),
		ReservedFileSlots:      decimal(counters.ReservedFileSlots),
		DirectoryMetadataBytes: decimal(counters.DirectoryMetadataBytes),
		CheckpointRecords:      decimal(counters.CheckpointRecords),
	}
}
