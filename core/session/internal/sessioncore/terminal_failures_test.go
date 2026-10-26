package sessioncore

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"errors"
	"github.com/windshare/windshare/core/session"

	"github.com/windshare/windshare/core/session/protocolsession"
	"testing"
	"time"
)

type failingTerminalSealer struct{ err error }

func (sealer failingTerminalSealer) NextSequence() (uint64, error) { return 1, nil }

func (sealer failingTerminalSealer) Seal([]byte) (protocolsession.SealedEnvelope, error) {
	return protocolsession.SealedEnvelope{}, sealer.err
}

func TestDeliveredTerminalPreservesCallerAndHardAdmissionFailures(t *testing.T) {
	body, err := protocolsession.EncodeSessionTerminal(protocolsession.SessionTerminal{
		Code: session.SessionStoppedCode, Message: "share cancelled",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("caller cancellation", func(t *testing.T) {
		sender, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
		sender.start()
		t.Cleanup(sender.close)
		outbound := &Responses{runtime: sender, controlPreparer: testControlPreparer(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize)))}
		callerContext, cancelCaller := context.WithCancel(context.Background())
		cancelCaller()
		err := outbound.sendTerminalRecipients(
			callerContext, body, sender.lanes.snapshot(),
		)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("delivered terminal erased caller cancellation: %v", err)
		}
	})
	t.Run("hard preparation failure", func(t *testing.T) {
		sender, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
		sender.start()
		t.Cleanup(sender.close)
		outbound := &Responses{runtime: sender, controlPreparer: testControlPreparer(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize)))}
		recipients := append([]selectedLane{{identity: session.LaneIdentity{}}}, sender.lanes.snapshot()...)
		err := outbound.sendTerminalRecipients(
			context.Background(), body, recipients,
		)
		if !errors.Is(err, protocolsession.ErrControlBinding) {
			t.Fatalf("delivered terminal erased hard preparation failure: %v", err)
		}
	})
	t.Run("accepted seal failure", func(t *testing.T) {
		sender, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
		sender.start()
		t.Cleanup(sender.close)
		outbound := &Responses{runtime: sender, controlPreparer: testControlPreparer(ed25519.NewKeyFromSeed(bytes.Repeat([]byte{7}, ed25519.SeedSize)))}
		policyRuntime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
		failedChannel := newMemoryChannel(t)
		sealErr := errors.New("terminal seal failed")
		failedWriter, err := protocolsession.NewSessionWriter(
			failedChannel,
			failingTerminalSealer{err: sealErr},
			runtimeLanePolicy{runtime: policyRuntime},
		)
		if err != nil {
			t.Fatal(err)
		}
		writerResult := make(chan error, 1)
		go func() { writerResult <- failedWriter.Run(context.Background()) }()
		recipients := append(
			[]selectedLane{{
				identity: session.LaneIdentity{ID: 99, Epoch: 1},
				channel:  failedChannel,
				writer:   failedWriter,
				done:     failedWriter.Done(),
			}},
			sender.lanes.snapshot()...,
		)
		err = outbound.sendTerminalRecipients(
			context.Background(), body, recipients,
		)
		if !errors.Is(err, sealErr) {
			t.Fatalf("delivered terminal erased accepted seal failure: %v", err)
		}
		select {
		case err := <-writerResult:
			if !errors.Is(err, sealErr) {
				t.Fatalf("failing terminal writer result = %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("failing terminal writer did not settle")
		}
	})
}
