package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/cmd/wind/internal/clievent"
	"github.com/windshare/windshare/cmd/wind/internal/runtrace"
	"github.com/windshare/windshare/core/session/protocolsession"

	sessionobs "github.com/windshare/windshare/core/session/observation"
	appobs "github.com/windshare/windshare/engine/observation"
)

func protocolAssemblyFact(role protocolsession.Role) sessionobs.ProtocolObservation {
	context := sessionobs.ProtocolObservationContext{
		ObservedAt: time.Unix(12, 345),
		Correlation: sessionobs.ProtocolObservationCorrelation{
			Role: role, ProtocolSessionID: protocolsession.ProtocolSessionID{1},
			OperationID: protocolsession.OperationID{2}, RequestKind: protocolsession.MessageListChildren,
		},
	}
	stage := sessionobs.ProtocolOperationReceiverFailed
	cause := sessionobs.ProtocolOperationCauseCanceled
	if role == protocolsession.RoleSender {
		stage, cause = sessionobs.ProtocolOperationSenderRequestReceived, sessionobs.ProtocolOperationCauseNone
	}
	return sessionobs.NewProtocolOperationObservation(context, sessionobs.ProtocolOperationObservation{
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
			if command == clievent.CommandShare {
				role = protocolsession.RoleSender
			}
			fact := protocolAssemblyFact(role)
			runtime.diagnosticSink().Record(fact)
			runtime.diagnosticSink().Lost("protocol", 3)
			runtime.Close()
			var projected int
			var lost uint64
			for _, event := range recorder.recorded() {
				switch event := event.(type) {
				case clievent.Diagnostic:
					if event.Record.Name() != "protocol_operation" {
						continue
					}
					projected++
					if clievent.CommandOf(event) != command || !event.Record.ObservedAt().Equal(fact.ObservedAt()) {
						t.Fatalf("projection changed source context: %+v", event)
					}
				case appobs.ObserverLossObserved:
					if event.Category() == appobs.ObserverLossCommandAdapter && event.Reason() == appobs.ObserverLossStreamCapacity {
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
		stage sessionobs.ProtocolOperationStage
		cause sessionobs.ProtocolOperationCause
	}{
		{sessionobs.ProtocolOperationReceiverEnded, sessionobs.ProtocolOperationCauseSuperseded},
		{sessionobs.ProtocolOperationReceiverFailed, sessionobs.ProtocolOperationCauseProtocolFailure},
	} {
		fact := sessionobs.NewProtocolOperationObservation(sessionobs.ProtocolObservationContext{
			ObservedAt: time.Unix(12, 345),
			Correlation: sessionobs.ProtocolObservationCorrelation{
				Role: protocolsession.RoleReceiver, ProtocolSessionID: protocolsession.ProtocolSessionID{1},
				OperationID: protocolsession.OperationID{byte(index + 2)}, RequestKind: protocolsession.MessageRequestBlocks,
			},
		}, sessionobs.ProtocolOperationObservation{Stage: outcome.stage, Cause: outcome.cause})
		runtime.diagnosticSink().Record(fact)
	}
	runtime.Close()
	var causes []string
	for _, event := range recorder.recorded() {
		if event, ok := event.(clievent.Diagnostic); ok && event.Record.Name() == "protocol_operation" {
			causes = append(causes, event.Record.Payload().Get("cause").Text())
		}
	}
	if len(causes) != 2 || causes[0] != "superseded" || causes[1] != "protocol_failure" {
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
