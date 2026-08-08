package sessionruntime

import (
	"context"
	"crypto/ed25519"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	framechannel "github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type terminalCancellationRejectionChannel struct {
	protocolsession.FrameChannel
	entered chan struct{}
	once    sync.Once
}

type failingTerminalSealer struct{ err error }

func (sealer failingTerminalSealer) NextSequence() (uint64, error) { return 1, nil }
func (sealer failingTerminalSealer) Seal([]byte) (protocolsession.SealedEnvelope, error) {
	return protocolsession.SealedEnvelope{}, sealer.err
}

func (channel *terminalCancellationRejectionChannel) SendTerminal(
	ctx context.Context,
	_ framechannel.Frame,
) error {
	channel.once.Do(func() { close(channel.entered) })
	<-ctx.Done()
	return framechannel.RejectSend(ctx.Err())
}

func TestEmptyTerminalFanoutDistinguishesCallerFromLifecycleCancellation(t *testing.T) {
	runtime := &runtimeCore{}
	runtime.lanes = newRuntimeLanes(runtime)
	outbound := senderOutbound{runtime: runtime}

	lifecycleContext, cancelLifecycle := context.WithCancel(context.Background())
	cancelLifecycle()
	if err := outbound.sendTerminalAll(lifecycleContext, context.Background(), nil); err != nil {
		t.Fatalf("natural lifecycle cancellation failed empty fanout: %v", err)
	}
	callerContext, cancelCaller := context.WithCancel(context.Background())
	cancelCaller()
	if err := outbound.sendTerminalAll(context.Background(), callerContext, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation across empty fanout = %v", err)
	}
}

func TestTerminalPreAdmissionWriterStopRequiresNoUsableReplacement(t *testing.T) {
	body, err := protocolsession.EncodeSessionTerminal(protocolsession.SessionTerminal{
		Code: SessionStoppedCode, Message: "share cancelled",
	})
	if err != nil {
		t.Fatal(err)
	}
	privateKey := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	for _, test := range []struct {
		name           string
		addReplacement bool
		wantError      bool
	}{
		{name: "writer completion precedes lane closing"},
		{name: "usable replacement remains", addReplacement: true, wantError: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
			recipients := runtime.lanes.snapshot()
			if len(recipients) != 1 {
				t.Fatalf("terminal recipients = %d, want 1", len(recipients))
			}
			if test.addReplacement {
				replacement := newMemoryChannel(t)
				if _, err := runtime.lanes.add(
					LaneIdentity{ID: 2, Epoch: 1}, replacement, permissiveInboundAuthenticator(), false,
				); err != nil {
					t.Fatalf("add usable replacement: %v", err)
				}
			}
			writerContext, cancelWriter := context.WithCancel(context.Background())
			cancelWriter()
			if err := recipients[0].writer.Run(writerContext); !errors.Is(err, context.Canceled) {
				t.Fatalf("stop recipient writer: %v", err)
			}
			runtime.lanes.mu.Lock()
			initial := runtime.lanes.active[runtime.initial.ID]
			closing := initial == nil || initial.closing
			runtime.lanes.mu.Unlock()
			if closing {
				t.Fatal("test did not preserve the writer-Done-before-lane-closing window")
			}
			usable := runtime.lanes.hasUsable()
			current := runtime.lanes.snapshot()
			selected, selectErr := runtime.lanes.selectLane(nil)
			if test.addReplacement {
				if !usable || len(current) != 1 || current[0].identity.ID != 2 ||
					selectErr != nil || selected.identity.ID != 2 {
					t.Fatalf("replacement authority: usable=%v snapshot=%+v selected=%+v err=%v",
						usable, current, selected, selectErr)
				}
			} else if usable || len(current) != 0 || !errors.Is(selectErr, ErrLaneUnavailable) {
				t.Fatalf("stopped-writer authority: usable=%v snapshot=%+v selected=%+v err=%v",
					usable, current, selected, selectErr)
			}
			err := (senderOutbound{runtime: runtime, privateKey: privateKey}).sendTerminalRecipients(
				context.Background(), context.Background(), body, recipients,
			)
			if test.wantError {
				if !errors.Is(err, protocolsession.ErrWriterStopped) {
					t.Fatalf("usable stopped recipient error = %v", err)
				}
			} else if err != nil {
				t.Fatalf("naturally retired recipients failed terminal fanout: %v", err)
			}
		})
	}
}

func TestTerminalPostAdmissionWriterStopRequiresNoUsableReplacement(t *testing.T) {
	body, err := protocolsession.EncodeSessionTerminal(protocolsession.SessionTerminal{
		Code: SessionStoppedCode, Message: "share cancelled",
	})
	if err != nil {
		t.Fatal(err)
	}
	privateKey := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	for _, test := range []struct {
		name           string
		addReplacement bool
		wantError      bool
		wantDecision   SenderTerminalDecision
	}{
		{name: "last writer retires", wantDecision: SenderTerminalDecisionNaturalRetirement},
		{
			name: "usable replacement remains", addReplacement: true, wantError: true,
			wantDecision: SenderTerminalDecisionFailed,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
				recipients := runtime.lanes.snapshot()
				if len(recipients) != 1 {
					t.Fatalf("terminal recipients = %d, want 1", len(recipients))
				}
				if test.addReplacement {
					replacement := newMemoryChannel(t)
					if _, err := runtime.lanes.add(
						LaneIdentity{ID: 2, Epoch: 1}, replacement, permissiveInboundAuthenticator(), false,
					); err != nil {
						t.Fatalf("add usable replacement: %v", err)
					}
				}
				observed := make(chan SenderTerminalObservation, 1)
				result := make(chan error, 1)
				go func() {
					result <- (senderOutbound{
						runtime: runtime, privateKey: privateKey,
						observer: SenderTerminalObserverFunc(
							func(observation SenderTerminalObservation) { observed <- observation },
						),
					}).sendTerminalRecipients(
						context.Background(), context.Background(), body, recipients,
					)
				}()
				// The sender is durably blocked on the admitted receipt before the
				// writer publishes its local retirement.
				synctest.Wait()
				writerContext, cancelWriter := context.WithCancel(context.Background())
				cancelWriter()
				if err := recipients[0].writer.Run(writerContext); !errors.Is(err, context.Canceled) {
					t.Fatalf("stop recipient writer: %v", err)
				}
				synctest.Wait()
				terminalErr := <-result
				if test.wantError {
					if !errors.Is(terminalErr, protocolsession.ErrWriterStopped) {
						t.Fatalf("usable stopped recipient error = %v", terminalErr)
					}
				} else if terminalErr != nil {
					t.Fatalf("naturally retired receipt failed terminal fanout: %v", terminalErr)
				}
				observation := <-observed
				if observation.TransportDisposition != SenderTerminalTransportNotReached ||
					observation.Outcome != SenderTerminalOutcomeDropped ||
					observation.Decision != test.wantDecision {
					t.Fatalf("post-admission retirement observation=%+v", observation)
				}
			})
		})
	}
}

func TestTerminalClaimRejectedByLaneCancellationRequiresNoUsableReplacement(t *testing.T) {
	body, err := protocolsession.EncodeSessionTerminal(protocolsession.SessionTerminal{
		Code: SessionStoppedCode, Message: "share cancelled",
	})
	if err != nil {
		t.Fatal(err)
	}
	privateKey := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	for _, test := range []struct {
		name           string
		addReplacement bool
		wantError      bool
		wantDecision   SenderTerminalDecision
	}{
		{name: "last lane cancellation", wantDecision: SenderTerminalDecisionNaturalRetirement},
		{
			name: "usable replacement remains", addReplacement: true, wantError: true,
			wantDecision: SenderTerminalDecisionFailed,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
				channel := &terminalCancellationRejectionChannel{
					FrameChannel: newMemoryChannel(t), entered: make(chan struct{}),
				}
				identity := LaneIdentity{ID: 2, Epoch: 1}
				if _, err := runtime.lanes.add(
					identity, channel, permissiveInboundAuthenticator(), false,
				); err != nil {
					t.Fatalf("add cancellation-gated lane: %v", err)
				}
				if !runtime.lanes.detach(runtime.initial) {
					t.Fatal("detach original terminal recipient")
				}
				recipients := runtime.lanes.snapshot()
				if len(recipients) != 1 || recipients[0].identity != identity {
					t.Fatalf("terminal recipients = %+v, want cancellation-gated lane", recipients)
				}
				if test.addReplacement {
					if _, err := runtime.lanes.add(
						LaneIdentity{ID: 3, Epoch: 1}, newMemoryChannel(t),
						permissiveInboundAuthenticator(), false,
					); err != nil {
						t.Fatalf("add usable replacement: %v", err)
					}
				}

				observed := make(chan SenderTerminalObservation, 1)
				terminalResult := make(chan error, 1)
				go func() {
					terminalResult <- (senderOutbound{
						runtime: runtime, privateKey: privateKey,
						observer: SenderTerminalObserverFunc(
							func(observation SenderTerminalObservation) { observed <- observation },
						),
					}).sendTerminalRecipients(
						context.Background(), context.Background(), body, recipients,
					)
				}()
				writerContext, cancelWriter := context.WithCancel(context.Background())
				defer cancelWriter()
				writerResult := make(chan error, 1)
				go func() { writerResult <- recipients[0].writer.Run(writerContext) }()

				// Reaching SendTerminal proves the writer claimed and sealed the receipt;
				// cancellation now exercises the physical pre-acceptance boundary.
				<-channel.entered
				cancelWriter()
				synctest.Wait()
				writerErr := <-writerResult
				if !errors.Is(writerErr, context.Canceled) ||
					framechannel.SendDispositionOf(writerErr) != framechannel.SendRejected {
					t.Fatalf("writer cancellation rejection = %v", writerErr)
				}
				terminalErr := <-terminalResult
				if test.wantError {
					if !errors.Is(terminalErr, context.Canceled) ||
						framechannel.SendDispositionOf(terminalErr) != framechannel.SendRejected {
						t.Fatalf("usable replacement terminal error = %v", terminalErr)
					}
				} else if terminalErr != nil {
					t.Fatalf("last-lane cancellation failed terminal fanout: %v", terminalErr)
				}
				observation := <-observed
				if !observation.Settled ||
					observation.TransportDisposition != SenderTerminalTransportRejected ||
					observation.Outcome != SenderTerminalOutcomeDropped ||
					observation.Decision != test.wantDecision {
					t.Fatalf("cancellation rejection observation=%+v", observation)
				}
			})
		})
	}
}

func TestDeliveredTerminalPreservesCallerAndHardAdmissionFailures(t *testing.T) {
	body, err := protocolsession.EncodeSessionTerminal(protocolsession.SessionTerminal{
		Code: SessionStoppedCode, Message: "share cancelled",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Run("caller cancellation", func(t *testing.T) {
		fixture := newVerticalFixture(t)
		sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
		t.Cleanup(sender.Close)
		t.Cleanup(receiver.Close)
		callerContext, cancelCaller := context.WithCancel(context.Background())
		cancelCaller()
		err := sender.outbound.sendTerminalRecipients(
			context.Background(), callerContext, body, sender.lanes.snapshot(),
		)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("delivered terminal erased caller cancellation: %v", err)
		}
	})
	t.Run("hard preparation failure", func(t *testing.T) {
		fixture := newVerticalFixture(t)
		sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
		t.Cleanup(sender.Close)
		t.Cleanup(receiver.Close)
		recipients := append([]selectedLane{{identity: LaneIdentity{}}}, sender.lanes.snapshot()...)
		err := sender.outbound.sendTerminalRecipients(
			context.Background(), context.Background(), body, recipients,
		)
		if !errors.Is(err, protocolsession.ErrControlBinding) {
			t.Fatalf("delivered terminal erased hard preparation failure: %v", err)
		}
	})
	t.Run("accepted seal failure", func(t *testing.T) {
		fixture := newVerticalFixture(t)
		sender, receiver := connectVerticalPair(t, fixture.senderFactory, fixture.receiverFactory)
		t.Cleanup(sender.Close)
		t.Cleanup(receiver.Close)
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
				identity: LaneIdentity{ID: 99, Epoch: 1},
				channel:  failedChannel,
				writer:   failedWriter,
				done:     failedWriter.Done(),
			}},
			sender.lanes.snapshot()...,
		)
		err = sender.outbound.sendTerminalRecipients(
			context.Background(), context.Background(), body, recipients,
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
