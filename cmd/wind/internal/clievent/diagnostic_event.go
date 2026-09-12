package clievent

import (
	"encoding/hex"
	"strconv"
	"strings"
	"unicode/utf8"
)

const (
	maxObservationRejectionLabelBytes = 96
	maxObservationRejectionValueBytes = 256
	maxObservationRejectionFields     = 16
	maxObservationRejectionBytes      = 4096
)

// ObservationRejection preserves evidence even when normal event identity or
// enum contracts reject the source. Association strings deliberately have only
// format bounds; they must not be mistaken for authenticated correlation.
type ObservationRejection struct {
	Event            string
	Source           string
	Stage            string
	Field            string
	Rule             string
	Session          string
	Operation        string
	Revision         string
	ResponseSequence string
	AttemptSequence  string

	evidence      [maxObservationRejectionFields]RejectionField
	evidenceCount uint8
	omittedFields uint64
	omittedBytes  uint64
}

// RejectionField is a representation of one original operand, not an inferred
// replacement for the rejected business value.
type RejectionField struct {
	Field          string
	Representation string
	Value          string
	omittedBytes   uint64
}

func RejectedEnum(field string, value uint64) RejectionField {
	return RejectionField{Field: field, Representation: "enum_number", Value: strconv.FormatUint(value, 10)}
}

func RejectedUint(field string, value uint64) RejectionField {
	return RejectionField{Field: field, Representation: "unsigned_decimal", Value: strconv.FormatUint(value, 10)}
}

func RejectedBool(field string, value bool) RejectionField {
	return RejectionField{Field: field, Representation: "boolean", Value: strconv.FormatBool(value)}
}

func RejectedIdentity(field string, value []byte) RejectionField {
	return rejectedBytes(field, "identity_hex", value)
}

func RejectedString(field, value string) RejectionField {
	if !utf8.ValidString(value) {
		limit := min(len(value), maxObservationRejectionValueBytes/2)
		result := rejectedBytes(field, "bytes_hex", []byte(value[:limit]))
		result.omittedBytes = uint64(len(value)-limit) * 2
		return result
	}
	return RejectionField{Field: field, Representation: "string", Value: value}
}

func rejectedBytes(field, representation string, value []byte) RejectionField {
	limit := min(len(value), maxObservationRejectionValueBytes/2)
	return RejectionField{
		Field: field, Representation: representation,
		Value:        hex.EncodeToString(value[:limit]),
		omittedBytes: uint64(len(value)-limit) * 2,
	}
}

// CaptureObservationRejection runs only after rejection. Fixed storage and
// cloned bounded strings prevent the sample retaining source buffers or growing
// with source size, while exact omission counts explain incomplete evidence.
func CaptureObservationRejection(context ObservationRejection, fields ...RejectionField) ObservationRejection {
	context.evidence = [maxObservationRejectionFields]RejectionField{}
	context.evidenceCount = 0
	remaining := maxObservationRejectionBytes
	capture := func(value string, limit int) string {
		value, omitted := boundedRejectionString(value, min(limit, remaining))
		context.omittedBytes = addRejectionCount(context.omittedBytes, omitted)
		remaining -= len(value)
		return value
	}
	for _, label := range []*string{&context.Event, &context.Source, &context.Stage, &context.Field, &context.Rule} {
		*label = capture(*label, maxObservationRejectionLabelBytes)
	}
	for _, association := range []*string{&context.Session, &context.Operation, &context.Revision, &context.ResponseSequence, &context.AttemptSequence} {
		*association = capture(*association, maxObservationRejectionValueBytes)
	}
	for _, field := range fields {
		context.omittedBytes = addRejectionCount(context.omittedBytes, field.omittedBytes)
		// Retain the representation atomically; a partial tag would make
		// otherwise useful diagnostic evidence fail its own format contract.
		metadataBytes := min(len(field.Field), maxObservationRejectionLabelBytes) + len(field.Representation)
		if int(context.evidenceCount) == len(context.evidence) || metadataBytes > remaining {
			context.omittedFields = addRejectionCount(context.omittedFields, 1)
			context.omittedBytes = addRejectionCount(context.omittedBytes, uint64(len(field.Field)+len(field.Representation)+len(field.Value)))
			continue
		}
		field.Field = capture(field.Field, maxObservationRejectionLabelBytes)
		field.Representation = capture(field.Representation, maxObservationRejectionLabelBytes)
		field.Value = capture(field.Value, maxObservationRejectionValueBytes)
		field.omittedBytes = 0
		if field.Field == "" || field.Representation == "" {
			context.omittedFields = addRejectionCount(context.omittedFields, 1)
			context.omittedBytes = addRejectionCount(context.omittedBytes, uint64(len(field.Field)+len(field.Representation)+len(field.Value)))
			continue
		}
		context.evidence[context.evidenceCount] = field
		context.evidenceCount++
	}
	return context
}

func boundedRejectionString(value string, limit int) (string, uint64) {
	if !utf8.ValidString(value) {
		// Invalid text remains byte-identifiable without asking JSON encoding to
		// silently replace source bytes with Unicode replacement characters.
		prefix := min(len(value), limit/2)
		return hex.EncodeToString([]byte(value[:prefix])), uint64(len(value)-prefix) * 2
	}
	end := min(len(value), limit)
	for end > 0 && !utf8.ValidString(value[:end]) {
		end--
	}
	return strings.Clone(value[:end]), uint64(len(value) - end)
}

func addRejectionCount(current, amount uint64) uint64 {
	if amount > ^uint64(0)-current {
		return ^uint64(0)
	}
	return current + amount
}

func (value ObservationRejection) Evidence() []RejectionField {
	return append([]RejectionField(nil), value.evidence[:value.evidenceCount]...)
}
func (value ObservationRejection) Truncated() bool {
	return value.omittedFields != 0 || value.omittedBytes != 0
}
func (value ObservationRejection) OmittedFields() uint64 { return value.omittedFields }
func (value ObservationRejection) OmittedBytes() uint64  { return value.omittedBytes }

func (value ObservationRejection) Valid() bool {
	total := 0
	for _, label := range [...]string{value.Event, value.Source, value.Stage, value.Field, value.Rule} {
		if label == "" || len(label) > maxObservationRejectionLabelBytes || !utf8.ValidString(label) {
			return false
		}
		total += len(label)
	}
	for _, association := range [...]string{value.Session, value.Operation, value.Revision, value.ResponseSequence, value.AttemptSequence} {
		if len(association) > maxObservationRejectionValueBytes || !utf8.ValidString(association) {
			return false
		}
		total += len(association)
	}
	for _, field := range value.evidence[:value.evidenceCount] {
		if field.Field == "" || len(field.Field) > maxObservationRejectionLabelBytes || !utf8.ValidString(field.Field) ||
			len(field.Value) > maxObservationRejectionValueBytes || !utf8.ValidString(field.Value) {
			return false
		}
		switch field.Representation {
		case "enum_number", "boolean", "unsigned_decimal", "identity_hex", "string", "bytes_hex":
		default:
			return false
		}
		total += len(field.Field) + len(field.Representation) + len(field.Value)
	}
	return total <= maxObservationRejectionBytes
}

// EventContractError names the rejected invariant without retaining a whole
// event or allocating a diagnostic snapshot on successful validation.
type EventContractError struct {
	Field string
	Rule  string
}

func (err EventContractError) Error() string { return "CLI event: " + err.Field + ": " + err.Rule }
func (err EventContractError) Unwrap() error { return ErrInvalidEvent }

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
	Command        Command
	Category       ObserverLossCategory
	Reason         ObserverLossReason
	Count          uint64
	OmittedSamples uint64
	Rejection      ObservationRejection
}

type ObserverLossObserved struct{ spec ObserverLossSpec }

func NewObserverLossObserved(spec ObserverLossSpec) (ObserverLossObserved, error) {
	_, categoryOK := spec.Category.Name()
	_, reasonOK := spec.Reason.Name()
	if !spec.Command.Valid() || !categoryOK || !reasonOK || spec.Count == 0 || spec.OmittedSamples > spec.Count ||
		(spec.Rejection != (ObservationRejection{}) && !spec.Rejection.Valid()) {
		return ObserverLossObserved{}, ErrInvalidEvent
	}
	if spec.Rejection != (ObservationRejection{}) {
		spec.Rejection = CaptureObservationRejection(spec.Rejection, spec.Rejection.Evidence()...)
	}
	return ObserverLossObserved{spec: spec}, nil
}

func (ObserverLossObserved) event()                               {}
func (value ObserverLossObserved) Command() Command               { return value.spec.Command }
func (ObserverLossObserved) Level() Level                         { return LevelDebug }
func (value ObserverLossObserved) Category() ObserverLossCategory { return value.spec.Category }
func (value ObserverLossObserved) Reason() ObserverLossReason     { return value.spec.Reason }
func (value ObserverLossObserved) Count() uint64                  { return value.spec.Count }
func (value ObserverLossObserved) OmittedSamples() uint64         { return value.spec.OmittedSamples }
func (value ObserverLossObserved) Rejection() (ObservationRejection, bool) {
	return value.spec.Rejection, value.spec.Rejection.Valid()
}
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
