package sessionruntime

import (
	"bytes"
	"context"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/requestlane"
	"github.com/windshare/windshare/core/transfer"
)

type responseDelayChannel struct {
	protocolsession.FrameChannel
	delay atomic.Int64
}

func (channel *responseDelayChannel) Send(ctx context.Context, frame framechannel.Frame) error {
	select {
	case <-ctx.Done():
		return framechannel.RejectSend(ctx.Err())
	case <-time.After(time.Duration(channel.delay.Load())):
		return channel.FrameChannel.Send(ctx, frame)
	}
}

func delayedResponseChannel(channel protocolsession.FrameChannel, delay time.Duration) *responseDelayChannel {
	result := &responseDelayChannel{FrameChannel: channel}
	result.delay.Store(int64(delay))
	return result
}

func TestControlRequestsUseMeasuredPathsWithoutRelayRTTPerFile(t *testing.T) {
	for _, trace := range []bool{false, true} {
		for _, test := range []struct {
			name                        string
			initialDelay, attachedDelay time.Duration
			route                       transfer.LaneRoute
		}{
			{"fast_direct", 500 * time.Millisecond, time.Millisecond, transfer.LaneRouteDirect},
			{"sub_clock_direct", 10 * time.Millisecond, 0, transfer.LaneRouteDirect},
			{"slow_direct", time.Millisecond, 500 * time.Millisecond, transfer.LaneRouteDirect},
			{"fast_attached_relay", 500 * time.Millisecond, time.Millisecond, transfer.LaneRouteRelay},
		} {
			name := test.name
			if trace {
				name += "_traced"
			}
			t.Run(name, func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					fixture := newVerticalFixture(t)
					config := fixture.receiverConfig
					var recorder *protocolTraceRecorder
					if trace {
						producer, consumer, err := observationstream.New[ProtocolObservation](256)
						if err != nil {
							t.Fatal(err)
						}
						config.ProtocolObservations = producer
						recorder = &protocolTraceRecorder{producer: producer, consumer: consumer}
					}
					factory, err := NewReceiverFactory(config)
					if err != nil {
						t.Fatal(err)
					}
					defer factory.Close()
					senderChannel, rawReceiver := newMemoryChannelPair()
					initialChannel := delayedResponseChannel(rawReceiver, test.initialDelay)
					accepted := make(chan *SenderRuntime, 1)
					go func() {
						sender, acceptErr := fixture.senderFactory.Accept(context.Background(), senderChannel)
						if acceptErr != nil {
							t.Error(acceptErr)
						}
						accepted <- sender
					}()
					receiver, err := factory.Connect(context.Background(), initialChannel, transfer.LaneRouteRelay)
					if err != nil {
						t.Fatal(err)
					}
					defer receiver.Close()
					sender := <-accepted
					if sender == nil {
						t.Fatal("sender not accepted")
					}
					defer sender.Close()

					grant, err := receiver.RequestLane(context.Background(), 0)
					if err != nil {
						t.Fatal(err)
					}
					attachedSender, rawAttached := newMemoryChannelPair()
					attachedChannel := delayedResponseChannel(rawAttached, test.attachedDelay)
					attached := make(chan error, 1)
					go func() {
						_, attachErr := fixture.senderFactory.Attach(context.Background(), attachedSender)
						attached <- attachErr
					}()
					admission, err := receiver.AttachLane(context.Background(), grant, attachedChannel, test.route)
					if err != nil {
						t.Fatal(err)
					}
					if err := <-attached; err != nil {
						t.Fatal(err)
					}
					want := receiver.initial
					if test.attachedDelay < test.initialDelay {
						want = admission.Lane
					}

					started := time.Now()
					for index := range 16 {
						fixture.contentStore.lease, err = content.NewRevisionLease(id16[content.LeaseID](byte(index+20)), fixture.contentStore.descriptor, contentflow.RevisionLeaseTTL, contentflow.RevisionLeaseRenewAfter)
						if err != nil {
							t.Fatal(err)
						}
						opened, err := receiver.OpenRevision(context.Background(), fixture.fileID)
						if err != nil {
							t.Fatal(err)
						}
						if err := receiver.ReleaseRevision(context.Background(), opened.LeaseID); err != nil {
							t.Fatal(err)
						}
					}
					if elapsed := time.Since(started); elapsed > 100*time.Millisecond {
						t.Fatalf("16 opens/releases retained slow-path waiting: %v", elapsed)
					}
					if trace {
						count := 0
						for _, event := range recorder.snapshot() {
							if event.RequestKind != protocolsession.MessageOpenRevisions && event.RequestKind != protocolsession.MessageReleaseLease {
								continue
							}
							count++
							if event.Lane != want || event.RequestScheduling.Expected == 0 {
								t.Fatalf("control route/decision = %+v, want lane %+v", event, want)
							}
						}
						if count != 32 {
							t.Fatalf("control observations = %d", count)
						}
					}
					// A connected path can become slower without disconnecting. Each kind
					// learns from its own completed request before the next file starts.
					fastChannel := initialChannel
					if want == admission.Lane {
						fastChannel = attachedChannel
					}
					fastChannel.delay.Store(int64(2 * time.Second))
					for index := range 2 {
						fixture.contentStore.lease, err = content.NewRevisionLease(id16[content.LeaseID](byte(40+index)), fixture.contentStore.descriptor, contentflow.RevisionLeaseTTL, contentflow.RevisionLeaseRenewAfter)
						if err != nil {
							t.Fatal(err)
						}
						began := time.Now()
						opened, openErr := receiver.OpenRevision(context.Background(), fixture.fileID)
						if openErr != nil {
							t.Fatal(openErr)
						}
						if err := receiver.ReleaseRevision(context.Background(), opened.LeaseID); err != nil {
							t.Fatal(err)
						}
						if index == 1 && time.Since(began) > 1100*time.Millisecond {
							t.Fatalf("connected slow path retained after response samples: %v", time.Since(began))
						}
					}
					fixture.contentStore.lease, err = content.NewRevisionLease(id16[content.LeaseID](42), fixture.contentStore.descriptor, contentflow.RevisionLeaseTTL, contentflow.RevisionLeaseRenewAfter)
					if err != nil {
						t.Fatal(err)
					}
					// Retirement changes subsequent routing while preserving the surviving
					// session and its authenticated revision/content authority.
					if !receiver.DetachLane(want) || !sender.DetachLane(want) {
						t.Fatal("detach selected lane")
					}
					opened, err := receiver.OpenRevision(context.Background(), fixture.fileID)
					if err != nil {
						t.Fatal(err)
					}
					output := make([]byte, len(fixture.fileData))
					err = receiver.BlockBroker().ReadRange(context.Background(), opened.LeaseID, opened.Descriptor,
						content.Range{Offset: 0, End: uint64(len(output))},
						transfer.RangeSinkFunc(func(_ context.Context, offset uint64, data []byte) error {
							copy(output[offset:], data)
							return nil
						}))
					if err != nil || !bytes.Equal(output, fixture.fileData) {
						t.Fatalf("fallback content: %v", err)
					}
					if err := receiver.ReleaseRevision(context.Background(), opened.LeaseID); err != nil {
						t.Fatal(err)
					}
				})
			})
		}
	}
}

func TestRequestReservationCleanupDoesNotUpdateReplacementEpoch(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	now := time.Unix(100, 0)
	runtime.now = func() time.Time { return now }
	kind := protocolsession.MessageReleaseLease
	channel, peer := newMemoryChannelPair()
	t.Cleanup(func() { _ = peer.Close() })
	identity := LaneIdentity{ID: 2, Epoch: 1}
	old, err := runtime.lanes.add(identity, channel, permissiveInboundAuthenticator(), false)
	if err != nil {
		t.Fatal(err)
	}
	old.requests = requestlane.New(time.Millisecond)
	_, reserved, estimate, err := runtime.lanes.selectRequestLane(nil, kind)
	if err != nil {
		t.Fatal(err)
	}
	call := newOperationCall(protocolsession.OperationID{}, kind, now, 0, false, false)
	call.requests.Reserve(reserved, estimate)
	if !runtime.lanes.detach(identity) {
		t.Fatal("retire old incarnation")
	}
	channel, nextPeer := newMemoryChannelPair()
	t.Cleanup(func() { _ = nextPeer.Close() })
	identity.Epoch++
	replacement, err := runtime.lanes.add(identity, channel, permissiveInboundAuthenticator(), false)
	if err != nil {
		t.Fatal(err)
	}
	call.requests.Complete(now.Add(time.Second))
	call.close()
	if got := replacement.requests.Estimate(kind, now, 0); got.Response != requestlane.InitialResponse || got.Pending != 0 {
		t.Fatalf("late completion polluted replacement: %+v", got)
	}
	if call.requests.Reserve(replacement.requests.Reserve(kind, now), estimate) {
		t.Fatal("closed call reserved")
	}
	if replacement.requests.Estimate(kind, now, 0).Pending != 0 {
		t.Fatal("closed call leaked reservation")
	}
}

func TestControlSelectionAccountsForCongestionAndExactLaneAuthority(t *testing.T) {
	runtime, _ := newUnstartedRuntime(t, protocolsession.RoleReceiver)
	secondChannel, peer := newMemoryChannelPair()
	t.Cleanup(func() { _ = peer.Close() })
	second := LaneIdentity{ID: 2, Epoch: 1}
	if _, err := runtime.lanes.add(second, secondChannel, permissiveInboundAuthenticator(), false); err != nil {
		t.Fatal(err)
	}
	now := time.Unix(100, 0)
	runtime.now = func() time.Time { return now }
	runtime.lanes.active[runtime.initial.ID].requests = requestlane.New(10 * time.Millisecond)
	runtime.lanes.active[second.ID].requests = requestlane.New(15 * time.Millisecond)
	kind := protocolsession.MessageOpenRevisions
	first, reservation, _, err := runtime.lanes.selectRequestLane(nil, kind)
	if err != nil || first.identity != runtime.initial {
		t.Fatalf("first selection %v %v", first.identity, err)
	}
	next, otherReservation, _, err := runtime.lanes.selectRequestLane(nil, kind)
	if err != nil || next.identity != second {
		t.Fatalf("pending reservation not charged: %v %v", next.identity, err)
	}
	otherReservation.Abandon()
	reservation.Abandon()
	runtime.lanes.queuedContent = func(id LaneIdentity) time.Duration {
		if id == runtime.initial {
			return time.Second
		}
		return 0
	}
	next, reservation, estimate, err := runtime.lanes.selectRequestLane(nil, kind)
	if err != nil || next.identity != second || estimate.Pending != 0 {
		t.Fatalf("content queue selection: %+v %v", next.identity, err)
	}
	reservation.Abandon()
	exact, exactReservation, _, err := runtime.lanes.selectRequestLane(&runtime.initial, kind)
	if err != nil || exact.identity != runtime.initial || exactReservation != nil {
		t.Fatalf("explicit authority overridden: %+v %v", exact.identity, err)
	}
	if !runtime.lanes.detach(second) {
		t.Fatal("detach")
	}
	next, reservation, _, err = runtime.lanes.selectRequestLane(nil, kind)
	if err != nil || next.identity != runtime.initial {
		t.Fatalf("surviving relay unavailable: %+v %v", next.identity, err)
	}
	reservation.Abandon()
}
