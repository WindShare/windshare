package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/runtrace"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

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
			role := protocolsession.RoleReceiver
			var project func(sessionruntime.ProtocolObservation)
			var loss func()
			if command == clievent.CommandShare {
				observations := newShareProjection(runtime)
				role = protocolsession.RoleSender
				project = func(fact sessionruntime.ProtocolObservation) {
					observations.protocolObservationContext(context.Background(), nil, fact)
				}
				loss = func() {
					observations.reportCumulativeLoss(observerLossProtocolQueue, clievent.ObserverLossProtocolOperation, clievent.ObserverLossStreamCapacity, 3)
				}
			} else {
				observations := newGetObservation(runtime)
				project = func(fact sessionruntime.ProtocolObservation) {
					observations.protocolObservationContext(context.Background(), nil, fact)
				}
				loss = func() {
					observations.reportCumulativeLoss(observerLossProtocolQueue, clievent.ObserverLossProtocolOperation, clievent.ObserverLossStreamCapacity, 3)
				}
			}
			fact := protocolAssemblyFact(role)
			project(fact)
			loss()
			loss()
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
		})
	}
}

func TestSupersededBlockObservationRetainsTraceWithoutVerboseWarning(t *testing.T) {
	recorder := newFakeUserTrace(runtrace.Status{Complete: true})
	stderr := bytes.NewBuffer(nil)
	app := &App{
		Stderr: stderr,
		openUserTrace: func(runtrace.Target, clievent.Command, runtrace.Config, runtrace.Dependencies) (userTraceRecorder, error) {
			return recorder, nil
		},
	}
	options := testExactTraceOptions("trace.ndjson")
	options.verbose = true
	runtime, err := app.newCommandRuntime(clievent.CommandGet, options)
	if err != nil {
		t.Fatal(err)
	}
	observations := newGetObservation(runtime)
	for index, outcome := range []struct {
		stage sessionruntime.ProtocolOperationStage
		cause sessionruntime.ProtocolOperationCause
	}{
		{sessionruntime.ProtocolOperationReceiverEnded, sessionruntime.ProtocolOperationCauseSuperseded},
		{sessionruntime.ProtocolOperationReceiverFailed, sessionruntime.ProtocolOperationCauseProtocolFailure},
	} {
		fact := sessionruntime.NewProtocolOperationObservation(sessionruntime.ProtocolObservationContext{
			ObservedAt: time.Unix(12, 345),
			Correlation: sessionruntime.ProtocolObservationCorrelation{
				Role: protocolsession.RoleReceiver, ProtocolSessionID: protocolsession.ProtocolSessionID{1},
				OperationID: protocolsession.OperationID{byte(index + 2)}, RequestKind: protocolsession.MessageRequestBlocks,
			},
		}, sessionruntime.ProtocolOperationObservation{Stage: outcome.stage, Cause: outcome.cause})
		observations.protocolObservationContext(context.Background(), nil, fact)
	}
	runtime.Close()
	var causes []clievent.ProtocolOperationCause
	for _, event := range recorder.recorded() {
		if event, ok := event.(clievent.ProtocolObservationObserved); ok {
			if fact, ok := event.Fact().(clievent.ProtocolOperationFact); ok {
				causes = append(causes, fact.Cause())
			}
		}
	}
	if len(causes) != 2 || causes[0] != clievent.ProtocolOperationCauseSuperseded || causes[1] != clievent.ProtocolOperationCauseProtocolFailure {
		t.Fatalf("trace lost terminal reasons: %v", causes)
	}
	if output := stderr.String(); strings.Count(output, "Protocol operation request blocks failed") != 1 ||
		!strings.Contains(output, "protocol failure") || strings.Contains(output, "superseded") {
		t.Fatalf("verbose warning classification = %q", output)
	}
}

func TestProtocolAssemblyReportsRejectedFactsAndSkipsRevokedProjection(t *testing.T) {
	emitter := &shareRecordingEmitter{detailed: true}
	observations := newShareProjection(emitter)
	observations.protocolObservationContext(context.Background(), nil, sessionruntime.ProtocolOperationObservation{})
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
}
