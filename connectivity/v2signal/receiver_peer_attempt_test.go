package v2signal

import (
	"bytes"
	"crypto/ed25519"
	"errors"
	"github.com/windshare/windshare/core/session/protocolsession"
	"testing"
	"time"
)

func TestRetiredPeerContinuationSurvivesOperationGC(t *testing.T) {
	now := time.Unix(1, 0)
	table, err := protocolsession.NewOperationTableWithContinuations(protocolsession.OperationLimits{MaxActive: 4, MaxTombstones: 4}, func() time.Time { return now }, ReceiverControlValidator{})
	if err != nil {
		t.Fatal(err)
	}
	binding := testBinding()
	oldID := protocolsession.OperationID{1}
	offer := func(id protocolsession.OperationID, b Binding) protocolsession.Message {
		body, e := EncodeOffer(Offer{Binding: b, SDP: "v=0"})
		if e != nil {
			t.Fatal(e)
		}
		m, e := protocolsession.NewMessage(protocolsession.MessagePeerOffer, &id, body)
		if e != nil {
			t.Fatal(e)
		}
		return m
	}
	admitted, err := table.ObserveInbound(protocolsession.DirectionReceiverToSender, offer(oldID, binding))
	if err != nil {
		t.Fatal(err)
	}
	if err = table.CancelGeneration(admitted.Generation); err != nil {
		t.Fatal(err)
	}
	now = now.Add(protocolsession.OperationTombstoneLifetime + time.Millisecond)
	current := binding
	current.AttemptSequence++
	current.AttemptID[0]++
	currentID := protocolsession.OperationID{2}
	currentAdmission, err := table.ObserveInbound(protocolsession.DirectionReceiverToSender, offer(currentID, current))
	if err != nil {
		t.Fatal(err)
	}
	continuation := func(id protocolsession.OperationID, b Binding, kind protocolsession.MessageKind) protocolsession.Message {
		var body []byte
		var e error
		switch kind {
		case protocolsession.MessagePeerAnswer:
			body, e = EncodeAnswer(Answer{Binding: b, SDP: "v=0"})
		case protocolsession.MessagePeerCandidate:
			body, e = EncodeCandidate(Candidate{Binding: b, Candidate: "candidate:late"})
		default:
			pb := protocolBinding(b)
			body, e = protocolsession.EncodeOperationFailure(protocolsession.OperationFailure{Scope: protocolsession.OperationScopePeer, Code: protocolsession.PeerOperationCodeAuthentication, Message: "late failure", PeerAttempt: &pb})
		}
		if e != nil {
			t.Fatal(e)
		}
		// Different lane identities sign one immutable negotiation binding.
		key := ed25519.NewKeyFromSeed(peerVectorBytes(0xa1, ed25519.SeedSize))
		base := protocolsession.ControlBinding{ShareInstance: mustPeerShare(t, peerVectorBytes(0x41, 16)), ProtocolSessionID: mustPeerSession(t, peerVectorBytes(0x61, 16)), LaneID: uint32(id[0]), LaneEpoch: 1, Sequence: 1, Direction: protocolsession.DirectionSenderToReceiver, MessageKind: kind, OperationID: id, HasOperationID: true}
		domain := protocolsession.ControlDomainOperation
		if kind == protocolsession.MessageOperationError {
			domain = protocolsession.ControlDomainOperation
		}
		signed, e := protocolsession.SignControlBody(key, domain, base, body)
		if e != nil {
			t.Fatal(e)
		}
		verified, e := protocolsession.VerifyControlBody(key.Public().(ed25519.PublicKey), domain, base, signed)
		if e != nil || !bytes.Equal(verified, body) {
			t.Fatal("signature verification", e)
		}
		m, e := protocolsession.NewMessage(kind, &id, signed)
		if e != nil {
			t.Fatal(e)
		}
		return m
	}
	for _, kind := range []protocolsession.MessageKind{protocolsession.MessagePeerAnswer, protocolsession.MessagePeerCandidate, protocolsession.MessageOperationError} {
		if d, e := table.Observe(protocolsession.DirectionSenderToReceiver, continuation(oldID, binding, kind)); e != nil || d != protocolsession.OperationDrop {
			t.Fatalf("late %d: %d %v", kind, d, e)
		}
		unknown := protocolsession.OperationID{3}
		future := current
		future.AttemptSequence++
		otherPath := binding
		otherPath.PeerPathID[0]++
		for _, wrong := range []Binding{future, otherPath} {
			if _, e := table.Observe(protocolsession.DirectionSenderToReceiver, continuation(unknown, wrong, kind)); !errors.Is(e, protocolsession.ErrUnknownOperation) {
				t.Fatalf("unknown binding %d: %v", kind, e)
			}
		}
		if _, e := table.Observe(protocolsession.DirectionSenderToReceiver, continuation(unknown, current, kind)); !errors.Is(e, protocolsession.ErrUnknownOperation) {
			t.Fatalf("current wrong operation %d: %v", kind, e)
		}
		if _, e := table.Observe(protocolsession.DirectionSenderToReceiver, continuation(currentID, binding, kind)); !errors.Is(e, protocolsession.ErrConflictingContinuation) {
			t.Fatalf("wrong current binding %d: %v", kind, e)
		}
	}
	if d, e := table.Observe(protocolsession.DirectionReceiverToSender, offer(protocolsession.OperationID{4}, binding)); e != nil || d != protocolsession.OperationDrop {
		t.Fatalf("superseded offer: %d %v", d, e)
	}
	if table.ActiveCount() != 1 || table.TombstoneCount() != 0 || table.Terminated() {
		t.Fatal("late traffic changed fresh authority")
	}
	// Repeated tombstone collection retains a bounded path boundary and must
	// never hide an older live attempt behind a later retired watermark.
	for sequence := uint64(3); sequence < 131; sequence++ {
		next := binding
		next.AttemptSequence = sequence
		next.AttemptID[0] = byte(sequence)
		id := protocolsession.OperationID{byte(sequence), 1}
		admission, e := table.ObserveInbound(protocolsession.DirectionReceiverToSender, offer(id, next))
		if e != nil {
			t.Fatal(e)
		}
		if e = table.CancelGeneration(admission.Generation); e != nil {
			t.Fatal(e)
		}
		now = now.Add(protocolsession.OperationTombstoneLifetime + time.Millisecond)
	}
	wrongID := protocolsession.OperationID{250}
	if _, e := table.Observe(protocolsession.DirectionSenderToReceiver, continuation(wrongID, current, protocolsession.MessageOperationError)); !errors.Is(e, protocolsession.ErrUnknownOperation) {
		t.Fatal("live older sequence lost exact operation protection", e)
	}
	if e := table.CancelGeneration(currentAdmission.Generation); e != nil {
		t.Fatal(e)
	}
	now = now.Add(protocolsession.OperationTombstoneLifetime + time.Millisecond)
	if d, e := table.Observe(protocolsession.DirectionSenderToReceiver, continuation(wrongID, current, protocolsession.MessageOperationError)); e != nil || d != protocolsession.OperationDrop {
		t.Fatal("retired older sequence was not collected", d, e)
	}
	for index := 1; index <= protocolsession.MaximumPeerContinuationPaths; index++ {
		next := binding
		next.PeerPathID[0] += byte(index)
		admission, e := table.ObserveInbound(protocolsession.DirectionReceiverToSender, offer(protocolsession.OperationID{byte(index), 2}, next))
		if index == protocolsession.MaximumPeerContinuationPaths {
			if !errors.Is(e, protocolsession.ErrContinuationAuthority) {
				t.Fatal("path budget expanded", e)
			}
		} else if e != nil {
			t.Fatal(e)
		} else if e = table.CancelGeneration(admission.Generation); e != nil {
			t.Fatal(e)
		}
	}
}
