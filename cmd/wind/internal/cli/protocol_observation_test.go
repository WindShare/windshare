package cli

import (
	"bytes"
	"context"
	"errors"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/observationbridge"
	"github.com/windshare/windshare/cmd/wind/internal/runtrace"
	"github.com/windshare/windshare/core/observationstream"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

func cleanupProtocolObservations(t *testing.T, stream protocolObservationStream) {
	t.Helper()
	t.Cleanup(func() { stream.complete(context.Background()) })
}

func protocolAssemblyFact(role protocolsession.Role) sessionruntime.ProtocolObservation {
	context := sessionruntime.ProtocolObservationContext{
		ObservedAt: time.Unix(12, 345),
		Correlation: sessionruntime.ProtocolObservationCorrelation{
			Role: role, ProtocolSessionID: protocolsession.ProtocolSessionID{1},
			OperationID: protocolsession.OperationID{2}, RequestKind: protocolsession.MessageListChildren,
		},
	}
	stage := sessionruntime.ProtocolOperationReceiverFailed
	cause := sessionruntime.ProtocolOperationCauseCanceled
	if role == protocolsession.RoleSender {
		stage, cause = sessionruntime.ProtocolOperationSenderRequestReceived, sessionruntime.ProtocolOperationCauseNone
	}
	return sessionruntime.NewProtocolOperationObservation(context, sessionruntime.ProtocolOperationObservation{
		Stage: stage, Cause: cause,
	})
}

func TestProtocolCommandAssemblyProjectsSourceFactsAndReportsFinalLossOnce(t *testing.T) {
	for _, command := range []clievent.Command{clievent.CommandShare, clievent.CommandGet} {
		name := "share"
		if command == clievent.CommandGet {
			name = "get"
		}
		t.Run(name, func(t *testing.T) {
			recorder := newFakeUserTrace(runtrace.Status{Complete: true})
			app := &App{
				Stderr: bytes.NewBuffer(nil),
				openUserTrace: func(runtrace.Target, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
					return recorder, nil
				},
			}
			runtime, err := app.newCommandRuntime(command, testExactTraceOptions("trace.ndjson"))
			if err != nil {
				t.Fatal(err)
			}
			var producer observationstream.Producer[sessionruntime.ProtocolObservation]
			var complete func(context.Context)
			role := protocolsession.RoleReceiver
			if command == clievent.CommandShare {
				observations := newShareObservations(runtime)
				producer, complete, role = observations.protocolObservations(), observations.complete, protocolsession.RoleSender
			} else {
				observation := newGetObservation(runtime)
				producer, complete = observation.protocolObservations(), observation.complete
			}
			fact := protocolAssemblyFact(role)
			if producer.IsZero() || !producer.TryPublish(fact) {
				t.Fatal("enabled command omitted protocol stream")
			}
			// Receipt adapters transfer their own bounded loss into this same producer.
			producer.RecordDropped(3)
			complete(context.Background())
			complete(context.Background())
			runtime.Close()
			var projected int
			var lost uint64
			for _, event := range recorder.recorded() {
				switch event := event.(type) {
				case clievent.ProtocolObservationObserved:
					projected++
					if event.Command() != command || !event.ObservedAt().Equal(fact.ObservedAt()) {
						t.Fatalf("projection changed source context: %+v", event)
					}
				case clievent.ObserverLossObserved:
					if event.Category() == clievent.ObserverLossProtocolOperation && event.Reason() == clievent.ObserverLossStreamCapacity {
						lost += event.Count()
					}
				}
			}
			if projected != 1 || lost != 3 || recorder.lifecycle != 3 {
				t.Fatalf("projected=%d loss=%d upstream=%d", projected, lost, recorder.lifecycle)
			}
			if producer.TryPublish(fact) {
				t.Fatal("completed command retained observation admission")
			}
		})
	}
}

func TestProtocolReaderCancellationLeavesBoundedResidueAndRevokesPublication(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		started, release := make(chan struct{}), make(chan struct{})
		published := false
		stream := startProtocolObservations(func(ctx context.Context, gate *observationbridge.PublicationGate, _ sessionruntime.ProtocolObservation) {
			close(started)
			<-release
			gate.Commit(ctx, func() bool { published = true; return true })
		})
		fact := protocolAssemblyFact(protocolsession.RoleSender)
		if !stream.producer.TryPublish(fact) {
			t.Fatal("first publication rejected")
		}
		<-started
		for range int(protocolObservationCapacity) {
			if !stream.producer.TryPublish(fact) {
				t.Fatal("bounded prefix rejected before capacity")
			}
		}
		if stream.producer.TryPublish(fact) {
			t.Fatal("stream grew beyond its named bound")
		}
		stream.producer.RecordDropped(2)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		completion, status := stream.complete(ctx)
		if completion.CapacityDropped != 3 || completion.Enqueued != uint64(protocolObservationCapacity)+1 ||
			status.Joined || !status.Active || status.Buffered != uint64(protocolObservationCapacity) {
			t.Fatalf("completion=%+v reader=%+v", completion, status)
		}
		close(release)
		synctest.Wait()
		if published {
			t.Fatal("canceled reader published after command join cut")
		}
	})
}

type protocolStopFactory struct {
	producer observationstream.Producer[sessionruntime.ProtocolObservation]
	joining  chan struct{}
	release  chan struct{}
	once     sync.Once
}

func (*protocolStopFactory) AdmitChannel(context.Context, protocolsession.FrameChannel) (sessionruntime.SenderChannelAdmission, error) {
	return sessionruntime.SenderChannelAdmission{}, sessionruntime.ErrRuntimeClosed
}
func (factory *protocolStopFactory) Stop(ctx context.Context, _ string) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	factory.once.Do(func() { close(factory.joining) })
	<-factory.release
	factory.producer.TryPublish(protocolAssemblyFact(protocolsession.RoleSender))
	factory.producer.RecordDropped(2)
	return nil
}

func TestShareObservationCutWaitsForRuntimeQuiescenceAfterStopDeadline(t *testing.T) {
	emitter := &shareRecordingEmitter{detailed: true}
	observations := newShareObservations(emitter)
	factory := &protocolStopFactory{
		producer: observations.protocolObservations(), joining: make(chan struct{}), release: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan error, 1)
	go func() {
		err := stopShareFactoryWithin(ctx, factory, "Sender stopped")
		observations.complete(context.Background())
		done <- err
	}()
	<-factory.joining
	if !factory.producer.TryPublish(protocolAssemblyFact(protocolsession.RoleSender)) {
		t.Fatal("caller deadline cut admission while a runtime still owned final facts")
	}
	select {
	case <-done:
		t.Fatal("command finalized before runtime quiescence")
	default:
	}
	close(factory.release)
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("stop error=%v", err)
	}
	if len(emitter.events) != 2 || emitter.lifecycleLoss != 2 {
		t.Fatalf("late runtime evidence missing: events=%d loss=%d", len(emitter.events), emitter.lifecycleLoss)
	}
}

func TestProtocolAssemblyReportsRejectedFactsAndSkipsRevokedProjection(t *testing.T) {
	emitter := &shareRecordingEmitter{detailed: true}
	observations := newShareObservations(emitter)
	observations.protocolObservations().TryPublish(sessionruntime.ProtocolOperationObservation{})
	observations.complete(context.Background())
	if emitter.lifecycleLoss != 1 {
		t.Fatalf("invalid source fact loss=%d", emitter.lifecycleLoss)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	observations.protocolObservationContext(ctx, nil, sessionruntime.ProtocolOperationObservation{})
	(getObservation{}).protocolObservationContext(ctx, nil, sessionruntime.ProtocolOperationObservation{})
	if emitter.lifecycleLoss != 1 {
		t.Fatal("revoked reader attempted another projection")
	}
	if !(getObservation{}).protocolObservations().IsZero() {
		t.Fatal("zero observation created a producer")
	}
}
