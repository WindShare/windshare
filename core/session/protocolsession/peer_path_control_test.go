package protocolsession

import (
	"bytes"
	"testing"
	"time"
)

func TestPeerPathControlCanonicalBoundedSchema(t *testing.T) {
	valid := PeerPathControl{PeerPathID: [16]byte{1}, NetworkGenerationID: [16]byte{2}, ControlSequence: 1, Kind: PeerPathDemand, ValidFor: time.Minute, HoldFor: 40 * time.Second, ProviderProfile: "pion-4.2.16-ice-4.2.7-windows"}
	for kind := PeerPathDemand; kind <= PeerPathNetworkChanged; kind++ {
		control := valid
		control.Kind = kind
		if kind != PeerPathDemand {
			control.HoldFor = 0
		}
		if kind == PeerPathRevoke {
			control.ValidFor = 0
		}
		encoded, err := EncodePeerPathControl(control)
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodePeerPathControl(encoded)
		if err != nil || decoded != control {
			t.Fatalf("roundtrip=%+v %v", decoded, err)
		}
		noncanonical := append([]byte{0x98, 8}, encoded[1:]...)
		if _, err := DecodePeerPathControl(noncanonical); err == nil {
			t.Fatal("noncanonical array accepted")
		}
	}
	for _, mutate := range []func(*PeerPathControl){
		func(c *PeerPathControl) { c.ProviderProfile = string(bytes.Repeat([]byte("a"), 65)) },
		func(c *PeerPathControl) { c.ProviderProfile = "untrusted profile" },
		func(c *PeerPathControl) { c.PeerPathID = [16]byte{} }, func(c *PeerPathControl) { c.NetworkGenerationID = [16]byte{} },
		func(c *PeerPathControl) { c.ControlSequence = 0 }, func(c *PeerPathControl) { c.Kind = 0 }, func(c *PeerPathControl) { c.Kind = 5 },
		func(c *PeerPathControl) { c.ValidFor = MaxPeerPathControlLifetime + time.Millisecond }, func(c *PeerPathControl) { c.ValidFor = -1 },
		func(c *PeerPathControl) { c.ValidFor = time.Nanosecond }, func(c *PeerPathControl) { c.HoldFor = time.Nanosecond },
		func(c *PeerPathControl) { c.HoldFor = -1 }, func(c *PeerPathControl) { c.HoldFor = c.ValidFor + time.Millisecond },
		func(c *PeerPathControl) { c.Kind = PeerPathRevoke }, func(c *PeerPathControl) { c.Kind = PeerPathMappingReady },
	} {
		control := valid
		mutate(&control)
		if _, err := EncodePeerPathControl(control); err == nil {
			t.Fatalf("accepted %+v", control)
		}
	}
	for _, body := range [][]byte{nil, {0x80}, {0x87, 1}, {0x87, 1, 1, 1, 1, 1, 1, 1}, bytes.Repeat([]byte{0xff}, MaxPeerPathControlBodyBytes+1)} {
		if _, err := DecodePeerPathControl(body); err == nil {
			t.Fatal("malformed body accepted")
		}
	}
}

func TestPeerPathControlIsSessionBoundWithoutOperationAuthority(t *testing.T) {
	table, err := NewOperationTable(OperationLimits{MaxActive: 1, MaxTombstones: 2}, time.Now)
	if err != nil {
		t.Fatal(err)
	}
	body, err := EncodePeerPathControl(PeerPathControl{PeerPathID: [16]byte{1}, NetworkGenerationID: [16]byte{2}, ControlSequence: 1, Kind: PeerPathRevoke})
	if err != nil {
		t.Fatal(err)
	}
	message, err := NewMessage(MessagePeerPathControl, nil, body)
	if err != nil {
		t.Fatal(err)
	}
	id := OperationID{1}
	if _, err := NewMessage(MessagePeerPathControl, &id, body); err == nil {
		t.Fatal("path control acquired operation identity")
	}
	for _, direction := range []Direction{DirectionReceiverToSender, DirectionSenderToReceiver} {
		admission, err := table.AdmitOutbound(direction, message, OutboundOperationPermit{})
		if err != nil || admission.Disposition != OperationDeliver || !admission.Generation.IsZero() {
			t.Fatalf("outbound=%+v %v", admission, err)
		}
		inbound, err := table.ObserveInbound(direction, message)
		if err != nil || inbound.Disposition != OperationDeliver || !inbound.Generation.IsZero() {
			t.Fatalf("inbound=%+v %v", inbound, err)
		}
	}
	if err := table.TerminateLocal(); err != nil {
		t.Fatal(err)
	}
	admitted, err := table.ObserveInbound(DirectionReceiverToSender, message)
	if err != nil || admitted.Disposition != OperationDrop {
		t.Fatalf("post-terminal=%+v %v", admitted, err)
	}
}

func TestPeerFailureNamespaceNeverGrantsUnknownAuthority(t *testing.T) {
	for code := uint32(0); code <= 65535; code++ {
		scope := PeerFailureScope(uint16(code))
		switch uint16(code) {
		case PeerOperationCodeNegotiation, PeerOperationCodeTimeout, PeerOperationCodeICE, PeerOperationCodeSTUN, PeerOperationCodeTransport, PeerOperationCodeDTLS:
			if scope != PeerFailureAttemptTransient {
				t.Fatal(code, scope)
			}
		case PeerOperationCodeAuthentication, PeerOperationCodeSessionInvariant:
			if scope != PeerFailureSessionTerminal {
				t.Fatal(code, scope)
			}
		default:
			if scope != PeerFailurePathTerminal {
				t.Fatal(code, scope)
			}
		}
	}
	for _, code := range []uint16{0x5000, 0x500c, 0x5fff} {
		encoded, err := EncodeOperationFailure(OperationFailure{Scope: OperationScopePeer, PeerAttempt: testPeerAttemptBinding(), Code: code, Message: "unknown peer reason"})
		if err != nil {
			t.Fatal(err)
		}
		decoded, err := DecodeOperationFailure(encoded)
		if err != nil || decoded.Code != code {
			t.Fatal(decoded, err)
		}
	}
}
