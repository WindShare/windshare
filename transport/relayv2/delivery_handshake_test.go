package relayv2

import (
	"crypto/ed25519"
	"errors"
	"testing"

	v2 "github.com/windshare/windshare/relay/protocol/v2"
	"github.com/windshare/windshare/transport/relayv2/internal/receiveledger"
)

func TestRegistrationBindsDeclaredCapacityToProofAndRuntime(t *testing.T) {
	for _, mode := range []v2.RegistrationMode{v2.RegistrationFresh, v2.RegistrationResume} {
		name := "register"
		if mode == v2.RegistrationResume {
			name = "resume"
		}
		t.Run(name, func(t *testing.T) {
			fixture := newClientFixture(t)
			init := fixture.fresh
			init.Mode = mode
			socket := newScriptedSocket()
			purpose := v2.ChallengeRegister
			if mode == v2.RegistrationResume {
				purpose = v2.ChallengeResume
			}
			challengeBytes := challengeFrame(t, purpose)
			socket.respond(challengeBytes)
			registered, _ := (v2.Registered{ShareID: init.ShareID, ShareInstance: init.ShareInstance, DescriptorDigest: init.DescriptorDigest}).MarshalBinary()
			socket.respond(registered)
			capacity := v2.ReceiveCapacity{Frames: 3, EncodedBytes: 3 * (v2.OpaqueRouteHeaderBytes + 1)}
			sender, err := DialSender(t.Context(), SenderConfig{
				RelayBaseURL: "https://relay.example", Init: init, SenderPrivateKey: fixture.privateKey,
				Descriptor: fixture.descriptor, ResumeToken: fixture.token,
				Dial: DialOptions{SocketDialer: socket.dial, ReceiveCapacity: capacity},
			})
			if err != nil {
				t.Fatal(err)
			}
			defer sender.Close()
			wireInit, err := v2.ParseDeliveryRegisterInit(assertWriteMagic(t, socket, "WS2R"))
			if err != nil || wireInit.ReceiveCapacity != capacity {
				t.Fatal(wireInit, err)
			}
			if mode == v2.RegistrationResume {
				assertWriteMagic(t, socket, "WS2T")
			}
			proof, err := v2.ParseRegisterProof(assertWriteMagic(t, socket, "WS2P"))
			if err != nil {
				t.Fatal(err)
			}
			challenge, _ := v2.ParseChallenge(challengeBytes)
			preimage, err := v2.DeliveryRegistrationPreimage(wireInit, challenge, sender.Endpoint().Identity)
			if err != nil || !ed25519.Verify(proof.SenderPublicKey[:], preimage, proof.Signature[:]) {
				t.Fatal("runtime registration proof omitted capacity", err)
			}
			channel := sender.link.installFixed(relaySessionID(90))
			assertDeclaredBuffer(t, channel, capacity.Frames)
		})
	}
}

func TestJoinUsesItsDeclaredReceiveCapacity(t *testing.T) {
	socket := newScriptedSocket()
	id := relaySessionID(91)
	delivery, _ := (v2.DescriptorDelivery{RelaySessionID: id, Object: []byte{1}}).MarshalBinary()
	socket.respond(delivery)
	capacity := v2.ReceiveCapacity{Frames: 2, EncodedBytes: 2 * (v2.OpaqueRouteHeaderBytes + 1)}
	receiver, err := DialReceiver(t.Context(), ReceiverConfig{
		RelayBaseURL: "https://relay.example", ShareID: newClientFixture(t).fresh.ShareID,
		Dial: DialOptions{SocketDialer: socket.dial, ReceiveCapacity: capacity},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer receiver.Close()
	join, err := v2.ParseDeliveryJoin(assertWriteMagic(t, socket, "WS2J"))
	if err != nil || join.ReceiveCapacity != capacity {
		t.Fatal(join, err)
	}
	assertDeclaredBuffer(t, receiver.Channel(), capacity.Frames)
}

func assertDeclaredBuffer(t *testing.T, channel *Channel, frames uint64) {
	t.Helper()
	for range frames {
		if err := channel.deliver([]byte{1}); err != nil {
			t.Fatal(err)
		}
	}
	if err := channel.deliver([]byte{1}); !errors.Is(err, receiveledger.ErrCapacity) {
		t.Fatal("runtime did not enforce the declared connection capacity", err)
	}
}

func TestPartialReceiveCapacityFailsBeforeDial(t *testing.T) {
	for _, capacity := range []v2.ReceiveCapacity{{Frames: 1}, {EncodedBytes: 1}} {
		dial := DialOptions{ReceiveCapacity: capacity}
		if _, err := DialSender(t.Context(), SenderConfig{Dial: dial}); !errors.Is(err, ErrReceiveCapacity) {
			t.Fatal(err)
		}
		if _, err := DialReceiver(t.Context(), ReceiverConfig{Dial: dial}); !errors.Is(err, ErrReceiveCapacity) {
			t.Fatal(err)
		}
	}
}
