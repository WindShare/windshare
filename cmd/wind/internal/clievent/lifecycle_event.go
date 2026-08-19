package clievent

type RelayLifecycleSpec struct {
	Command          Command
	LinkID           uint64
	RelaySession     RelaySessionID
	SendOperationID  uint64
	Stage            RelayLifecycleStage
	Terminal         bool
	Disposition      SendDisposition
	RetirementSource RelayRetirementSource
	Cause            RelayLifecycleCause
	DrainCause       RelayLifecycleCause
	Dropped          uint64
}

type RelayLifecycleObserved struct{ spec RelayLifecycleSpec }

func NewRelayLifecycleObserved(spec RelayLifecycleSpec) (RelayLifecycleObserved, error) {
	if !validRelayLifecycleSpec(spec) {
		return RelayLifecycleObserved{}, ErrInvalidEvent
	}
	return RelayLifecycleObserved{spec: spec}, nil
}

func validRelayLifecycleSpec(spec RelayLifecycleSpec) bool {
	_, stageOK := spec.Stage.Name()
	_, retirementOK := spec.RetirementSource.Name()
	_, causeOK := spec.Cause.Name()
	_, drainOK := spec.DrainCause.Name()
	_, dispositionOK := spec.Disposition.Name()
	// Source packages own stage-aware shape validation. The event layer validates
	// only its closed vocabulary so it cannot become a second, narrower producer
	// matrix as lifecycle semantics evolve.
	return spec.Command.Valid() && spec.LinkID != 0 && stageOK && retirementOK && causeOK && drainOK &&
		(spec.Disposition == 0 || dispositionOK)
}

func (RelayLifecycleObserved) event()                 {}
func (value RelayLifecycleObserved) Command() Command { return value.spec.Command }
func (RelayLifecycleObserved) Level() Level           { return LevelDebug }
func (value RelayLifecycleObserved) LinkID() uint64   { return value.spec.LinkID }
func (value RelayLifecycleObserved) RelaySessionID() (RelaySessionID, bool) {
	return value.spec.RelaySession, value.spec.RelaySession.Valid()
}
func (value RelayLifecycleObserved) SendOperationID() uint64 {
	return value.spec.SendOperationID
}
func (value RelayLifecycleObserved) Stage() RelayLifecycleStage { return value.spec.Stage }
func (value RelayLifecycleObserved) Terminal() bool             { return value.spec.Terminal }
func (value RelayLifecycleObserved) Disposition() (SendDisposition, bool) {
	_, ok := value.spec.Disposition.Name()
	return value.spec.Disposition, ok
}
func (value RelayLifecycleObserved) RetirementSource() RelayRetirementSource {
	return value.spec.RetirementSource
}
func (value RelayLifecycleObserved) Cause() RelayLifecycleCause { return value.spec.Cause }
func (value RelayLifecycleObserved) DrainCause() RelayLifecycleCause {
	return value.spec.DrainCause
}
func (value RelayLifecycleObserved) Dropped() uint64 { return value.spec.Dropped }
func (value RelayLifecycleObserved) Accept(visitor Visitor) error {
	return acceptRelayLifecycleObserved(visitor, value)
}

type WebRTCLifecycleSpec struct {
	Command         Command
	ChannelID       uint64
	SendOperationID uint64
	Operation       WebRTCOperation
	Transition      WebRTCTransition
	Disposition     SendDisposition
	State           ChannelState
	Terminal        WebRTCTerminalState
	Cause           WebRTCLifecycleCause
	Dropped         uint64
}

type WebRTCLifecycleObserved struct{ spec WebRTCLifecycleSpec }

func NewWebRTCLifecycleObserved(spec WebRTCLifecycleSpec) (WebRTCLifecycleObserved, error) {
	if !validWebRTCLifecycleSpec(spec) {
		return WebRTCLifecycleObserved{}, ErrInvalidEvent
	}
	return WebRTCLifecycleObserved{spec: spec}, nil
}

func validWebRTCLifecycleSpec(spec WebRTCLifecycleSpec) bool {
	_, operationOK := spec.Operation.Name()
	_, transitionOK := spec.Transition.Name()
	_, dispositionOK := spec.Disposition.Name()
	_, stateOK := spec.State.Name()
	_, terminalOK := spec.Terminal.Name()
	_, causeOK := spec.Cause.Name()
	return spec.Command.Valid() && spec.ChannelID != 0 && operationOK && transitionOK && stateOK && terminalOK && causeOK &&
		(spec.Disposition == 0 || dispositionOK)
}

func (WebRTCLifecycleObserved) event()                  {}
func (value WebRTCLifecycleObserved) Command() Command  { return value.spec.Command }
func (WebRTCLifecycleObserved) Level() Level            { return LevelDebug }
func (value WebRTCLifecycleObserved) ChannelID() uint64 { return value.spec.ChannelID }
func (value WebRTCLifecycleObserved) SendOperationID() uint64 {
	return value.spec.SendOperationID
}
func (value WebRTCLifecycleObserved) Operation() WebRTCOperation   { return value.spec.Operation }
func (value WebRTCLifecycleObserved) Transition() WebRTCTransition { return value.spec.Transition }
func (value WebRTCLifecycleObserved) Disposition() (SendDisposition, bool) {
	_, ok := value.spec.Disposition.Name()
	return value.spec.Disposition, ok
}
func (value WebRTCLifecycleObserved) State() ChannelState           { return value.spec.State }
func (value WebRTCLifecycleObserved) Terminal() WebRTCTerminalState { return value.spec.Terminal }
func (value WebRTCLifecycleObserved) Cause() WebRTCLifecycleCause   { return value.spec.Cause }
func (value WebRTCLifecycleObserved) Dropped() uint64               { return value.spec.Dropped }
func (value WebRTCLifecycleObserved) Accept(visitor Visitor) error {
	return acceptWebRTCLifecycleObserved(visitor, value)
}

type CandidateCounts struct {
	LocalEmitted   uint32
	RemoteAccepted uint32
}

type PeerAttemptSpec struct {
	Command                   Command
	Session                   ProtocolSessionID
	PeerPath                  PeerPathID
	Attempt                   PeerAttemptID
	OfferOperation            ProtocolOperationID
	HasOfferOperation         bool
	Sequence                  uint64
	ElapsedMillis             uint64
	Stage                     PeerAttemptStage
	Phase                     PeerAttemptPhase
	DeadlineMillis            uint64
	Candidates                CandidateCounts
	HasCandidates             bool
	GrantOperation            ProtocolOperationID
	HasGrantOperation         bool
	Lane                      LaneIdentity
	HasLane                   bool
	AdmissionDisposition      PeerAdmissionDisposition
	ResponseDelivery          PeerResponseDelivery
	RejectionCode             PeerLaneRejectionCode
	RejectionRetryAfterMillis uint64
	FailedAtStage             PeerAttemptStage
	FailureScope              PeerFailureScope
	Failure                   Failure
}

type PeerAttemptObserved struct{ spec PeerAttemptSpec }

func NewPeerAttemptObserved(spec PeerAttemptSpec) (PeerAttemptObserved, error) {
	if !validPeerAttemptSpec(spec) {
		return PeerAttemptObserved{}, ErrInvalidEvent
	}
	return PeerAttemptObserved{spec: spec}, nil
}

func validPeerAttemptSpec(spec PeerAttemptSpec) bool {
	_, stageOK := spec.Stage.Name()
	_, scopeOK := spec.FailureScope.Name()
	_, phaseOK := spec.Phase.Name()
	_, admissionOK := spec.AdmissionDisposition.Name()
	_, deliveryOK := spec.ResponseDelivery.Name()
	_, rejectionOK := spec.RejectionCode.Name()
	_, failedAtOK := spec.FailedAtStage.Name()
	hasFailure := spec.Failure.Valid()
	deadlineStage := spec.Stage == PeerNegotiationDeadlineArmed ||
		spec.Stage == PeerNegotiationDeadlineExpired || spec.Stage == PeerAdmissionDeadlineArmed ||
		spec.Stage == PeerAdmissionDeadlineExpired
	negotiationPhaseStage := spec.Stage == PeerNegotiationDeadlineArmed ||
		spec.Stage == PeerNegotiationDeadlineExpired
	admissionPhaseStage := spec.Stage == PeerAdmissionDeadlineArmed ||
		spec.Stage == PeerAdmissionDeadlineExpired || spec.Stage == PeerLaneHelloAuthenticated ||
		spec.Stage == PeerAdmissionResponseSettled || spec.Stage == PeerAttemptAdmitted
	phaseRequired := negotiationPhaseStage || admissionPhaseStage
	grantRequired := spec.Stage == PeerLaneHelloAuthenticated ||
		spec.Stage == PeerAdmissionResponseSettled || spec.Stage == PeerAttemptAdmitted
	grantAllowed := grantRequired || spec.Stage == PeerAdmissionDeadlineExpired ||
		spec.Stage == PeerAttemptFailed
	responseStage := spec.Stage == PeerAdmissionResponseSettled || spec.Stage == PeerAttemptAdmitted
	rejectionStage := responseStage && spec.AdmissionDisposition == PeerAdmissionRejected
	return spec.Command.Valid() && spec.Session.Valid() && spec.PeerPath.Valid() && spec.Attempt.Valid() &&
		spec.Sequence != 0 && stageOK &&
		(spec.Stage == PeerAttemptFailed) == hasFailure &&
		(spec.Stage == PeerAttemptFailed) == failedAtOK &&
		(!failedAtOK || spec.FailedAtStage != PeerAttemptStarted && spec.FailedAtStage != PeerAttemptFailed) &&
		(spec.Stage == PeerAttemptFailed) == scopeOK &&
		spec.HasOfferOperation == spec.OfferOperation.Valid() &&
		phaseRequired == phaseOK &&
		(!negotiationPhaseStage || spec.Phase == PeerPhaseNegotiation) &&
		(!admissionPhaseStage || spec.Phase == PeerPhaseAdmission) &&
		deadlineStage == (spec.DeadlineMillis != 0) &&
		spec.HasGrantOperation == spec.GrantOperation.Valid() &&
		(!grantRequired || spec.HasGrantOperation) && (!spec.HasGrantOperation || grantAllowed) &&
		(!spec.HasLane || spec.Lane.Valid()) &&
		(!grantRequired || spec.HasLane) && (!spec.HasLane || grantAllowed) &&
		responseStage == admissionOK && responseStage == deliveryOK &&
		rejectionStage == rejectionOK &&
		(spec.Stage != PeerAttemptAdmitted ||
			spec.AdmissionDisposition == PeerAdmissionAccepted && spec.ResponseDelivery == PeerResponseDelivered) &&
		(spec.RejectionRetryAfterMillis == 0 || spec.RejectionCode == PeerLaneRejectAdmissionLimited)
}

func (PeerAttemptObserved) event()                                     {}
func (value PeerAttemptObserved) Command() Command                     { return value.spec.Command }
func (PeerAttemptObserved) Level() Level                               { return LevelDebug }
func (value PeerAttemptObserved) ProtocolSessionID() ProtocolSessionID { return value.spec.Session }
func (value PeerAttemptObserved) PeerPathID() PeerPathID               { return value.spec.PeerPath }
func (value PeerAttemptObserved) PeerAttemptID() PeerAttemptID         { return value.spec.Attempt }
func (value PeerAttemptObserved) OfferOperationID() (ProtocolOperationID, bool) {
	return value.spec.OfferOperation, value.spec.HasOfferOperation
}
func (value PeerAttemptObserved) Sequence() uint64        { return value.spec.Sequence }
func (value PeerAttemptObserved) ElapsedMillis() uint64   { return value.spec.ElapsedMillis }
func (value PeerAttemptObserved) Stage() PeerAttemptStage { return value.spec.Stage }
func (value PeerAttemptObserved) PhaseDeadline() (PeerAttemptPhase, uint64, bool) {
	_, ok := value.spec.Phase.Name()
	return value.spec.Phase, value.spec.DeadlineMillis, ok
}
func (value PeerAttemptObserved) Candidates() (CandidateCounts, bool) {
	return value.spec.Candidates, value.spec.HasCandidates
}
func (value PeerAttemptObserved) Lane() (LaneIdentity, bool) {
	return value.spec.Lane, value.spec.HasLane
}
func (value PeerAttemptObserved) GrantOperationID() (ProtocolOperationID, bool) {
	return value.spec.GrantOperation, value.spec.HasGrantOperation
}
func (value PeerAttemptObserved) Admission() (PeerAdmissionDisposition, PeerResponseDelivery, bool) {
	_, admissionOK := value.spec.AdmissionDisposition.Name()
	_, deliveryOK := value.spec.ResponseDelivery.Name()
	return value.spec.AdmissionDisposition, value.spec.ResponseDelivery, admissionOK && deliveryOK
}
func (value PeerAttemptObserved) Rejection() (PeerLaneRejectionCode, uint64, bool) {
	_, ok := value.spec.RejectionCode.Name()
	return value.spec.RejectionCode, value.spec.RejectionRetryAfterMillis, ok
}
func (value PeerAttemptObserved) Failure() (PeerFailureScope, Failure, bool) {
	return value.spec.FailureScope, value.spec.Failure, value.spec.Failure.Valid()
}
func (value PeerAttemptObserved) FailedAtStage() (PeerAttemptStage, bool) {
	_, ok := value.spec.FailedAtStage.Name()
	return value.spec.FailedAtStage, ok
}
func (value PeerAttemptObserved) Accept(visitor Visitor) error {
	return acceptPeerAttemptObserved(visitor, value)
}

type TransferLifecycleSpec struct {
	ReceiveOperation ReceiveOperationID
	ProtocolSession  ProtocolSessionID
	TransferJob      TransferJobID
	Stage            TransferLifecycleStage
	Progress         ProgressSnapshot
	FileSelection    FileSelectionDecision
	FileSettlement   FileSettlement
	ItemBlockReason  ItemBlockReason
	TreeSettlement   TreeSettlement
	Failure          Failure
}

type TransferLifecycleObserved struct{ spec TransferLifecycleSpec }

func NewTransferLifecycleObserved(spec TransferLifecycleSpec) (TransferLifecycleObserved, error) {
	if !validTransferLifecycleSpec(spec) {
		return TransferLifecycleObserved{}, ErrInvalidEvent
	}
	return TransferLifecycleObserved{spec: spec}, nil
}

func validTransferLifecycleSpec(spec TransferLifecycleSpec) bool {
	_, stageOK := spec.Stage.Name()
	_, selectionOK := spec.FileSelection.Name()
	_, fileSettlementOK := spec.FileSettlement.Name()
	_, itemBlockReasonOK := spec.ItemBlockReason.Name()
	_, treeSettlementOK := spec.TreeSettlement.Name()
	return spec.ReceiveOperation.Valid() && spec.ProtocolSession.Valid() && spec.TransferJob.Valid() &&
		stageOK && spec.Progress.Valid() && selectionOK && fileSettlementOK && itemBlockReasonOK &&
		treeSettlementOK && (spec.ItemBlockReason == ItemBlockNone || spec.FileSettlement == FileItemBlocked)
}

func (TransferLifecycleObserved) event()           {}
func (TransferLifecycleObserved) Command() Command { return CommandGet }
func (TransferLifecycleObserved) Level() Level     { return LevelDebug }
func (value TransferLifecycleObserved) ReceiveOperationID() ReceiveOperationID {
	return value.spec.ReceiveOperation
}
func (value TransferLifecycleObserved) ProtocolSessionID() ProtocolSessionID {
	return value.spec.ProtocolSession
}
func (value TransferLifecycleObserved) TransferJobID() TransferJobID  { return value.spec.TransferJob }
func (value TransferLifecycleObserved) Stage() TransferLifecycleStage { return value.spec.Stage }
func (value TransferLifecycleObserved) Progress() ProgressSnapshot    { return value.spec.Progress }
func (value TransferLifecycleObserved) FileSelection() FileSelectionDecision {
	return value.spec.FileSelection
}
func (value TransferLifecycleObserved) FileSettlement() FileSettlement {
	return value.spec.FileSettlement
}
func (value TransferLifecycleObserved) ItemBlock() (ItemBlockReason, bool) {
	return value.spec.ItemBlockReason, value.spec.ItemBlockReason != ItemBlockNone
}
func (value TransferLifecycleObserved) TreeSettlement() TreeSettlement {
	return value.spec.TreeSettlement
}
func (value TransferLifecycleObserved) Failure() (Failure, bool) {
	return value.spec.Failure, value.spec.Failure.Valid()
}
func (value TransferLifecycleObserved) Accept(visitor Visitor) error {
	return acceptTransferLifecycleObserved(visitor, value)
}

type FilesystemOutputCounters struct {
	NodeClaims             uint64
	DirectoryClaims        uint64
	FileClaims             uint64
	ActiveFileClaims       uint64
	ReservedFileSlots      uint64
	DirectoryMetadataBytes uint64
	CheckpointRecords      uint64
}

type FilesystemOutputSpec struct {
	Operation           FilesystemOutputOperation
	ReceiveIntent       ReceiveIntentDigest
	ReceiveOperation    ReceiveOperationID
	OutputSession       OutputSessionID
	Certification       FilesystemCertification
	NativeLockScope     FilesystemNativeLockScope
	NativeLockMilestone FilesystemNativeLockMilestone
	RootDisposition     FilesystemRootDisposition
	RuntimeComponent    FilesystemRuntimeComponent
	RuntimeOperation    FilesystemRuntimeOperation
	RuntimeDecision     FilesystemRuntimeDecisionKind
	CheckpointDecision  FilesystemCheckpointDecision
	OperationID         uint64
	ClaimID             uint64
	Counters            FilesystemOutputCounters
	Failure             Failure
	FailureStage        FilesystemFailureStage
	ReconciliationStep  FilesystemReconciliationStep
	NativeErrorClass    FilesystemNativeErrorClass
}

type FilesystemOutputObserved struct{ spec FilesystemOutputSpec }

func NewFilesystemOutputObserved(spec FilesystemOutputSpec) (FilesystemOutputObserved, error) {
	if !validFilesystemOutputSpec(spec) {
		return FilesystemOutputObserved{}, ErrInvalidEvent
	}
	return FilesystemOutputObserved{spec: spec}, nil
}

func validFilesystemOutputSpec(spec FilesystemOutputSpec) bool {
	return validFilesystemOutputNames(spec) &&
		spec.Failure.Valid() == (spec.FailureStage != 0) &&
		(spec.NativeLockScope != 0) == (spec.NativeLockMilestone != 0) &&
		validFilesystemRuntimeDecision(spec) &&
		validFilesystemReconciliation(spec)
}

type filesystemNamedValue interface {
	Name() (string, bool)
}

type optionalFilesystemName struct {
	present bool
	value   filesystemNamedValue
}

func validFilesystemOutputNames(spec FilesystemOutputSpec) bool {
	if _, ok := spec.Operation.Name(); !ok {
		return false
	}
	optional := [...]optionalFilesystemName{
		{spec.Certification != 0, spec.Certification},
		{spec.NativeLockScope != 0, spec.NativeLockScope},
		{spec.NativeLockMilestone != 0, spec.NativeLockMilestone},
		{spec.RootDisposition != 0, spec.RootDisposition},
		{spec.RuntimeComponent != 0, spec.RuntimeComponent},
		{spec.RuntimeOperation != 0, spec.RuntimeOperation},
		{spec.RuntimeDecision != 0, spec.RuntimeDecision},
		{spec.CheckpointDecision != 0, spec.CheckpointDecision},
		{spec.FailureStage != 0, spec.FailureStage},
		{spec.ReconciliationStep != 0, spec.ReconciliationStep},
		{spec.NativeErrorClass != 0, spec.NativeErrorClass},
	}
	for _, field := range optional {
		if field.present && !validFilesystemName(field.value) {
			return false
		}
	}
	return true
}

func validFilesystemName(value filesystemNamedValue) bool {
	_, ok := value.Name()
	return ok
}

func validFilesystemRuntimeDecision(spec FilesystemOutputSpec) bool {
	runtimeFields := 0
	if spec.RuntimeComponent != 0 {
		runtimeFields++
	}
	if spec.RuntimeOperation != 0 {
		runtimeFields++
	}
	if spec.RuntimeDecision != 0 {
		runtimeFields++
	}
	return runtimeFields == 0 || runtimeFields == 3
}

func validFilesystemReconciliation(spec FilesystemOutputSpec) bool {
	return spec.ReconciliationStep == 0 ||
		spec.FailureStage == FilesystemFailureCheckpointReconciliation ||
		spec.FailureStage == FilesystemFailureNativeDurability
}

func (FilesystemOutputObserved) event()           {}
func (FilesystemOutputObserved) Command() Command { return CommandGet }
func (FilesystemOutputObserved) Level() Level     { return LevelDebug }
func (value FilesystemOutputObserved) ReceiveOperationID() (ReceiveOperationID, bool) {
	return value.spec.ReceiveOperation, value.spec.ReceiveOperation.Valid()
}
func (value FilesystemOutputObserved) ReceiveIntentDigest() (ReceiveIntentDigest, bool) {
	return value.spec.ReceiveIntent, value.spec.ReceiveIntent.Valid()
}
func (value FilesystemOutputObserved) OutputSessionID() (OutputSessionID, bool) {
	return value.spec.OutputSession, value.spec.OutputSession.Valid()
}
func (value FilesystemOutputObserved) Operation() FilesystemOutputOperation {
	return value.spec.Operation
}
func (value FilesystemOutputObserved) Certification() (FilesystemCertification, bool) {
	_, ok := value.spec.Certification.Name()
	return value.spec.Certification, ok
}
func (value FilesystemOutputObserved) NativeLock() (FilesystemNativeLockScope, FilesystemNativeLockMilestone, bool) {
	_, a := value.spec.NativeLockScope.Name()
	_, b := value.spec.NativeLockMilestone.Name()
	return value.spec.NativeLockScope, value.spec.NativeLockMilestone, a && b
}
func (value FilesystemOutputObserved) RootDisposition() (FilesystemRootDisposition, bool) {
	_, ok := value.spec.RootDisposition.Name()
	return value.spec.RootDisposition, ok
}
func (value FilesystemOutputObserved) RuntimeDecision() (FilesystemRuntimeComponent, FilesystemRuntimeOperation, FilesystemRuntimeDecisionKind, bool) {
	_, a := value.spec.RuntimeComponent.Name()
	_, b := value.spec.RuntimeOperation.Name()
	_, c := value.spec.RuntimeDecision.Name()
	return value.spec.RuntimeComponent, value.spec.RuntimeOperation, value.spec.RuntimeDecision, a && b && c
}
func (value FilesystemOutputObserved) CheckpointDecision() (FilesystemCheckpointDecision, bool) {
	_, ok := value.spec.CheckpointDecision.Name()
	return value.spec.CheckpointDecision, ok
}
func (value FilesystemOutputObserved) Correlation() (uint64, uint64) {
	return value.spec.OperationID, value.spec.ClaimID
}
func (value FilesystemOutputObserved) Counters() FilesystemOutputCounters { return value.spec.Counters }
func (value FilesystemOutputObserved) Failure() (Failure, bool) {
	return value.spec.Failure, value.spec.Failure.Valid()
}
func (value FilesystemOutputObserved) FailureClassification() (FilesystemFailureStage, FilesystemReconciliationStep, FilesystemNativeErrorClass, bool) {
	return value.spec.FailureStage, value.spec.ReconciliationStep, value.spec.NativeErrorClass, value.spec.Failure.Valid()
}
func (value FilesystemOutputObserved) Accept(visitor Visitor) error {
	return acceptFilesystemOutputObserved(visitor, value)
}

type SenderTerminalObserved struct {
	session              ProtocolSessionID
	lane                 LaneIdentity
	settled              bool
	transportDisposition SenderTerminalTransport
	outcome              SenderTerminalOutcome
	decision             SenderTerminalDecision
}

func NewSenderTerminalObserved(
	session ProtocolSessionID,
	lane LaneIdentity,
	settled bool,
	transportDisposition SenderTerminalTransport,
	outcome SenderTerminalOutcome,
	decision SenderTerminalDecision,
) (SenderTerminalObserved, error) {
	_, transportOK := transportDisposition.Name()
	_, outcomeOK := outcome.Name()
	_, decisionOK := decision.Name()
	if !session.Valid() || !lane.Valid() || !transportOK || !outcomeOK || !decisionOK {
		return SenderTerminalObserved{}, ErrInvalidEvent
	}
	return SenderTerminalObserved{
		session: session, lane: lane, settled: settled,
		transportDisposition: transportDisposition, outcome: outcome, decision: decision,
	}, nil
}

func (SenderTerminalObserved) event()                                     {}
func (SenderTerminalObserved) Command() Command                           { return CommandShare }
func (SenderTerminalObserved) Level() Level                               { return LevelDebug }
func (value SenderTerminalObserved) ProtocolSessionID() ProtocolSessionID { return value.session }
func (value SenderTerminalObserved) Lane() LaneIdentity                   { return value.lane }
func (value SenderTerminalObserved) Settled() bool                        { return value.settled }
func (value SenderTerminalObserved) TransportDisposition() SenderTerminalTransport {
	return value.transportDisposition
}
func (value SenderTerminalObserved) Outcome() SenderTerminalOutcome   { return value.outcome }
func (value SenderTerminalObserved) Decision() SenderTerminalDecision { return value.decision }
func (value SenderTerminalObserved) Accept(visitor Visitor) error {
	return acceptSenderTerminalObserved(visitor, value)
}

type CatalogUsage struct {
	ActiveScans uint64
	ScanWork    uint64
	Entries     uint64
	MemoryBytes uint64
	SpillBytes  uint64
}

type CatalogStorageObserved struct {
	operation          CatalogStorageOperation
	cause              CatalogStorageCause
	usage              CatalogUsage
	legacyRootsRemoved uint64
}

func NewCatalogStorageObserved(
	operation CatalogStorageOperation,
	cause CatalogStorageCause,
	usage CatalogUsage,
	legacyRootsRemoved uint64,
) (CatalogStorageObserved, error) {
	_, operationOK := operation.Name()
	_, causeOK := cause.Name()
	if !operationOK || !causeOK {
		return CatalogStorageObserved{}, ErrInvalidEvent
	}
	return CatalogStorageObserved{
		operation: operation, cause: cause, usage: usage,
		legacyRootsRemoved: legacyRootsRemoved,
	}, nil
}

func (CatalogStorageObserved) event()                                   {}
func (CatalogStorageObserved) Command() Command                         { return CommandShare }
func (CatalogStorageObserved) Level() Level                             { return LevelDebug }
func (value CatalogStorageObserved) Operation() CatalogStorageOperation { return value.operation }
func (value CatalogStorageObserved) Cause() CatalogStorageCause         { return value.cause }
func (value CatalogStorageObserved) Usage() CatalogUsage                { return value.usage }
func (value CatalogStorageObserved) LegacyRootsRemoved() uint64 {
	return value.legacyRootsRemoved
}
func (value CatalogStorageObserved) Accept(visitor Visitor) error {
	return acceptCatalogStorageObserved(visitor, value)
}

type RootPrefetchObserved struct {
	decision     RootPrefetchDecision
	attempt      uint64
	entryCount   uint64
	omittedCount uint64
}

func NewRootPrefetchObserved(
	decision RootPrefetchDecision,
	attempt uint64,
	entryCount uint64,
	omittedCount uint64,
) (RootPrefetchObserved, error) {
	_, decisionOK := decision.Name()
	if !decisionOK || (decision != RootPrefetchStopped && attempt == 0) ||
		(decision != RootPrefetchCommitted && (entryCount != 0 || omittedCount != 0)) {
		return RootPrefetchObserved{}, ErrInvalidEvent
	}
	return RootPrefetchObserved{
		decision: decision, attempt: attempt, entryCount: entryCount, omittedCount: omittedCount,
	}, nil
}

func (RootPrefetchObserved) event()                               {}
func (RootPrefetchObserved) Command() Command                     { return CommandShare }
func (RootPrefetchObserved) Level() Level                         { return LevelDebug }
func (value RootPrefetchObserved) Decision() RootPrefetchDecision { return value.decision }
func (value RootPrefetchObserved) Attempt() uint64                { return value.attempt }
func (value RootPrefetchObserved) EntryCount() uint64             { return value.entryCount }
func (value RootPrefetchObserved) OmittedCount() uint64           { return value.omittedCount }
func (value RootPrefetchObserved) Accept(visitor Visitor) error {
	return acceptRootPrefetchObserved(visitor, value)
}
