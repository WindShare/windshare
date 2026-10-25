package wire

import (
	errors "errors"
	strconv "strconv"

	appobs "github.com/windshare/windshare/engine/observation"
)

const SchemaVersion = 4

var ErrUnsupportedEvent = errors.New("observation cannot be represented by the application schema")

var errInvalidSchemaEvent = ErrUnsupportedEvent

type Payload interface{ ObservationPayload() }

type Encoded struct {
	SchemaVersion int            `json:"schema_version"`
	Event         string         `json:"event"`
	Workflow      string         `json:"workflow"`
	Level         string         `json:"level"`
	Correlation   *CorrelationV1 `json:"correlation,omitempty"`
	Payload       Payload        `json:"payload"`
}

type namedValue interface{ Name() (string, bool) }

type encoder struct{ record *Encoded }

func Encode(event appobs.Event) (Encoded, error) {
	if event == nil {
		return Encoded{}, appobs.ErrInvalidEvent
	}
	if err := event.Validate(); err != nil {
		return Encoded{}, err
	}
	var record Encoded
	enc := &encoder{record: &record}
	if err := enc.encodeEvent(event); err != nil {
		return Encoded{}, err
	}
	if record.Event == "" || record.Payload == nil {
		return Encoded{}, ErrUnsupportedEvent
	}
	record.SchemaVersion = SchemaVersion
	var workflowOK, levelOK bool
	record.Workflow, workflowOK = event.Workflow().Name()
	record.Level, levelOK = event.Level().Name()
	if !workflowOK || !levelOK {
		return Encoded{}, appobs.ErrInvalidEvent
	}
	return record, nil
}

func decimal(value uint64) string { return strconv.FormatUint(value, 10) }

func signedDecimal(value int64) string { return strconv.FormatInt(value, 10) }

func decimalPointer(value uint64) *string { encoded := decimal(value); return &encoded }

func (enc *encoder) encodeEvent(event appobs.Event) error {
	switch value := event.(type) {
	case appobs.Ready:
		return enc.encodeReady(value)
	case appobs.SharingSubjectSelected:
		return enc.encodeSharingSubjectSelected(value)
	case appobs.RelayConnected:
		return enc.encodeRelayConnected(value)
	case appobs.RelayAvailability:
		return enc.encodeRelayAvailability(value)
	case appobs.RelayRecovering:
		return enc.encodeRelayRecovering(value)
	case appobs.ContentPathSelected:
		return enc.encodeContentPathSelected(value)
	case appobs.Fallback:
		return enc.encodeFallback(value)
	case appobs.TransferProgress:
		return enc.encodeTransferProgress(value)
	case appobs.Warning:
		return enc.encodeWarning(value)
	case appobs.LaneAdopted:
		return enc.encodeLaneAdopted(value)
	case appobs.EngineTaskObserved:
		return enc.encodeEngineTaskObserved(value)
	case appobs.ObserverLossObserved:
		return enc.encodeObserverLossObserved(value)
	default:
		return ErrUnsupportedEvent
	}
}
