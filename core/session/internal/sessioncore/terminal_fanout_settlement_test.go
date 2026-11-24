package sessioncore

import (
	"context"
	"errors"
	"io"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"

	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
)

type terminalReceiptGateChannel struct {
	*memoryChannel
	entered chan struct{}
	release chan struct{}
	result  error
}

func (channel *terminalReceiptGateChannel) SendTerminal(context.Context, framechannel.Frame) error {
	close(channel.entered)
	<-channel.release
	return channel.result
}

func TestTerminalFanoutWaitsForReceiptsAfterLifecycleCancellation(t *testing.T) {
	for _, cancelCaller := range []bool{false, true} {
		name := "lifecycle cancellation"
		if cancelCaller {
			name = "caller cancellation"
		}
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
				body, err := protocolsession.EncodeSessionTerminal(protocolsession.SessionTerminal{
					Code: session.SessionStoppedCode, Message: "stop",
				})
				if err != nil {
					t.Fatal(err)
				}
				channels := []*terminalReceiptGateChannel{
					{memoryChannel: newMemoryChannel(t), entered: make(chan struct{}), release: make(chan struct{})},
					{memoryChannel: newMemoryChannel(t), entered: make(chan struct{}), release: make(chan struct{}), result: io.ErrClosedPipe},
				}
				recipients := make([]selectedLane, 0, len(channels))
				for index, channel := range channels {
					lane, err := runtime.lanes.add(
						session.LaneIdentity{ID: uint32(index + 2), Epoch: 1}, channel, permissiveInboundAuthenticator(), false,
					)
					if err != nil {
						t.Fatal(err)
					}
					recipients = append(recipients, lane.selected())
				}
				writerResults := make(chan error, len(recipients))
				for _, lane := range recipients {
					go func() { writerResults <- lane.writer.Run(runtime.ctx) }()
				}
				result := make(chan error, 1)
				callerContext, cancel := context.WithCancel(context.Background())
				defer cancel()
				go func() {
					result <- (&Responses{
						runtime: runtime,
					}).sendTerminalRecipients(callerContext, body, recipients)
				}()
				for _, channel := range channels {
					<-channel.entered
				}
				// A peer can consume terminal and retire all lanes before the physical
				// writer publishes acceptance. Lifecycle cancellation is not its receipt.
				runtime.cancelContext()
				if cancelCaller {
					cancel()
				}
				synctest.Wait()
				premature := false
				select {
				case err := <-result:
					premature = true
					if !cancelCaller || !errors.Is(err, context.Canceled) {
						t.Errorf("terminal fanout returned before its admitted receipts settled: %v", err)
					}
				default:
					if cancelCaller {
						t.Error("terminal fanout ignored caller cancellation")
					}
				}
				for _, channel := range channels {
					close(channel.release)
				}
				for range recipients {
					<-writerResults
				}
				if !premature {
					if err := <-result; err != nil {
						t.Fatalf("delivered terminal lost to sibling closure: %v", err)
					}
				}
			})
		})
	}
}

type contextTerminalChannel struct {
	*memoryChannel
	precedingEntered chan struct{}
	precedingRelease chan struct{}
	precedingOnce    sync.Once
	terminalEntered  chan struct{}
	terminalRelease  chan struct{}
	terminalOnce     sync.Once
	terminalErr      error
	normalSends      atomic.Int32
	delivered        atomic.Int32
}

func (channel *contextTerminalChannel) Send(ctx context.Context, frame framechannel.Frame) error {
	channel.normalSends.Add(1)
	if channel.precedingRelease != nil {
		channel.precedingOnce.Do(func() { close(channel.precedingEntered) })
		select {
		case <-ctx.Done():
			return framechannel.RejectSend(ctx.Err())
		case <-channel.precedingRelease:
		}
	}
	return channel.memoryChannel.Send(ctx, frame)
}

func (channel *contextTerminalChannel) SendTerminal(ctx context.Context, frame framechannel.Frame) error {
	if channel.terminalEntered != nil {
		channel.terminalOnce.Do(func() { close(channel.terminalEntered) })
	}
	if channel.terminalRelease != nil {
		select {
		case <-ctx.Done():
			return framechannel.RejectSend(ctx.Err())
		case <-channel.terminalRelease:
		}
	}
	if err := ctx.Err(); err != nil {
		return framechannel.RejectSend(err)
	}
	if channel.terminalErr != nil {
		return channel.terminalErr
	}
	channel.delivered.Add(1)
	return channel.memoryChannel.SendTerminal(ctx, frame)
}

func TestTerminalFanoutSiblingRetirementDoesNotCancelHealthyDelivery(t *testing.T) {
	body, err := protocolsession.EncodeSessionTerminal(protocolsession.SessionTerminal{
		Code: session.SessionStoppedCode, Message: "stop",
	})
	if err != nil {
		t.Fatal(err)
	}
	controlBody, err := protocolsession.EncodePeerPathControl(protocolsession.PeerPathControl{
		PeerPathID:          [16]byte{1},
		NetworkGenerationID: [16]byte{2},
		ControlSequence:     1,
		Kind:                protocolsession.PeerPathRevoke,
	})
	if err != nil {
		t.Fatal(err)
	}
	precedingControl, err := protocolsession.NewMessage(protocolsession.MessagePeerPathControl, nil, controlBody)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("failing sibling preserves in-flight healthy terminal", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
			healthy := &contextTerminalChannel{
				memoryChannel:   newMemoryChannel(t),
				terminalEntered: make(chan struct{}),
				terminalRelease: make(chan struct{}),
			}
			failing := &contextTerminalChannel{
				memoryChannel: newMemoryChannel(t),
				terminalErr:   framechannel.RetireSend(io.ErrClosedPipe),
			}
			healthyLane, err := runtime.lanes.add(
				session.LaneIdentity{ID: 2, Epoch: 1}, healthy, permissiveInboundAuthenticator(), false,
			)
			if err != nil {
				t.Fatal(err)
			}
			failingLane, err := runtime.lanes.add(
				session.LaneIdentity{ID: 3, Epoch: 1}, failing, permissiveInboundAuthenticator(), false,
			)
			if err != nil {
				t.Fatal(err)
			}
			if !runtime.DetachLane(runtime.initial) {
				t.Fatal("initial lane did not detach")
			}
			runtime.Start()
			defer runtime.Close()

			result := make(chan error, 1)
			go func() {
				result <- (&Responses{runtime: runtime}).sendTerminalAll(context.Background(), body)
			}()
			<-failingLane.done
			<-healthy.terminalEntered
			synctest.Wait()

			if err := runtime.Lifetime().Err(); err != nil {
				t.Fatalf("failing sibling canceled lifetime during healthy terminal send: %v", err)
			}
			select {
			case err := <-result:
				t.Fatalf("terminal fanout returned before healthy lane settled: %v", err)
			default:
			}

			close(healthy.terminalRelease)
			if err := <-result; err != nil {
				t.Fatalf("healthy lane terminal delivery failed after sibling failure: %v", err)
			}
			if healthy.delivered.Load() != 1 {
				t.Fatalf("healthy terminal deliveries = %d, want 1", healthy.delivered.Load())
			}
			<-healthyLane.done
			if runtime.Lifetime().Err() == nil {
				t.Fatal("final terminal lane retirement did not cancel lifetime")
			}
		})
	})

	t.Run("fast sibling completion preserves queued terminal on sibling writer", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
			slow := &contextTerminalChannel{
				memoryChannel:    newMemoryChannel(t),
				precedingEntered: make(chan struct{}),
				precedingRelease: make(chan struct{}),
			}
			fast := &contextTerminalChannel{
				memoryChannel: newMemoryChannel(t),
			}
			slowLane, err := runtime.lanes.add(
				session.LaneIdentity{ID: 2, Epoch: 1}, slow, permissiveInboundAuthenticator(), false,
			)
			if err != nil {
				t.Fatal(err)
			}
			fastLane, err := runtime.lanes.add(
				session.LaneIdentity{ID: 3, Epoch: 1}, fast, permissiveInboundAuthenticator(), false,
			)
			if err != nil {
				t.Fatal(err)
			}
			if !runtime.DetachLane(runtime.initial) {
				t.Fatal("initial lane did not detach")
			}
			runtime.Start()
			defer runtime.Close()

			if err := slowLane.writer.EnqueueControl(precedingControl, nil); err != nil {
				t.Fatalf("queue preceding control: %v", err)
			}
			<-slow.precedingEntered

			result := make(chan error, 1)
			go func() {
				result <- (&Responses{runtime: runtime}).sendTerminalAll(context.Background(), body)
			}()
			<-fastLane.done
			synctest.Wait()

			if fast.delivered.Load() != 1 {
				t.Fatalf("fast terminal deliveries = %d, want 1", fast.delivered.Load())
			}
			if err := runtime.Lifetime().Err(); err != nil {
				t.Fatalf("fast sibling retirement canceled lifetime before queued sibling sent terminal: %v", err)
			}

			close(slow.precedingRelease)
			if err := <-result; err != nil {
				t.Fatalf("terminal fanout failed: %v", err)
			}
			if slow.delivered.Load() != 1 {
				t.Fatalf("slow terminal deliveries = %d, want 1", slow.delivered.Load())
			}
			<-slowLane.done
			if runtime.Lifetime().Err() == nil {
				t.Fatal("final terminal lane retirement did not cancel lifetime")
			}
		})
	})
}

func TestTerminalFanoutQueuedResponsePreservesLifetimeAcrossSiblingRetirement(t *testing.T) {
	body, err := protocolsession.EncodeSessionTerminal(protocolsession.SessionTerminal{
		Code: session.SessionStoppedCode, Message: "stop",
	})
	if err != nil {
		t.Fatal(err)
	}
	controlBody, err := protocolsession.EncodePeerPathControl(protocolsession.PeerPathControl{
		PeerPathID:          [16]byte{1},
		NetworkGenerationID: [16]byte{2},
		ControlSequence:     1,
		Kind:                protocolsession.PeerPathRevoke,
	})
	if err != nil {
		t.Fatal(err)
	}
	precedingControl, err := protocolsession.NewMessage(protocolsession.MessagePeerPathControl, nil, controlBody)
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name        string
		siblingErr  error
		sendControl bool
	}{
		{
			name:       "queued fragment survives failing sibling",
			siblingErr: framechannel.RetireSend(io.ErrClosedPipe),
		},
		{
			name:        "queued control response survives fast sibling completion",
			sendControl: true,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
				outbound := NewResponses(runtime, ResponseConfig{})
				healthy := &contextTerminalChannel{
					memoryChannel:    newMemoryChannel(t),
					precedingEntered: make(chan struct{}),
					precedingRelease: make(chan struct{}),
					terminalEntered:  make(chan struct{}),
					terminalRelease:  make(chan struct{}),
				}
				sibling := &contextTerminalChannel{
					memoryChannel: newMemoryChannel(t),
					terminalErr:   test.siblingErr,
				}
				healthyLane, err := runtime.lanes.add(
					session.LaneIdentity{ID: 2, Epoch: 1}, healthy, permissiveInboundAuthenticator(), false,
				)
				if err != nil {
					t.Fatal(err)
				}
				siblingLane, err := runtime.lanes.add(
					session.LaneIdentity{ID: 3, Epoch: 1}, sibling, permissiveInboundAuthenticator(), false,
				)
				if err != nil {
					t.Fatal(err)
				}
				if !runtime.DetachLane(runtime.initial) {
					t.Fatal("initial lane did not detach")
				}
				runtime.Start()
				defer runtime.Close()

				if err := healthyLane.writer.EnqueueControl(precedingControl, nil); err != nil {
					t.Fatalf("queue preceding control: %v", err)
				}
				<-healthy.precedingEntered

				opID := protocolsession.OperationID(201)
				request, err := protocolsession.NewMessage(
					protocolsession.MessageRequestBlocks, &opID, []byte{0xa1, 0x00, 0x01},
				)
				if err != nil {
					t.Fatal(err)
				}
				op := admitTestSenderOperation(t, runtime, healthyLane.identity, request)
				responseDone := make(chan error, 1)
				if test.sendControl {
					completeBody, encodeErr := contentflow.EncodeOperationComplete(1)
					if encodeErr != nil {
						t.Fatal(encodeErr)
					}
					go func() {
						_, sendErr := outbound.SendControl(
							runtime.Lifetime(), op, protocolsession.MessageOperationComplete, completeBody,
						)
						responseDone <- sendErr
					}()
				} else {
					fragments, fragErr := contentflow.FragmentRecord(opID, []byte("queued-fragment"))
					if fragErr != nil {
						t.Fatal(fragErr)
					}
					go func() {
						responseDone <- outbound.SendFragment(runtime.Lifetime(), op, fragments[0])
					}()
				}
				synctest.Wait()

				terminalDone := make(chan error, 1)
				go func() {
					terminalDone <- outbound.SendTerminalAll(context.Background(), body)
				}()

				responseErr := <-responseDone
				if !errors.Is(responseErr, protocolsession.ErrWriterTerminal) || !protocolsession.IsNotSent(responseErr) {
					t.Fatalf("preempted response err = %v, want NotSent(ErrWriterTerminal)", responseErr)
				}
				if errors.Is(responseErr, ErrLaneUnavailable) {
					t.Fatalf("preempted response retried into ErrLaneUnavailable: %v", responseErr)
				}

				<-siblingLane.done
				synctest.Wait()

				if err := runtime.Lifetime().Err(); err != nil {
					t.Fatalf("sibling retirement canceled lifetime after queued response preemption: %v", err)
				}
				if sibling.normalSends.Load() != 0 {
					t.Fatalf("preempted response leaked %d normal send(s) onto sibling lane", sibling.normalSends.Load())
				}

				close(healthy.precedingRelease)
				<-healthy.terminalEntered
				synctest.Wait()
				if err := runtime.Lifetime().Err(); err != nil {
					t.Fatalf("lifetime canceled while healthy lane delivered terminal: %v", err)
				}

				close(healthy.terminalRelease)
				if err := <-terminalDone; err != nil {
					t.Fatalf("terminal fanout failed: %v", err)
				}
				if healthy.delivered.Load() != 1 {
					t.Fatalf("healthy terminal deliveries = %d, want 1", healthy.delivered.Load())
				}
				<-healthyLane.done
				if runtime.Lifetime().Err() == nil {
					t.Fatal("final terminal lane retirement did not cancel lifetime")
				}
			})
		})
	}

	t.Run("preempted response stops before failing over to sibling not yet terminal", func(t *testing.T) {
		synctest.Test(t, func(t *testing.T) {
			runtime, _ := newUnstartedRuntime(t, protocolsession.RoleSender)
			outbound := NewResponses(runtime, ResponseConfig{})
			laneAChannel := &contextTerminalChannel{
				memoryChannel:    newMemoryChannel(t),
				precedingEntered: make(chan struct{}),
				precedingRelease: make(chan struct{}),
			}
			laneBChannel := &contextTerminalChannel{
				memoryChannel: newMemoryChannel(t),
			}
			laneA, err := runtime.lanes.add(
				session.LaneIdentity{ID: 2, Epoch: 1}, laneAChannel, permissiveInboundAuthenticator(), false,
			)
			if err != nil {
				t.Fatal(err)
			}
			laneB, err := runtime.lanes.add(
				session.LaneIdentity{ID: 3, Epoch: 1}, laneBChannel, permissiveInboundAuthenticator(), false,
			)
			if err != nil {
				t.Fatal(err)
			}
			if !runtime.DetachLane(runtime.initial) {
				t.Fatal("initial lane did not detach")
			}
			runtime.Start()
			defer runtime.Close()

			if err := laneA.writer.EnqueueControl(precedingControl, nil); err != nil {
				t.Fatalf("queue preceding control: %v", err)
			}
			<-laneAChannel.precedingEntered

			opID := protocolsession.OperationID(202)
			request, err := protocolsession.NewMessage(
				protocolsession.MessageRequestBlocks, &opID, []byte{0xa1, 0x00, 0x01},
			)
			if err != nil {
				t.Fatal(err)
			}
			op := admitTestSenderOperation(t, runtime, laneA.identity, request)
			fragments, err := contentflow.FragmentRecord(opID, []byte("no-failover"))
			if err != nil {
				t.Fatal(err)
			}
			responseDone := make(chan error, 1)
			go func() {
				responseDone <- outbound.SendFragment(runtime.Lifetime(), op, fragments[0])
			}()
			synctest.Wait()

			terminalMsg, err := protocolsession.NewMessage(protocolsession.MessageSessionTerminal, nil, body)
			if err != nil {
				t.Fatal(err)
			}
			// Enqueue terminal on laneA first while laneB is still writerAccepting,
			// matching the mid-loop window inside sendTerminalRecipients.
			terminalSend, err := laneA.writer.EnqueueTerminal(terminalMsg, runtime.lanes.acceptTerminal)
			if err != nil {
				t.Fatalf("enqueue terminal on laneA: %v", err)
			}
			responseErr := <-responseDone
			if !errors.Is(responseErr, protocolsession.ErrWriterTerminal) || !protocolsession.IsNotSent(responseErr) {
				t.Fatalf("preempted response err = %v, want NotSent(ErrWriterTerminal)", responseErr)
			}
			if laneBChannel.normalSends.Load() != 0 {
				t.Fatalf("preempted response leaked %d send(s) onto still-accepting sibling lane", laneBChannel.normalSends.Load())
			}
			// Even an explicit markSelectedClosing must not poison laneA while it delivers terminal.
			runtime.lanes.markSelectedClosing(laneA.selected())
			if !runtime.DetachLane(laneB.identity) {
				t.Fatal("sibling laneB did not detach")
			}
			if err := runtime.Lifetime().Err(); err != nil {
				t.Fatalf("sibling detach canceled lifetime while laneA delivered terminal: %v", err)
			}

			close(laneAChannel.precedingRelease)
			if res := terminalSend.Await(context.Background()); res.err != nil {
				t.Fatalf("laneA terminal delivery failed: %v", res.err)
			}
			if laneAChannel.delivered.Load() != 1 {
				t.Fatalf("laneA terminal deliveries = %d, want 1", laneAChannel.delivered.Load())
			}
			<-laneA.done
			if runtime.Lifetime().Err() == nil {
				t.Fatal("final terminal lane retirement did not cancel lifetime")
			}
		})
	})
}
