package browsermatrixpion

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	pion "github.com/pion/webrtc/v4"

	"github.com/windshare/windshare/internal/testloopback"
)

func TestPionAttemptEchoesAcrossRealLoopbackSelectedPair(t *testing.T) {
	loopback := testloopback.New(t)
	remoteAPI := loopback.NewPionAPI()
	localAPI := loopback.NewPionAPI()
	port := uint16(remoteAPI.LocalAddr().Port)
	var traceMu sync.Mutex
	var traces []TraceEvent
	factory, err := NewPionAttemptFactory(PionAttemptFactoryConfig{
		InstanceID: "remote-a", PeerConnections: remoteAPI,
		Trace: func(event TraceEvent) {
			traceMu.Lock()
			traces = append(traces, event)
			traceMu.Unlock()
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	attemptID := strings.Repeat("a", 22)
	challenge := strings.Repeat("c", 43)
	authority := testAttemptAuthority(attemptID, challenge)
	attemptValue, err := factory.Create(context.Background(), authority)
	if err != nil {
		t.Fatal(err)
	}
	attempt := attemptValue.(*pionAttempt)
	t.Cleanup(func() {
		if err := attempt.Close(); err != nil {
			t.Errorf("close remote Pion attempt: %v", err)
		}
	})

	local, err := localAPI.NewPeerConnection(pion.Configuration{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := local.Close(); err != nil {
			t.Errorf("close local Pion peer: %v", err)
		}
	})
	channel, err := local.CreateDataChannel("matrix-echo", nil)
	if err != nil {
		t.Fatal(err)
	}
	echoed := make(chan string, 1)
	channel.OnOpen(func() {
		frame := challengeFrame(t, authority)
		if sendErr := channel.SendText(frame); sendErr != nil {
			echoed <- "send-failed"
		}
	})
	channel.OnMessage(func(message pion.DataChannelMessage) { echoed <- string(message.Data) })

	offer, err := local.CreateOffer(nil)
	if err != nil {
		t.Fatal(err)
	}
	gathered := pion.GatheringCompletePromise(local)
	if err := local.SetLocalDescription(offer); err != nil {
		t.Fatal(err)
	}
	select {
	case <-gathered:
	case <-time.After(5 * time.Second):
		t.Fatal("local candidate gathering timed out")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	answerSDP, err := attempt.Offer(ctx, local.LocalDescription().SDP)
	if err != nil {
		t.Fatal(err)
	}
	if err := local.SetRemoteDescription(pion.SessionDescription{Type: pion.SDPTypeAnswer, SDP: answerSDP}); err != nil {
		t.Fatal(err)
	}
	select {
	case message := <-echoed:
		if message != challengeFrame(t, authority) {
			t.Fatalf("wrong echo: %q", message)
		}
	case <-ctx.Done():
		t.Fatal("data-channel echo timed out")
	}
	for {
		result := attempt.Result()
		if result.State == attemptStateEstablished {
			if result.SelectedPair == nil || result.SelectedPair.Local.Protocol != protocolUDP ||
				result.SelectedPair.Local.Port != port ||
				result.ChallengeBindingSHA256 != challengeBindingSHA256(authority) {
				t.Fatalf("selected pair is not bound to the authorized UDP endpoint: %#v", result.SelectedPair)
			}
			for {
				traceMu.Lock()
				pairSeen, challengeSeen := false, false
				for _, event := range traces {
					pairSeen = pairSeen || event.Milestone == tracePairObserved
					challengeSeen = challengeSeen || event.Milestone == traceChallengeObserved
				}
				traceMu.Unlock()
				if pairSeen && challengeSeen {
					break
				}
				select {
				case <-ctx.Done():
					t.Fatal("attempt proof traces were not emitted")
				case <-time.After(10 * time.Millisecond):
				}
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatalf("attempt did not publish selected pair: %#v", result)
		case <-time.After(10 * time.Millisecond):
		}
	}
	if _, err := attempt.Offer(ctx, offer.SDP); err == nil {
		t.Fatal("attempt accepted a second offer")
	}
	if err := attempt.Close(); err != nil {
		t.Fatal(err)
	}
	if err := attempt.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestPionEndpointAPIRejectsInvalidAuthority(t *testing.T) {
	for _, config := range []PionEndpointConfig{
		{},
		{PublicIP: "2001:db8::1", UDPPortMin: 1, UDPPortMax: 2},
		{PublicIP: "127.0.0.1", UDPPortMin: 2, UDPPortMax: 1},
	} {
		if _, err := NewPionEndpointAPI(config); err == nil {
			t.Fatalf("invalid endpoint authority accepted: %#v", config)
		}
	}
}

func TestPionAttemptRejectsInvalidAuthorityAndOffer(t *testing.T) {
	loopback := testloopback.New(t)
	if _, err := NewPionAttemptFactory(PionAttemptFactoryConfig{}); err == nil {
		t.Fatal("factory accepted an absent instance and PeerConnection API")
	}
	factory, err := NewPionAttemptFactory(PionAttemptFactoryConfig{
		InstanceID: "remote-a", PeerConnections: loopback.NewPionAPI(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := factory.Create(context.Background(), testAttemptAuthority("short", strings.Repeat("c", 43))); err == nil {
		t.Fatal("invalid attempt ID accepted")
	}
	if _, err := factory.Create(context.Background(), testAttemptAuthority(strings.Repeat("b", 22), "short")); err == nil {
		t.Fatal("invalid challenge accepted")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := factory.Create(cancelled, testAttemptAuthority(strings.Repeat("b", 22), strings.Repeat("c", 43))); err == nil {
		t.Fatal("cancelled creation authority accepted")
	}
	attemptValue, err := factory.Create(context.Background(), testAttemptAuthority(
		strings.Repeat("b", 22), strings.Repeat("c", 43),
	))
	if err != nil {
		t.Fatal(err)
	}
	attempt := attemptValue.(*pionAttempt)
	t.Cleanup(func() {
		if err := attempt.Close(); err != nil {
			t.Errorf("close rejected-offer Pion attempt: %v", err)
		}
	})
	if _, err := attempt.Offer(context.Background(), "not-sdp"); err == nil {
		t.Fatal("invalid SDP accepted")
	}
	result := attempt.Result()
	if result.State != attemptStateFailed || result.FailureCode == nil || *result.FailureCode != "invalid-offer" {
		t.Fatalf("invalid offer failure is not observable: %#v", result)
	}
}

type absentPeerConnectionAPI struct{}

func (absentPeerConnectionAPI) NewPeerConnection(
	pion.Configuration,
) (*pion.PeerConnection, error) {
	return nil, nil
}

func TestPionAttemptFactoryRejectsAbsentInjectedPeer(t *testing.T) {
	factory, err := NewPionAttemptFactory(PionAttemptFactoryConfig{
		InstanceID: "remote-a", PeerConnections: absentPeerConnectionAPI{},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := factory.Create(context.Background(), testAttemptAuthority(
		strings.Repeat("b", 22), strings.Repeat("c", 43),
	)); err == nil {
		t.Fatal("injected API returned no PeerConnection without failing closed")
	}
}

type fakeChallengeEchoer struct {
	err    error
	echoed []string
}

func (echoer *fakeChallengeEchoer) SendText(value string) error {
	echoer.echoed = append(echoer.echoed, value)
	return echoer.err
}

func TestChallengeProofFailsClosed(t *testing.T) {
	attemptID := strings.Repeat("a", 22)
	challenge := strings.Repeat("c", 43)
	authority := testAttemptAuthority(attemptID, challenge)
	validFrame := challengeFrame(t, authority)
	pair := &SelectedPairEvidence{}

	t.Run("wrong", func(t *testing.T) {
		attempt := testPionAttempt(authority, pair)
		wrong := authority
		wrong.Challenge = strings.Repeat("d", 43)
		attempt.onChallenge(&fakeChallengeEchoer{}, pion.DataChannelMessage{IsString: true, Data: []byte(challengeFrame(t, wrong))})
		assertAttemptFailure(t, attempt, "data-channel-challenge-invalid")
	})
	t.Run("noncanonical-order", func(t *testing.T) {
		reordered, err := json.Marshal(map[string]any{
			"attemptAuthority": authority,
			"protocolVersion":  ProtocolVersion,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := parseDataChannelChallenge(pion.DataChannelMessage{
			IsString: true, Data: reordered,
		}); err == nil {
			t.Fatal("noncanonical challenge field order accepted")
		}
	})
	t.Run("replay", func(t *testing.T) {
		attempt := testPionAttempt(authority, nil)
		echoer := &fakeChallengeEchoer{}
		message := pion.DataChannelMessage{IsString: true, Data: []byte(validFrame)}
		attempt.onChallenge(echoer, message)
		attempt.onChallenge(echoer, message)
		assertAttemptFailure(t, attempt, "data-channel-challenge-replayed")
	})
	t.Run("replay-after-establishment", func(t *testing.T) {
		attempt := testPionAttempt(authority, pair)
		echoer := &fakeChallengeEchoer{}
		message := pion.DataChannelMessage{IsString: true, Data: []byte(validFrame)}
		attempt.onChallenge(echoer, message)
		if attempt.Result().State != attemptStateEstablished {
			t.Fatal("first challenge did not establish the attempt")
		}
		attempt.onChallenge(echoer, message)
		assertAttemptFailure(t, attempt, "data-channel-challenge-replayed")
	})
	t.Run("echo", func(t *testing.T) {
		attempt := testPionAttempt(authority, pair)
		attempt.onChallenge(&fakeChallengeEchoer{err: errors.New("send failed")}, pion.DataChannelMessage{IsString: true, Data: []byte(validFrame)})
		assertAttemptFailure(t, attempt, "data-channel-echo-failed")
	})
	t.Run("success", func(t *testing.T) {
		attempt := testPionAttempt(authority, pair)
		echoer := &fakeChallengeEchoer{}
		attempt.onChallenge(echoer, pion.DataChannelMessage{IsString: true, Data: []byte(validFrame)})
		if attempt.Result().State != attemptStateEstablished || len(echoer.echoed) != 1 || echoer.echoed[0] != validFrame {
			t.Fatalf("challenge proof did not establish exact echo: %#v", attempt.Result())
		}
	})
}

func challengeFrame(t *testing.T, authority AttemptAuthority) string {
	t.Helper()
	encoded, err := json.Marshal(DataChannelChallenge{
		ProtocolVersion: ProtocolVersion, AttemptAuthority: authority,
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(encoded)
}

func testAttemptAuthority(attemptID, challenge string) AttemptAuthority {
	binding := testAttemptBinding()
	return AttemptAuthority{
		SchemaVersion: AttemptAuthoritySchemaVersion,
		RequestAuthority: testAttemptRequestAuthority(
			binding,
			"request-00000001",
		),
		AttemptID: attemptID,
		Challenge: challenge,
	}
}

func testAttemptBinding() AttemptBinding {
	sampleAuthority, err := NewSampleAuthority(
		"matrix-run-00000001",
		"scheduled-public-stun",
		"chromium",
		1,
		"matrix-process-00000001",
	)
	if err != nil {
		panic(err)
	}
	return AttemptBinding{
		ControlAuthority: ControlAuthority{
			SchemaVersion:   ControlAuthoritySchemaVersion,
			SampleAuthority: sampleAuthority,
			ControlLeaseID:  "control-lease-00000001",
		},
		FixtureBinding: AttemptFixtureBinding{
			AttestationSHA256:       strings.Repeat("a", 64),
			AuthorityInstanceID:     "authority-00000001",
			RemoteServiceInstanceID: "remote-a",
			NetworkBindingSHA256:    strings.Repeat("b", 64),
			RemotePeerBindingSHA256: strings.Repeat("c", 64),
		},
	}
}

func testAttemptRequestAuthority(binding AttemptBinding, requestID string) AttemptRequestAuthority {
	return AttemptRequestAuthority{
		SchemaVersion:    AttemptRequestAuthoritySchemaVersion,
		ControlAuthority: binding.ControlAuthority,
		RequestID:        requestID,
		FixtureBinding:   binding.FixtureBinding,
	}
}

func testCreateAttemptRequest(binding AttemptBinding, requestID string, leaseMillis int64) CreateAttemptRequest {
	return CreateAttemptRequest{
		ProtocolVersion:  ProtocolVersion,
		RequestAuthority: testAttemptRequestAuthority(binding, requestID),
		LeaseMillis:      leaseMillis,
	}
}

func attemptBindingFromAuthority(authority AttemptAuthority) AttemptBinding {
	return AttemptBinding{
		ControlAuthority: authority.RequestAuthority.ControlAuthority,
		FixtureBinding:   authority.RequestAuthority.FixtureBinding,
	}
}

func testPionAttempt(authority AttemptAuthority, selectedPair *SelectedPairEvidence) *pionAttempt {
	return &pionAttempt{
		authority: authority,
		result: AttemptResult{
			ProtocolVersion:  ProtocolVersion,
			AttemptAuthority: authority,
			State:            attemptStatePending,
			SelectedPair:     selectedPair,
		},
	}
}

func assertAttemptFailure(t *testing.T, attempt *pionAttempt, want string) {
	t.Helper()
	result := attempt.Result()
	if result.State != attemptStateFailed || result.FailureCode == nil || *result.FailureCode != want || result.SelectedPair != nil {
		t.Fatalf("failure=%#v want=%q", result, want)
	}
}

func TestAttemptFailureStatesAndCandidateProjection(t *testing.T) {
	attempt := &pionAttempt{result: AttemptResult{State: attemptStatePending}}
	attempt.onICEState(pion.ICEConnectionStateFailed)
	if attempt.Result().State != attemptStateFailed {
		t.Fatal("ICE failure was not recorded")
	}
	pair := &SelectedPairEvidence{}
	attempt = &pionAttempt{result: AttemptResult{State: attemptStatePending, SelectedPair: pair}}
	attempt.onICEState(pion.ICEConnectionStateClosed)
	assertAttemptFailure(t, attempt, "attempt-closed")
	attempt = &pionAttempt{result: AttemptResult{State: attemptStatePending}}
	attempt.onICEState(pion.ICEConnectionStateConnected)
	if attempt.Result().FailureCode == nil || *attempt.Result().FailureCode != "selected-pair-unavailable" {
		t.Fatal("missing selected pair was not fail-closed")
	}
	if _, err := selectedPair(nil); err == nil {
		t.Fatal("nil peer produced selected pair")
	}
	for _, test := range []struct {
		address, family string
	}{
		{"192.0.2.1", "ipv4"}, {"2001:db8::1", "ipv6"}, {"invalid", "unknown"},
	} {
		candidate := candidateEvidence(&pion.ICECandidate{
			Address: test.address, Protocol: pion.ICEProtocolUDP, Port: 1234, Typ: pion.ICECandidateTypeHost,
		})
		if candidate.AddressFamily != test.family || candidate.Protocol != protocolUDP {
			t.Fatalf("candidate projection=%#v", candidate)
		}
	}
}
