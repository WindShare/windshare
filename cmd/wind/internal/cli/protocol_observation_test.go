package cli

import (
	"bytes"
	"strings"
	"testing"
	"time"

	sessionobs "github.com/windshare/windshare/core/session/observation"
	"github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/internal/diaglog"
)

func protocolAssemblyFact(role protocolsession.Role) sessionobs.ProtocolObservation {
	context := sessionobs.ProtocolObservationContext{
		ObservedAt: time.Unix(12, 345),
		Correlation: sessionobs.ProtocolObservationCorrelation{
			Role: role, ProtocolSessionID: protocolsession.ProtocolSessionID{1},
			OperationID: protocolsession.OperationID(2), RequestKind: protocolsession.MessageListChildren,
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
	for _, command := range []string{commandShare, commandGet} {
		t.Run(command, func(t *testing.T) {
			recorder := newFakeUserTrace(diaglog.RunStatus{})
			app := &App{
				Stderr: bytes.NewBuffer(nil),
				openUserTrace: func(diaglog.Target, string, diaglog.RunOptions) (runTraceRecorder, error) {
					return recorder, nil
				},
			}
			runtime, err := app.newCommandRuntime(command, testExactTraceOptions("trace.ndjson"))
			if err != nil {
				t.Fatal(err)
			}
			role := protocolsession.RoleReceiver
			if command == commandShare {
				role = protocolsession.RoleSender
			}
			fact := protocolAssemblyFact(role)
			runtime.diagnosticSink().Record(fact)
			runtime.diagnosticSink().Lost("protocol", 3)
			runtime.Close()
			var projected int
			for _, rec := range recorder.recorded() {
				if rec.Event != "protocol_operation" {
					continue
				}
				projected++
				if !rec.SourceTime.Equal(fact.ObservedAt()) {
					t.Fatalf("projection changed source timestamp: got %v want %v", rec.SourceTime, fact.ObservedAt())
				}
			}
			if projected != 1 || recorder.droppedCount() != 3 {
				t.Fatalf("projected=%d dropped=%d", projected, recorder.droppedCount())
			}
		})
	}
}

func TestSupersededBlockObservationRetainsTraceWithoutVerboseWarning(t *testing.T) {
	recorder := newFakeUserTrace(diaglog.RunStatus{})
	stderr := bytes.NewBuffer(nil)
	app := &App{
		Stderr: stderr,
		openUserTrace: func(diaglog.Target, string, diaglog.RunOptions) (runTraceRecorder, error) {
			return recorder, nil
		},
	}
	options := testExactTraceOptions("trace.ndjson")
	options.verbose = true
	runtime, err := app.newCommandRuntime(commandGet, options)
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
				OperationID: protocolsession.OperationID(index + 2), RequestKind: protocolsession.MessageRequestBlocks,
			},
		}, sessionobs.ProtocolOperationObservation{Stage: outcome.stage, Cause: outcome.cause})
		runtime.diagnosticSink().Record(fact)
	}
	runtime.Close()
	var causes []string
	for _, rec := range recorder.recorded() {
		if rec.Event == "protocol_operation" {
			var p struct {
				Cause string `json:"cause"`
			}
			decodeRecordPayload(t, rec.Payload, &p)
			causes = append(causes, p.Cause)
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
