package clievent

import "testing"

func TestPeerAttemptLaneRequirementMatchesAdmissionLifecycle(t *testing.T) {
	session, err := NewProtocolSessionID(bytes16(1))
	if err != nil {
		t.Fatal(err)
	}
	path, err := NewPeerPathID(bytes16(2))
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := NewPeerAttemptID(bytes16(3))
	if err != nil {
		t.Fatal(err)
	}
	lane, err := NewLaneIdentity(2, 1)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := NewProtocolOperationID(bytes16(4))
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name    string
		stage   PeerAttemptStage
		hasLane bool
		valid   bool
	}{
		{name: "datachannel without lane", stage: PeerDataChannelOpen, valid: true},
		{name: "datachannel with lane", stage: PeerDataChannelOpen, hasLane: true},
		{name: "lane hello without lane", stage: PeerLaneHelloAuthenticated},
		{name: "lane hello with lane", stage: PeerLaneHelloAuthenticated, hasLane: true, valid: true},
		{name: "admitted without lane", stage: PeerAttemptAdmitted},
		{name: "admitted with lane", stage: PeerAttemptAdmitted, hasLane: true, valid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := PeerAttemptSpec{
				Command: CommandShare, Session: session, PeerPath: path,
				Attempt: attempt, Sequence: 1, Stage: test.stage,
			}
			if test.stage == PeerLaneHelloAuthenticated || test.stage == PeerAttemptAdmitted {
				spec.Phase = PeerPhaseAdmission
				spec.GrantOperation = grant
				spec.HasGrantOperation = true
			}
			if test.stage == PeerAttemptAdmitted {
				spec.AdmissionDisposition = PeerAdmissionAccepted
				spec.ResponseDelivery = PeerResponseDelivered
			}
			if test.hasLane {
				spec.Lane = lane
				spec.HasLane = true
			}
			_, err := NewPeerAttemptObserved(spec)
			if (err == nil) != test.valid {
				t.Fatalf("NewPeerAttemptObserved() error = %v, valid = %t", err, test.valid)
			}
		})
	}
}
