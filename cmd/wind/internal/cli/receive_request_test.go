package cli

import (
	"bytes"
	"testing"

	"github.com/windshare/windshare/engine"
)

func TestReceiveCommandsKeepSeparateAdmissionContracts(t *testing.T) {
	capability := newSemanticCapability(t)
	encoded, err := capability.URL("https://relay.example")
	if err != nil {
		t.Fatal(err)
	}
	const operation = "31313131313131313131313131313131"
	for _, test := range []struct {
		name              string
		command           receiveCommand
		options           []string
		outcome           requestParseOutcome
		admission         engine.ReceiveOutputAdmission
		selectedOperation bool
	}{
		{"get defaults", receiveGet, nil, requestParseReady, engine.ReceiveCreateOrResume, false},
		{"retry defaults", receiveResumeRetry, nil, requestParseReady, engine.ReceiveResumeExisting, false},
		{"redownload", receiveGet, []string{"--redownload"}, requestParseReady, engine.ReceiveRedownload, false},
		{"selected retry", receiveResumeRetry, []string{"--operation", operation}, requestParseReady, engine.ReceiveResumeExisting, true},
		{"get cannot select operation", receiveGet, []string{"--operation", operation}, requestParseUsageFailure, 0, false},
		{"retry cannot redownload", receiveResumeRetry, []string{"--redownload"}, requestParseUsageFailure, 0, false},
		{"invalid operation", receiveResumeRetry, []string{"--operation", "invalid"}, requestParseUsageFailure, 0, false},
		{"empty destination", receiveResumeRetry, []string{"-o", ""}, requestParseUsageFailure, 0, false},
	} {
		t.Run(test.name, func(t *testing.T) {
			app := &App{Stderr: &bytes.Buffer{}}
			request, outcome := app.parseReceiveRequest(test.command, append([]string{encoded}, test.options...))
			if outcome != test.outcome {
				t.Fatalf("parse=%v want=%v", outcome, test.outcome)
			}
			if outcome == requestParseReady && (request.admission != test.admission ||
				request.outDir != defaultReceiveOutputDirectory || request.operationID.IsZero() == test.selectedOperation) {
				t.Fatalf("command changed admission semantics: %+v", request)
			}
		})
	}
}

func TestReceiveRecoveryInputCannotChooseUnavailableActions(t *testing.T) {
	pause := engine.ReceivePause{Actions: []engine.ReceiveRecoveryAction{engine.ReceiveRecoveryDiscard, engine.ReceiveRecoveryExit}}
	for _, input := range []string{"r", "retry", " RETRY ", "unknown"} {
		if action, allowed := parseReceiveRecoveryAction(pause, input); allowed {
			t.Fatalf("accepted unavailable action %d from %q", action, input)
		}
	}
	for input, expected := range map[string]engine.ReceiveRecoveryAction{
		"d": engine.ReceiveRecoveryDiscard, " discard ": engine.ReceiveRecoveryDiscard,
		"e": engine.ReceiveRecoveryExit, "EXIT": engine.ReceiveRecoveryExit, "": engine.ReceiveRecoveryExit,
	} {
		if action, allowed := parseReceiveRecoveryAction(pause, input); !allowed || action != expected {
			t.Fatalf("input=%q action=%d allowed=%t", input, action, allowed)
		}
	}
	pause.Actions = append(pause.Actions, engine.ReceiveRecoveryRetry)
	if action, allowed := parseReceiveRecoveryAction(pause, "retry"); !allowed || action != engine.ReceiveRecoveryRetry {
		t.Fatal("retry remained unavailable after the engine enabled it")
	}
}
