package clievent

import (
	"reflect"
	"testing"
)

func TestSealedEventPayloadTypesExposeNoOpenEndedOrRawErrorSurface(t *testing.T) {
	eventTypes := []reflect.Type{
		reflect.TypeFor[Ready](), reflect.TypeFor[SharingSubjectSelected](),
		reflect.TypeFor[RelayConnected](), reflect.TypeFor[RelayRecovering](),
		reflect.TypeFor[ContentPathSelected](), reflect.TypeFor[Fallback](),
		reflect.TypeFor[TransferProgress](), reflect.TypeFor[Warning](),
		reflect.TypeFor[CommandFailed](), reflect.TypeFor[TransferSettled](),
		reflect.TypeFor[SharingStopped](), reflect.TypeFor[TraceIncomplete](),
		reflect.TypeFor[LaneAdopted](),
		reflect.TypeFor[RelayLifecycleObserved](), reflect.TypeFor[WebRTCLifecycleObserved](),
		reflect.TypeFor[PeerAttemptObserved](), reflect.TypeFor[TransferLifecycleObserved](),
		reflect.TypeFor[FilesystemOutputObserved](), reflect.TypeFor[SenderTerminalObserved](),
		reflect.TypeFor[CatalogStorageObserved](),
		reflect.TypeFor[RootPrefetchObserved](),
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
	if value == reflect.TypeFor[error]() {
		t.Fatalf("event payload reaches raw error interface through %v", value)
	}
	switch value.Kind() {
	case reflect.Map, reflect.Interface, reflect.Func, reflect.Chan, reflect.Pointer, reflect.Slice:
		t.Fatalf("event payload contains open-ended or reference-bearing type %v", value)
	case reflect.String:
		// The only strings reachable from events are explicitly display-only text
		// and the normalized relay host. Everything else is a closed numeric enum.
		return
	case reflect.Array:
		assertSafePayloadType(t, value.Elem(), seen)
	case reflect.Struct:
		for field := range value.Fields() {
			if field.Type.Kind() == reflect.String {
				owner := value.Name()
				if owner != "DisplayName" && owner != "DisplayPath" && owner != "RelayAuthority" {
					t.Fatalf("unreviewed string field %s.%s", owner, field.Name)
				}
			}
			assertSafePayloadType(t, field.Type, seen)
		}
	}
}
