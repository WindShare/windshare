package observation

import (
	hex "encoding/hex"
	strconv "strconv"
	strings "strings"
	utf8 "unicode/utf8"
)

const (
	maxObservationRejectionLabelBytes = 96
	maxObservationRejectionValueBytes = 256
	maxObservationRejectionFields     = 16
	maxObservationRejectionBytes      = 4096
)

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

type RejectionField struct {
	Field          string
	Representation string
	Value          string
	omittedBytes   uint64
}

func RejectedUint(field string, value uint64) RejectionField {
	return RejectionField{Field: field, Representation: "unsigned_decimal", Value: strconv.FormatUint(value, 10)}
}

func RejectedIdentity(field string, value []byte) RejectionField {
	return rejectedBytes(field, "identity_hex", value)
}

func rejectedBytes(field, representation string, value []byte) RejectionField {
	limit := min(len(value), maxObservationRejectionValueBytes/2)
	return RejectionField{
		Field: field, Representation: representation,
		Value:        hex.EncodeToString(value[:limit]),
		omittedBytes: uint64(len(value)-limit) * 2,
	}
}

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

func (value ObservationRejection) OmittedBytes() uint64 { return value.omittedBytes }

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

type ObserverLossSpec struct {
	Workflow       Workflow
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
	if !spec.Workflow.Valid() || !categoryOK || !reasonOK || spec.Count == 0 || spec.OmittedSamples > spec.Count ||
		(spec.Rejection != (ObservationRejection{}) && !spec.Rejection.Valid()) {
		return ObserverLossObserved{}, ErrInvalidEvent
	}
	if spec.Rejection != (ObservationRejection{}) {
		spec.Rejection = CaptureObservationRejection(spec.Rejection, spec.Rejection.Evidence()...)
	}
	return ObserverLossObserved{spec: spec}, nil
}

func (ObserverLossObserved) EngineEvent() {}

func (value ObserverLossObserved) Workflow() Workflow { return value.spec.Workflow }

func (ObserverLossObserved) Level() Level { return LevelDebug }

func (value ObserverLossObserved) Category() ObserverLossCategory { return value.spec.Category }

func (value ObserverLossObserved) Reason() ObserverLossReason { return value.spec.Reason }

func (value ObserverLossObserved) Count() uint64 { return value.spec.Count }

func (value ObserverLossObserved) OmittedSamples() uint64 { return value.spec.OmittedSamples }

func (value ObserverLossObserved) Rejection() (ObservationRejection, bool) {
	return value.spec.Rejection, value.spec.Rejection.Valid()
}

func (value ObserverLossObserved) Validate() error { return validateObserverLossObserved(value) }

func RejectedEnum(field string, value uint64) RejectionField {
	return RejectionField{Field: field, Representation: "enum_number", Value: strconv.FormatUint(value, 10)}
}

func RejectedBool(field string, value bool) RejectionField {
	return RejectionField{Field: field, Representation: "boolean", Value: strconv.FormatBool(value)}
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
