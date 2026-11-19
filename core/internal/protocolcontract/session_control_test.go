package protocolcontract

import (
	"strconv"
	"testing"
	"time"

	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/turncontrol"
)

type sessionControlVector struct {
	Name                 string                      `json:"name"`
	Kind                 protocolsession.MessageKind `json:"kind"`
	Direction            protocolsession.Direction   `json:"direction"`
	BodyB64              string                      `json:"bodyB64"`
	PlaintextB64         string                      `json:"plaintextB64"`
	SenderPublicKeyB64   string                      `json:"senderPublicKeyB64"`
	ShareInstanceB64     string                      `json:"shareInstanceB64"`
	ProtocolSessionIDB64 string                      `json:"protocolSessionIdB64"`
	LaneID               uint32                      `json:"laneId"`
	LaneEpoch            uint32                      `json:"laneEpoch"`
	Sequence             string                      `json:"sequence"`
}

func sessionControlCases(t *testing.T, f *fixture) []any {
	t.Helper()
	header := turncontrol.Header{SessionID: [16]byte{1}, OwnerPathID: [16]byte{2}, Sequence: 3}
	path := protocolsession.PeerPathControl{
		PeerPathID: [16]byte{2}, NetworkGenerationID: [16]byte{3}, ControlSequence: 4,
		Kind: protocolsession.PeerPathDemand, ValidFor: time.Minute, HoldFor: time.Second,
	}
	demand, err := protocolsession.EncodePeerPathControl(path)
	if err != nil {
		t.Fatal(err)
	}
	path.Kind, path.HoldFor = protocolsession.PeerPathMappingReady, 0
	ready, err := protocolsession.EncodePeerPathControl(path)
	if err != nil {
		t.Fatal(err)
	}
	turnDemand, err := turncontrol.Encode(turncontrol.Demand{Authority: header, HoldFor: time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	grant, err := turncontrol.Encode(turncontrol.Grant{
		Authority: header, PathID: [16]byte{4}, Provider: "coturn", Account: "home", Profile: "primary",
		CredentialKind: turncontrol.Expiring, Username: "123:user", Password: "delegated-password",
		ValidFor: time.Hour, URLs: []string{"turn:example.org:3478?transport=udp"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var result []any
	for index, control := range []struct {
		name      string
		kind      protocolsession.MessageKind
		direction protocolsession.Direction
		body      []byte
	}{
		{"peer-path-demand", protocolsession.MessagePeerPathControl, protocolsession.DirectionReceiverToSender, demand},
		{"peer-path-mapping-ready", protocolsession.MessagePeerPathControl, protocolsession.DirectionSenderToReceiver, ready},
		{"turn-demand", protocolsession.MessageTURNControl, protocolsession.DirectionReceiverToSender, turnDemand},
		{"turn-grant", protocolsession.MessageTURNControl, protocolsession.DirectionSenderToReceiver, grant},
	} {
		message, err := protocolsession.NewMessage(control.kind, nil, control.body)
		if err != nil {
			t.Fatal(err)
		}
		plaintext, err := protocolsession.EncodeMessage(message)
		if err != nil {
			t.Fatal(err)
		}
		result = append(result, sessionControlVector{
			Name: control.name, Kind: control.kind, Direction: control.direction,
			BodyB64: b64(control.body), PlaintextB64: b64(plaintext), SenderPublicKeyB64: b64(f.edPublic),
			ShareInstanceB64: b64(f.shareInstance), ProtocolSessionIDB64: b64(header.SessionID[:]),
			LaneID: 7, LaneEpoch: 2, Sequence: strconv.Itoa(index + 10),
		})
	}
	return result
}
