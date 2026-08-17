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

	for _, test := range []struct {
		name    string
		stage   PeerAttemptStage
		hasLane bool
		valid   bool
	}{
		{name: "datachannel without lane", stage: PeerDataChannelOpen, valid: true},
		{name: "datachannel with lane", stage: PeerDataChannelOpen, hasLane: true},
		{name: "admission started without lane", stage: PeerLaneAdmissionStarted},
		{name: "admission started with lane", stage: PeerLaneAdmissionStarted, hasLane: true, valid: true},
		{name: "admitted without lane", stage: PeerAttemptAdmitted},
		{name: "admitted with lane", stage: PeerAttemptAdmitted, hasLane: true, valid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			spec := PeerAttemptSpec{
				Command: CommandShare, Session: session, PeerPath: path,
				Attempt: attempt, Sequence: 1, Stage: test.stage,
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
