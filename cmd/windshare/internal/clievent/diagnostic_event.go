package clievent

type LaneSettlementSpec struct {
	Session             ProtocolSessionID
	Route               LaneRoute
	Lane                LaneIdentity
	DeliveredBlocks     uint64
	DeliveredBytes      uint64
	FailedBlockAttempts uint64
	ReassignedBlocks    uint64
	Incomplete          bool
}

type LaneSettlementObserved struct{ spec LaneSettlementSpec }

func NewLaneSettlementObserved(spec LaneSettlementSpec) (LaneSettlementObserved, error) {
	_, routeOK := spec.Route.Name()
	if !spec.Session.Valid() || !spec.Lane.Valid() || !routeOK {
		return LaneSettlementObserved{}, ErrInvalidEvent
	}
	return LaneSettlementObserved{spec: spec}, nil
}

func (LaneSettlementObserved) event()                                     {}
func (LaneSettlementObserved) Command() Command                           { return CommandGet }
func (LaneSettlementObserved) Level() Level                               { return LevelDebug }
func (value LaneSettlementObserved) ProtocolSessionID() ProtocolSessionID { return value.spec.Session }
func (value LaneSettlementObserved) Route() LaneRoute                     { return value.spec.Route }
func (value LaneSettlementObserved) Lane() LaneIdentity                   { return value.spec.Lane }
func (value LaneSettlementObserved) DeliveredBlocks() uint64              { return value.spec.DeliveredBlocks }
func (value LaneSettlementObserved) DeliveredBytes() uint64               { return value.spec.DeliveredBytes }
func (value LaneSettlementObserved) FailedBlockAttempts() uint64 {
	return value.spec.FailedBlockAttempts
}
func (value LaneSettlementObserved) ReassignedBlocks() uint64 { return value.spec.ReassignedBlocks }
func (value LaneSettlementObserved) Incomplete() bool         { return value.spec.Incomplete }
func (value LaneSettlementObserved) Accept(visitor Visitor) error {
	return acceptLaneSettlementObserved(visitor, value)
}

type ObserverLossSpec struct {
	Command  Command
	Category ObserverLossCategory
	Reason   ObserverLossReason
	Count    uint64
}

type ObserverLossObserved struct{ spec ObserverLossSpec }

func NewObserverLossObserved(spec ObserverLossSpec) (ObserverLossObserved, error) {
	_, categoryOK := spec.Category.Name()
	_, reasonOK := spec.Reason.Name()
	if !spec.Command.Valid() || !categoryOK || !reasonOK || spec.Count == 0 {
		return ObserverLossObserved{}, ErrInvalidEvent
	}
	return ObserverLossObserved{spec: spec}, nil
}

func (ObserverLossObserved) event()                               {}
func (value ObserverLossObserved) Command() Command               { return value.spec.Command }
func (ObserverLossObserved) Level() Level                         { return LevelDebug }
func (value ObserverLossObserved) Category() ObserverLossCategory { return value.spec.Category }
func (value ObserverLossObserved) Reason() ObserverLossReason     { return value.spec.Reason }
func (value ObserverLossObserved) Count() uint64                  { return value.spec.Count }
func (value ObserverLossObserved) Accept(visitor Visitor) error {
	return acceptObserverLossObserved(visitor, value)
}

type ReceiverTerminationSpec struct {
	Operation             ProtocolOperationID
	HasOperation          bool
	LocalGeneration       uint64
	TransitionAuthority   ReceiverTerminalOwner
	Disposition           ReceiverDisposition
	TransitionProvenance  ReceiverProvenance
	ConsequenceProvenance ReceiverProvenance
	LocalStopReason       ReceiverLocalStopReason
	DiagnosticsTruncated  bool
	BenignComponents      []ReceiverBenignComponent
	RetainedCauseClasses  []ReceiverCauseClass
	TeardownTransitions   []PeerTeardownTransition
	PeerShutdownFailed    bool
	ChannelDrainFailed    bool
}

type ReceiverTerminationObserved struct{ spec ReceiverTerminationSpec }

func NewReceiverTerminationObserved(spec ReceiverTerminationSpec) (ReceiverTerminationObserved, error) {
	if !validReceiverTerminationSpec(spec) {
		return ReceiverTerminationObserved{}, ErrInvalidEvent
	}
	spec.BenignComponents = append([]ReceiverBenignComponent(nil), spec.BenignComponents...)
	spec.RetainedCauseClasses = append([]ReceiverCauseClass(nil), spec.RetainedCauseClasses...)
	spec.TeardownTransitions = append([]PeerTeardownTransition(nil), spec.TeardownTransitions...)
	return ReceiverTerminationObserved{spec: spec}, nil
}

func validReceiverTerminationSpec(spec ReceiverTerminationSpec) bool {
	_, ownerOK := spec.TransitionAuthority.Name()
	_, dispositionOK := spec.Disposition.Name()
	_, transitionOK := spec.TransitionProvenance.Name()
	_, consequenceOK := spec.ConsequenceProvenance.Name()
	_, localStopOK := spec.LocalStopReason.Name()
	if spec.HasOperation != spec.Operation.Valid() || spec.LocalGeneration == 0 || !ownerOK || !dispositionOK || !transitionOK || !consequenceOK || !localStopOK {
		return false
	}
	for _, value := range spec.BenignComponents {
		if _, ok := value.Name(); !ok {
			return false
		}
	}
	for _, value := range spec.RetainedCauseClasses {
		if _, ok := value.Name(); !ok {
			return false
		}
	}
	for _, value := range spec.TeardownTransitions {
		if _, ok := value.Name(); !ok {
			return false
		}
	}
	return true
}

func (ReceiverTerminationObserved) event()           {}
func (ReceiverTerminationObserved) Command() Command { return CommandGet }
func (ReceiverTerminationObserved) Level() Level     { return LevelDebug }
func (value ReceiverTerminationObserved) OperationID() (ProtocolOperationID, bool) {
	return value.spec.Operation, value.spec.HasOperation
}
func (value ReceiverTerminationObserved) LocalGeneration() uint64 { return value.spec.LocalGeneration }
func (value ReceiverTerminationObserved) TransitionAuthority() ReceiverTerminalOwner {
	return value.spec.TransitionAuthority
}
func (value ReceiverTerminationObserved) Disposition() ReceiverDisposition {
	return value.spec.Disposition
}
func (value ReceiverTerminationObserved) TransitionProvenance() ReceiverProvenance {
	return value.spec.TransitionProvenance
}
func (value ReceiverTerminationObserved) ConsequenceProvenance() ReceiverProvenance {
	return value.spec.ConsequenceProvenance
}
func (value ReceiverTerminationObserved) LocalStopReason() ReceiverLocalStopReason {
	return value.spec.LocalStopReason
}
func (value ReceiverTerminationObserved) DiagnosticsTruncated() bool {
	return value.spec.DiagnosticsTruncated
}
func (value ReceiverTerminationObserved) BenignComponents() []ReceiverBenignComponent {
	return append([]ReceiverBenignComponent(nil), value.spec.BenignComponents...)
}
func (value ReceiverTerminationObserved) RetainedCauseClasses() []ReceiverCauseClass {
	return append([]ReceiverCauseClass(nil), value.spec.RetainedCauseClasses...)
}
func (value ReceiverTerminationObserved) TeardownTransitions() []PeerTeardownTransition {
	return append([]PeerTeardownTransition(nil), value.spec.TeardownTransitions...)
}
func (value ReceiverTerminationObserved) PeerShutdownFailed() bool {
	return value.spec.PeerShutdownFailed
}
func (value ReceiverTerminationObserved) ChannelDrainFailed() bool {
	return value.spec.ChannelDrainFailed
}
func (value ReceiverTerminationObserved) Accept(visitor Visitor) error {
	return acceptReceiverTerminationObserved(visitor, value)
}
