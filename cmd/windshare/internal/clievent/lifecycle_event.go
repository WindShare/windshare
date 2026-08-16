package clievent

type RelayLifecycleSpec struct {
	Command          Command
	LinkID           uint64
	SendOperationID  uint64
	Stage            RelayLifecycleStage
	Terminal         bool
	Disposition      SendDisposition
	RetirementSource RelayRetirementSource
	Cause            RelayLifecycleCause
	DrainCause       RelayLifecycleCause
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
	return spec.Command.Valid() && spec.LinkID != 0 && stageOK && retirementOK && causeOK && drainOK &&
		(spec.Disposition == 0 || dispositionOK)
}

func (RelayLifecycleObserved) event()                 {}
func (value RelayLifecycleObserved) Command() Command { return value.spec.Command }
func (RelayLifecycleObserved) Level() Level           { return LevelDebug }
func (value RelayLifecycleObserved) LinkID() uint64   { return value.spec.LinkID }
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
	return spec.Command.Valid() && spec.ChannelID != 0 && operationOK && transitionOK && stateOK &&
		terminalOK && causeOK && (spec.Disposition == 0 || dispositionOK) &&
		(spec.Transition == WebRTCTraceDropped) == (spec.Dropped != 0)
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
	Command       Command
	Session       ProtocolSessionID
	PeerPath      PeerPathID
	Attempt       PeerAttemptID
	Sequence      uint64
	ElapsedMillis uint64
	Stage         PeerAttemptStage
	Candidates    CandidateCounts
	HasCandidates bool
	Lane          LaneIdentity
	HasLane       bool
	FailureScope  PeerFailureScope
	Failure       Failure
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
	hasFailure := spec.Failure.Valid()
	return spec.Command.Valid() && spec.Session.Valid() && spec.PeerPath.Valid() && spec.Attempt.Valid() &&
		spec.Sequence != 0 && stageOK &&
		(spec.Stage == PeerAttemptFailed) == hasFailure &&
		(spec.Stage == PeerAttemptFailed) == scopeOK &&
		(!spec.HasLane || spec.Lane.Valid()) &&
		(spec.Stage == PeerAttemptAdmitted) == spec.HasLane
}

func (PeerAttemptObserved) event()                                     {}
func (value PeerAttemptObserved) Command() Command                     { return value.spec.Command }
func (PeerAttemptObserved) Level() Level                               { return LevelDebug }
func (value PeerAttemptObserved) ProtocolSessionID() ProtocolSessionID { return value.spec.Session }
func (value PeerAttemptObserved) PeerPathID() PeerPathID               { return value.spec.PeerPath }
func (value PeerAttemptObserved) PeerAttemptID() PeerAttemptID         { return value.spec.Attempt }
func (value PeerAttemptObserved) Sequence() uint64                     { return value.spec.Sequence }
func (value PeerAttemptObserved) ElapsedMillis() uint64                { return value.spec.ElapsedMillis }
func (value PeerAttemptObserved) Stage() PeerAttemptStage              { return value.spec.Stage }
func (value PeerAttemptObserved) Candidates() (CandidateCounts, bool) {
	return value.spec.Candidates, value.spec.HasCandidates
}
func (value PeerAttemptObserved) Lane() (LaneIdentity, bool) {
	return value.spec.Lane, value.spec.HasLane
}
func (value PeerAttemptObserved) Failure() (PeerFailureScope, Failure, bool) {
	return value.spec.FailureScope, value.spec.Failure, value.spec.Failure.Valid()
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
	_, treeSettlementOK := spec.TreeSettlement.Name()
	return spec.ReceiveOperation.Valid() && spec.ProtocolSession.Valid() && spec.TransferJob.Valid() &&
		stageOK && spec.Progress.Valid() && selectionOK && fileSettlementOK && treeSettlementOK
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

type FilesystemOutputObserved struct {
	receiveOperation    ReceiveOperationID
	hasReceiveOperation bool
	operation           FilesystemOutputOperation
	counters            FilesystemOutputCounters
	failure             Failure
	hasFailure          bool
}

func NewFilesystemOutputObserved(
	receiveOperation ReceiveOperationID,
	operation FilesystemOutputOperation,
	counters FilesystemOutputCounters,
	failure Failure,
) (FilesystemOutputObserved, error) {
	_, operationOK := operation.Name()
	if !operationOK {
		return FilesystemOutputObserved{}, ErrInvalidEvent
	}
	return FilesystemOutputObserved{
		receiveOperation: receiveOperation, hasReceiveOperation: receiveOperation.Valid(),
		operation: operation, counters: counters,
		failure: failure, hasFailure: failure.Valid(),
	}, nil
}

func (FilesystemOutputObserved) event()           {}
func (FilesystemOutputObserved) Command() Command { return CommandGet }
func (FilesystemOutputObserved) Level() Level     { return LevelDebug }
func (value FilesystemOutputObserved) ReceiveOperationID() (ReceiveOperationID, bool) {
	return value.receiveOperation, value.hasReceiveOperation
}
func (value FilesystemOutputObserved) Operation() FilesystemOutputOperation { return value.operation }
func (value FilesystemOutputObserved) Counters() FilesystemOutputCounters   { return value.counters }
func (value FilesystemOutputObserved) Failure() (Failure, bool) {
	return value.failure, value.hasFailure
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
