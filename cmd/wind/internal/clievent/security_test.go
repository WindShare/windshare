package clievent

import (
	"reflect"
	"testing"
	"time"

	"github.com/windshare/windshare/core/diagnosticerror"
	appobs "github.com/windshare/windshare/engine/observation"
)

func TestSealedEventPayloadTypesExposeNoOpenEndedOrRawErrorSurface(t *testing.T) {
	eventTypes := []reflect.Type{
		reflect.TypeFor[PlatformSetupObserved](), reflect.TypeFor[ObserverLossObserved](),
		reflect.TypeFor[Ready](), reflect.TypeFor[SharingSubjectSelected](),
		reflect.TypeFor[RelayConnected](), reflect.TypeFor[RelayRecovering](),
		reflect.TypeFor[ContentPathSelected](), reflect.TypeFor[Fallback](),
		reflect.TypeFor[TransferProgress](), reflect.TypeFor[Warning](),
		reflect.TypeFor[CommandFailed](), reflect.TypeFor[TransferSettled](),
		reflect.TypeFor[SharingStopped](), reflect.TypeFor[TraceIncomplete](),
		reflect.TypeFor[LaneAdopted](),
		reflect.TypeFor[RelayLifecycleObserved](), reflect.TypeFor[WebRTCLifecycleObserved](),
		reflect.TypeFor[PeerAttemptObserved](), reflect.TypeFor[TransferLifecycleObserved](),
		reflect.TypeFor[FilesystemOutputObserved](), reflect.TypeFor[SenderTerminalSendObserved](),
		reflect.TypeFor[SenderSessionTerminated](), reflect.TypeFor[ProtocolObservationObserved](),
		reflect.TypeFor[CatalogStorageObserved](),
		reflect.TypeFor[RootPrefetchObserved](),
		reflect.TypeFor[SenderCapacityObserved](), reflect.TypeFor[SenderRevisionObserved](),
	}
	seen := make(map[reflect.Type]bool)
	for _, eventType := range eventTypes {
		assertSafePayloadType(t, eventType, seen)
	}
}

func assertSafePayloadType(t *testing.T, value reflect.Type, seen map[reflect.Type]bool) {
	t.Helper()
	if seen[value] {
		return
	}
	seen[value] = true
	if value == reflect.TypeFor[time.Time]() {
		return
	}
	if value == reflect.TypeFor[appobs.ProtocolFact]() {
		for _, fact := range []reflect.Type{reflect.TypeFor[appobs.ProtocolOperationFact](), reflect.TypeFor[appobs.SenderContentDecisionFact](), reflect.TypeFor[appobs.ResponseSendNotStartedFact](), reflect.TypeFor[appobs.ResponseSendReturnedFact](), reflect.TypeFor[appobs.SendAttemptSettledFact](), reflect.TypeFor[appobs.ReceivedProtocolErrorFact]()} {
			assertSafePayloadType(t, fact, seen)
		}
		return
	}
	if value == reflect.TypeFor[error]() {
		t.Fatalf("event payload reaches raw error interface through %v", value)
	}
	switch value.Kind() {
	case reflect.Slice:
		// Only the snapshot's private, bounded collections are allowed; its
		// accessors return copies and its leaves cannot retain raw errors.
		if value != reflect.TypeFor[[]diagnosticerror.Node]() && value != reflect.TypeFor[[]diagnosticerror.Frame]() {
			t.Fatalf("event payload contains unreviewed slice %v", value)
		}
		assertSafePayloadType(t, value.Elem(), seen)
	case reflect.Map, reflect.Interface, reflect.Func, reflect.Chan, reflect.Pointer:
		t.Fatalf("event payload contains open-ended or reference-bearing type %v", value)
	case reflect.String:
		// Strings belong to reviewed display, relay-host, or frozen diagnostic
		// values. Product classifications remain closed numeric enums.
		return
	case reflect.Array:
		assertSafePayloadType(t, value.Elem(), seen)
	case reflect.Struct:
		for field := range value.Fields() {
			if field.Type.Kind() == reflect.String {
				owner := value.Name()
				diagnostic := value == reflect.TypeFor[diagnosticerror.Snapshot]() ||
					value == reflect.TypeFor[diagnosticerror.Node]() || value == reflect.TypeFor[diagnosticerror.Frame]()
				// Rejection evidence has fixed field storage and explicit
				// aggregate/per-value byte bounds; raw errors are never retained.
				diagnostic = diagnostic || value == reflect.TypeFor[appobs.ObservationRejection]() ||
					value == reflect.TypeFor[appobs.RejectionField]() || value == reflect.TypeFor[appobs.SendAttemptCause]()
				if !diagnostic && owner != "DisplayName" && owner != "DisplayPath" && owner != "RelayAuthority" {
					t.Fatalf("unreviewed string field %s.%s", owner, field.Name)
				}
			}
			assertSafePayloadType(t, field.Type, seen)
		}
	}
}
