package clievent

import (
	"testing"
	"time"
)

func TestNativeConnectivitySealsFactsAndRejectsUnboundedInput(t *testing.T) {
	session, _ := NewProtocolSessionID(append([]byte{1}, make([]byte, 15)...))
	path, _ := NewPeerPathID(append([]byte{2}, make([]byte, 15)...))
	attempt, _ := NewPeerAttemptID(append([]byte{3}, make([]byte, 15)...))
	base := NativeConnectivitySpec{Command: CommandGet, Session: session, Path: path, Attempt: attempt, AttemptSequence: 4, NetworkGeneration: 5, Profile: "ice-0123abcd", Side: "receiver", Kind: "candidate", State: "unknown", Candidate: &NativeCandidateFacts{Type: "host", Protocol: "udp", Address: "127.0.0.1", Family: "ipv4", Origin: "ordinary", Port: 99}}
	event, err := NewNativeConnectivityObserved(base)
	if err != nil {
		t.Fatal(err)
	}
	base.Candidate.Port = 100
	facts := event.Facts()
	if facts.Candidate.Port != 99 {
		t.Fatal("mutable producer facts escaped")
	}
	facts.Candidate.Port = 101
	if event.Facts().Candidate.Port != 99 {
		t.Fatal("mutable visitor facts escaped")
	}
	if event.Command() != CommandGet || event.Level() != LevelDebug {
		t.Fatal("wrong event visibility")
	}
	visitor := &exhaustiveVisitor{}
	if err := event.Accept(visitor); err != nil {
		t.Fatal(err)
	}
	if err := event.Accept(nil); err == nil {
		t.Fatal("nil visitor accepted")
	}
	if err := (NativeConnectivityObserved{}).Accept(visitor); err == nil {
		t.Fatal("zero event accepted")
	}
	invalid := []func(*NativeConnectivitySpec){
		func(s *NativeConnectivitySpec) { s.Command = 0 },
		func(s *NativeConnectivitySpec) { s.Side = "password=secret" },
		func(s *NativeConnectivitySpec) { s.Kind = "raw provider error" },
		func(s *NativeConnectivitySpec) { s.State = "ice-password-secret" },
		func(s *NativeConnectivitySpec) { s.Session = ProtocolSessionID{} },
		func(s *NativeConnectivitySpec) { s.Path = PeerPathID{} },
		func(s *NativeConnectivitySpec) { s.AttemptSequence = 0 },
		func(s *NativeConnectivitySpec) { s.Profile = "turn:user:password@host" },
		func(s *NativeConnectivitySpec) { s.Profile = "ice-xxxxxxxx" },
		func(s *NativeConnectivitySpec) { s.Candidate = nil },
		func(s *NativeConnectivitySpec) { s.Candidate.Type = "unexpected" },
		func(s *NativeConnectivitySpec) { s.Candidate.Address = "credential.local" },
		func(s *NativeConnectivitySpec) { s.Reachability = &NativeReachabilityFacts{} },
	}
	for i, mutate := range invalid {
		spec := event.Facts()
		mutate(&spec)
		if _, err := NewNativeConnectivityObserved(spec); err == nil {
			t.Fatalf("invalid mutation %d accepted", i)
		}
	}
	pathWithoutSession := NativeConnectivitySpec{Command: CommandGet, Side: "unknown", Kind: "provider_closed", State: "unknown", Path: path}
	if _, err := NewNativeConnectivityObserved(pathWithoutSession); err == nil {
		t.Fatal("unbound path accepted")
	}
	for _, spec := range []NativeConnectivitySpec{
		{Command: CommandShare, Side: "sender", Kind: "selected_pair", State: "unknown", Pair: &NativePairFacts{LocalType: "host", RemoteType: "srflx", Protocol: "tcp", LocalAddress: "::1", RemoteAddress: "unknown", PairRTT: time.Millisecond}},
		{Command: CommandGet, Side: "receiver", Kind: "lease-ready", State: "unknown", Reachability: &NativeReachabilityFacts{Protocol: "udp", Reason: "none"}},
		{Command: CommandGet, Side: "receiver", Kind: "network_changed", State: "unknown", Lifecycle: &NativeLifecycleFacts{PreviousGeneration: 1}},
	} {
		value, err := NewNativeConnectivityObserved(spec)
		if err != nil {
			t.Fatal(err)
		}
		copied := value.Facts()
		if copied.Pair != nil {
			copied.Pair.PairRTT = -1
			if _, err := NewNativeConnectivityObserved(copied); err == nil {
				t.Fatal("negative RTT accepted")
			}
			if value.Facts().Pair.PairRTT != time.Millisecond {
				t.Fatal("pair alias")
			}
		}
		if copied.Reachability != nil {
			copied.Reachability.Reason = "raw-error"
			if _, err := NewNativeConnectivityObserved(copied); err == nil {
				t.Fatal("open reason accepted")
			}
			if value.Facts().Reachability.Reason != "none" {
				t.Fatal("reachability alias")
			}
		}
		if copied.Lifecycle != nil {
			copied.Lifecycle.PreviousGeneration = 9
			if value.Facts().Lifecycle.PreviousGeneration != 1 {
				t.Fatal("lifecycle alias")
			}
		}
	}
}
