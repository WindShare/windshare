package session

import (
	"errors"
	"fmt"
	"testing"

	"github.com/windshare/windshare/core/session/protocolsession"
)

func peerTerminal(t *testing.T, code uint16) error {
	t.Helper()
	body, err := protocolsession.EncodeSessionTerminal(protocolsession.SessionTerminal{Code: code, Message: "Sender closed"})
	if err != nil {
		t.Fatal(err)
	}
	message, err := protocolsession.NewMessage(protocolsession.MessageSessionTerminal, nil, body)
	if err != nil {
		t.Fatal(err)
	}
	return protocolsession.NewPeerSessionTerminalError(message)
}

func TestPeerStoppedRequiresTheAuthenticatedStopCode(t *testing.T) {
	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{"wrapped stop terminal", fmt.Errorf("pump failed: %w", peerTerminal(t, SessionStoppedCode)), true},
		{"other terminal code", peerTerminal(t, SessionStoppedCode-1), false},
		{"bare sentinel", protocolsession.ErrPeerSessionTerminal, false},
		{"empty carrier", &protocolsession.PeerSessionTerminalError{}, false},
		{"unrelated", errors.New("relay closed"), false},
		{"nil", nil, false},
	} {
		if got := PeerStopped(test.err); got != test.want {
			t.Fatalf("%s: PeerStopped = %t, want %t", test.name, got, test.want)
		}
	}
}
