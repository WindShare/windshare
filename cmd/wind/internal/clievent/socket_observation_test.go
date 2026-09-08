package clievent

import "testing"

func TestSocketFactsAreImmutableAndRequireMatchingEventKinds(t *testing.T) {
	base := NativeConnectivitySpec{Command: CommandShare, Side: "sender", State: "unknown", Kind: "socket_handoff_finished", Socket: &NativeSocketFacts{Result: "completed"}}
	event, err := NewNativeConnectivityObserved(base)
	if err != nil {
		t.Fatal(err)
	}
	base.Socket.Result = "failed"
	facts := event.Facts()
	facts.Socket.Result = "retired"
	if event.Facts().Socket.Result != "completed" {
		t.Fatal("socket facts were mutated after publication")
	}
	for _, mutate := range []func(*NativeConnectivitySpec){
		func(spec *NativeConnectivitySpec) { spec.Socket = nil },
		func(spec *NativeConnectivitySpec) { spec.Kind = "provider_closed" },
		func(spec *NativeConnectivitySpec) { spec.Socket.Duration = -1 },
		func(spec *NativeConnectivitySpec) { spec.Socket.Result = "unclassified result" },
		func(spec *NativeConnectivitySpec) { spec.Socket.Result = "pending" },
		func(spec *NativeConnectivitySpec) { spec.Kind = "socket_handoff_started" },
	} {
		spec := event.Facts()
		mutate(&spec)
		if _, err := NewNativeConnectivityObserved(spec); err == nil {
			t.Fatalf("invalid socket facts accepted: %+v", spec)
		}
	}
}
