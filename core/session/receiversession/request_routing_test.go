package receiversession

import (
	"bytes"
	"context"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/framechannel"
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/core/session/contentflow"
	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sendersession"
	"github.com/windshare/windshare/core/transfer"
	transferlanes "github.com/windshare/windshare/core/transfer/lanes"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"
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
			route                       transferlanes.Route
		}{
			{"fast_direct", 500 * time.Millisecond, time.Millisecond, transferlanes.RouteDirect},
			{"sub_clock_direct", 10 * time.Millisecond, 0, transferlanes.RouteDirect},
			{"slow_direct", time.Millisecond, 500 * time.Millisecond, transferlanes.RouteDirect},
			{"fast_attached_relay", 500 * time.Millisecond, time.Millisecond, transferlanes.RouteRelay},
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
						producer, consumer, err := observationstream.New[sessionobs.ProtocolObservation](256)
						if err != nil {
							t.Fatal(err)
						}
						config.ProtocolObservations = producer
						recorder = &protocolTraceRecorder{producer: producer, consumer: consumer}
					}
					factory, err := NewFactory(config)
					if err == nil {
						trackReceiverFactory(factory)
					}
					if err != nil {
						t.Fatal(err)
					}
					defer factory.Close()
					senderChannel, rawReceiver := newMemoryChannelPair()
					initialChannel := delayedResponseChannel(rawReceiver, test.initialDelay)
					accepted := make(chan *sendersession.Runtime, 1)
					go func() {
						sender, acceptErr := fixture.senderFactory.Accept(context.Background(), senderChannel)
						if acceptErr != nil {
							t.Error(acceptErr)
						}
						accepted <- sender
					}()
					receiver, err := factory.Connect(context.Background(), initialChannel, transferlanes.RouteRelay)
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
					want := runtimeIdentity(receiver)
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
