package cli

import (
	"bytes"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer"

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

func TestReceiveCommandsNormalizeEquivalentCatalogSelections(t *testing.T) {
	capability := newSemanticCapability(t)
	encoded, err := capability.URL("https://relay.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []receiveCommand{receiveGet, receiveResumeRetry} {
		t.Run(string(command), func(t *testing.T) {
			app := &App{Stderr: &bytes.Buffer{}}
			request, outcome := app.parseReceiveRequest(command, []string{
				encoded, "--only", "tree/", "--only", "cafe\u0301/",
				"--only", "tree", "--only", "caf\u00e9", "--only", " spaced ",
			})
			want := []string{"tree", "caf\u00e9", " spaced "}
			wantTyped := []string{"tree/", "cafe\u0301/", " spaced "}
			if outcome != requestParseReady || !slices.Equal(request.selection.paths, want) ||
				!slices.Equal(request.selection.typed, wantTyped) {
				t.Fatalf("selection=%q typed=%q outcome=%d", request.selection.paths, request.selection.typed, outcome)
			}
			if _, err := transfer.NewPathSelectionRules(request.selection.paths); err != nil {
				t.Fatalf("CLI selection violated the core contract: %v", err)
			}
		})
	}
}

func TestInvalidReceiveSelectionReportsTheOptionBeforeRequestingAKey(t *testing.T) {
	capability := newSemanticCapability(t)
	bare, _, err := capability.SplitURL("https://relay.example")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"", "./tree", "../escape", "tree//file.bin", "/tree", "tree//", "tree\\file", ".wsresume"} {
		for _, command := range []receiveCommand{receiveGet, receiveResumeRetry} {
			var stderr bytes.Buffer
			// No input stream or engine: invalid paths must stop at argument parsing.
			app := &App{Stderr: &stderr}
			_, outcome := app.parseReceiveRequest(command, []string{bare, "--only", path})
			if outcome != requestParseUsageFailure || !strings.Contains(stderr.String(), "--only") {
				t.Fatalf("path=%q outcome=%d diagnostic=%q", path, outcome, stderr.String())
			}
		}
	}
}

func TestReceiveSelectionBoundsApplyAfterDeduplication(t *testing.T) {
	paths := make([]string, transfer.MaxSelectionPathTargets+1)
	for i := range paths {
		paths[i] = fmt.Sprintf("file-%04d", i)
	}
	if _, err := normalizeReceiveSelection(paths); err == nil {
		t.Fatal("accepted too many distinct catalog paths")
	}
	paths[len(paths)-1] = paths[0] + "/"
	normalized, err := normalizeReceiveSelection(paths)
	if err != nil || len(normalized.paths) != transfer.MaxSelectionPathTargets {
		t.Fatalf("duplicate consumed a path slot: count=%d err=%v", len(normalized.paths), err)
	}
	prefix := strings.Repeat("a", catalog.MaxNameBytes) + "/" + strings.Repeat("b", catalog.MaxNameBytes) + "/"
	paths = paths[:0]
	for totalBytes := 0; totalBytes <= transfer.MaxSelectionPathTargetBytes; {
		path := fmt.Sprintf("%sfile-%04d", prefix, len(paths))
		paths = append(paths, path)
		totalBytes += len(path)
	}
	if _, err := normalizeReceiveSelection(paths); err == nil {
		t.Fatal("accepted catalog paths beyond the total byte limit")
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
