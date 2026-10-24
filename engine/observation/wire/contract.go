// Package wire owns the JSON contract shared by log writers and application transports.
// Encoding happens at consumers, after bounded publication, never on a transfer's IO path.
package wire

import (
	"errors"
	"strconv"

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

// ProviderEncoder lets a platform own its diagnostic schema without importing
// that platform into the shared engine.
type ProviderEncoder interface{ EncodeObservation() (Encoded, error) }

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
func decimal(value uint64) string         { return strconv.FormatUint(value, 10) }
func signedDecimal(value int64) string    { return strconv.FormatInt(value, 10) }
func decimalPointer(value uint64) *string { encoded := decimal(value); return &encoded }

func (enc *encoder) encodeEvent(event appobs.Event) error {
	switch value := event.(type) {
	case appobs.LossSnapshot:
		return enc.encodeLossSnapshot(value)
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
	case appobs.NativeConnectivityObserved:
		return enc.encodeNativeConnectivityObserved(value)
	case appobs.SenderCapacityObserved:
		return enc.encodeSenderCapacityObserved(value)
	case appobs.SenderRevisionObserved:
		return enc.encodeSenderRevisionObserved(value)
	case appobs.SenderTerminalSendObserved:
		return enc.encodeSenderTerminalSendObserved(value)
	case appobs.SenderSessionTerminated:
		return enc.encodeSenderSessionTerminated(value)
	case appobs.CatalogStorageObserved:
		return enc.encodeCatalogStorageObserved(value)
	case appobs.RootPrefetchObserved:
		return enc.encodeRootPrefetchObserved(value)
	case appobs.ProtocolObservationObserved:
		return enc.encodeProtocolObservationObserved(value)
	case appobs.LaneSettlementObserved:
		return enc.encodeLaneSettlementObserved(value)
	case appobs.ObserverLossObserved:
		return enc.encodeObserverLossObserved(value)
	case appobs.ReceiverTerminationObserved:
		return enc.encodeReceiverTerminationObserved(value)
	case appobs.TransferLifecycleObserved:
		return enc.encodeTransferLifecycleObserved(value)
	case appobs.RelayLifecycleObserved:
		return enc.encodeRelayLifecycleObserved(value)
	case appobs.WebRTCLifecycleObserved:
		return enc.encodeWebRTCLifecycleObserved(value)
	case appobs.PeerAttemptObserved:
		return enc.encodePeerAttemptObserved(value)
	case ProviderEncoder:
		record, err := value.EncodeObservation()
		*enc.record = record
		return err
	default:
		return ErrUnsupportedEvent
	}
}
