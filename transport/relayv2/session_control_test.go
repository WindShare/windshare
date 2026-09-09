package relayv2

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"

	"github.com/windshare/windshare/core/framechannel"
	v2 "github.com/windshare/windshare/relay/protocol/v2"
)

func TestSenderCreditCancellationAndControlScheduling(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		socket := newScriptedSocket()
		link := newLink(context.Background(), socket, false)
		defer link.stop(nil)
		slow, _ := link.channel(relaySessionID(41))
		fast, _ := link.channel(relaySessionID(42))
		link.start()
		payload := framechannel.Frame("payload")
		for range v2.SenderWindowFrames {
			if err := slow.Send(t.Context(), payload); err != nil {
				t.Fatal(err)
			}
			<-socket.writes
		}
		ctx, cancel := context.WithCancel(t.Context())
		result := make(chan error, 1)
		go func() { result <- slow.Send(ctx, payload) }()
		synctest.Wait()
		if len(socket.writes) != 0 {
			t.Fatal("sender exceeded session credits")
		}
		cancel()
		synctest.Wait()
		if err := <-result; !errors.Is(err, context.Canceled) || framechannel.SendDispositionOf(err) != framechannel.SendRejected {
			t.Fatal("credit-blocked send was not withdrawn before exposure", err)
		}
		if err := slow.ConfirmAdmission(t.Context()); err != nil {
			t.Fatal(err)
		}
		confirmation := <-socket.writes
		if _, err := v2.ParseSessionAdmitted(confirmation.data); err != nil {
			t.Fatal(err)
		}
		if err := fast.Send(t.Context(), payload); err != nil {
			t.Fatal(err)
		}
		sibling := <-socket.writes
		if route, err := v2.ParseOpaqueRoute(sibling.data); err != nil || route.RelaySessionID != fast.id {
			t.Fatal("sibling blocked", err)
		}

		go func() { result <- slow.Send(t.Context(), payload) }()
		synctest.Wait()
		credit := v2.SessionCredit{RelaySessionID: slow.id, Frames: 1, Bytes: uint32(len(payload) + v2.OpaqueRouteHeaderBytes)}
		encoded, err := credit.MarshalBinary()
		if err != nil {
			t.Fatal(err)
		}
		socket.respond(encoded)
		synctest.Wait()
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		<-socket.writes
		if err := slow.Close(); err != nil {
			t.Fatal(err)
		}
		socket.respond(encoded)
		synctest.Wait()
		link.writeMu.Lock()
		_, exists := link.windows[slow.id]
		link.writeMu.Unlock()
		if exists {
			t.Fatal("late credit recreated retired window")
		}
	})
}

func TestRelayCreditRejectsOversizedAndWrongRoleGrants(t *testing.T) {
	for _, fixed := range []bool{false, true} {
		synctest.Test(t, func(t *testing.T) {
			socket := newScriptedSocket()
			link := newLink(context.Background(), socket, fixed)
			channel := link.installFixed(relaySessionID(44))
			link.start()
			if fixed {
				if err := channel.ConfirmAdmission(t.Context()); !errors.Is(err, ErrProtocol) {
					t.Fatal(err)
				}
			}
			credit, _ := (v2.SessionCredit{RelaySessionID: channel.id, Frames: 1, Bytes: 32}).MarshalBinary()
			socket.respond(credit)
			synctest.Wait()
			select {
			case <-link.done:
			default:
				t.Fatal("invalid credit did not retire transport")
			}
			if !errors.Is(link.Err(), ErrProtocol) {
				t.Fatal(link.Err())
			}
		})
	}
}
