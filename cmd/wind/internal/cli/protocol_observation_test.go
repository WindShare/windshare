package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/runtrace"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/sessionruntime"
	"github.com/windshare/windshare/engine"
	appobs "github.com/windshare/windshare/engine/observation"
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
					projectProtocolFixture(t, app, runtime, command, fact)
				}
				loss = func() {
					observations.reportCumulativeLoss(appobs.LossProtocolQueue, appobs.ObserverLossProtocolOperation, appobs.ObserverLossStreamCapacity, 3)
				}
			} else {
				observations := newGetObservation(runtime)
				project = func(fact sessionruntime.ProtocolObservation) {
					projectProtocolFixture(t, app, runtime, command, fact)
				}
				loss = func() {
					observations.reportCumulativeLoss(appobs.LossProtocolQueue, appobs.ObserverLossProtocolOperation, appobs.ObserverLossStreamCapacity, 3)
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
				case appobs.ProtocolObservationObserved:
					projected++
					if clievent.CommandOf(event) != command || !event.ObservedAt().Equal(fact.ObservedAt()) {
						t.Fatalf("projection changed source context: %+v", event)
					}
				case appobs.ObserverLossObserved:
					if event.Category() == appobs.ObserverLossProtocolOperation && event.Reason() == appobs.ObserverLossStreamCapacity {
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
		projectProtocolFixture(t, app, runtime, clievent.CommandGet, fact)
	}
	runtime.Close()
	var causes []appobs.ProtocolOperationCause
	for _, event := range recorder.recorded() {
		if event, ok := event.(appobs.ProtocolObservationObserved); ok {
			if fact, ok := event.Fact().(appobs.ProtocolOperationFact); ok {
				causes = append(causes, fact.Cause())
			}
		}
	}
	if len(causes) != 2 || causes[0] != appobs.ProtocolOperationCauseSuperseded || causes[1] != appobs.ProtocolOperationCauseProtocolFailure {
		t.Fatalf("trace lost terminal reasons: %v", causes)
	}
	if output := stderr.String(); strings.Count(output, "Protocol operation request blocks failed") != 1 ||
		!strings.Contains(output, "protocol failure") || strings.Contains(output, "superseded") {
		t.Fatalf("verbose warning classification = %q", output)
	}
}

func TestProtocolAssemblyReportsRejectedFacts(t *testing.T) {
	emitter := &shareRecordingEmitter{detailed: true}
	loss, err := appobs.NewObserverLossObserved(appobs.ObserverLossSpec{Workflow: appobs.WorkflowShare, Category: appobs.ObserverLossProtocolOperation, Reason: appobs.ObserverLossEventContract, Count: 1})
	if err != nil {
		t.Fatal(err)
	}
	(&App{}).observeEngineShare(newShareProjection(emitter), loss)
	if emitter.lifecycleLoss != 1 {
		t.Fatalf("rejected facts=%d", emitter.lifecycleLoss)
	}
}
func projectProtocolFixture(t *testing.T, app *App, runtime *commandRuntime, command clievent.Command, fact sessionruntime.ProtocolObservation) {
	t.Helper()
	event, err := engine.DescribeProtocolObservation(appobs.Workflow(command), fact)
	if err != nil {
		t.Fatal(err)
	}
	if command == clievent.CommandShare {
		app.observeEngineShare(newShareProjection(runtime), event)
	} else {
		app.projectReceiveObservation(newGetObservation(runtime), event)
	}
}
