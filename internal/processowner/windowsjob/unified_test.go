package windowsjob

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"unicode/utf8"

	ownerprotocol "github.com/windshare/windshare/internal/processowner/protocol"
)

var testIdentity = ownerprotocol.Identity{RunID: "test-run", OperationID: "test-operation", Scenario: "test-scenario"}

func validSupervisionRequest(t *testing.T) supervisionRequest {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	request := ownerprotocol.NewRequest(testIdentity, ownerprotocol.Command{
		Executable: filepath.Clean(executable), Arguments: []string{},
		WorkingDirectory: filepath.Clean(t.TempDir()), Environment: []ownerprotocol.EnvironmentEntry{},
	}, 5_000, 1_000)
	return newSupervisionRequest(request, 0)
}

func TestUnifiedSettlementBuilders(t *testing.T) {
	request := validSupervisionRequest(t)
	root := rootStatus{PID: 42, ExitCode: 23}
	settlement := completedSettlement(request, root, ownerprotocol.TerminationNatural,
		ownerprotocol.InputEvidence{Outcome: ownerprotocol.InputNotRequested}, nil)
	if err := ownerprotocol.ValidateSettlement(settlement); err != nil {
		t.Fatal(err)
	}
	if settlement.TreeState != ownerprotocol.TreeProvenEmpty || settlement.Target.ExitCode == nil || *settlement.Target.ExitCode != 23 {
		t.Fatalf("completed settlement = %#v", settlement)
	}
	spawn := spawnFailedSettlement(request, errors.New("missing"))
	if err := ownerprotocol.ValidateSettlement(spawn); err != nil {
		t.Fatal(err)
	}
	failed := ownerFailedSettlement(request, errors.New("lost authority"))
	if err := ownerprotocol.ValidateSettlement(failed); err != nil {
		t.Fatal(err)
	}
	if failed.TreeState != ownerprotocol.TreeProvenEmpty || failed.Cleanup.Outcome != ownerprotocol.CleanupCompleted {
		t.Fatalf("pre-launch owner failure discarded empty-tree evidence: %#v", failed)
	}
}

func TestOwnerFailurePreservesTreeProofIndependentlyFromTargetEvidence(t *testing.T) {
	request := validSupervisionRequest(t)
	request.Stdin = &ownerprotocol.Stdin{ByteLength: 1}
	request.Protocol.Command.Stdin = request.Stdin
	for _, test := range []struct {
		name          string
		start         targetStartKnowledge
		cleanupErr    error
		targetOutcome string
		inputOutcome  string
		treeState     string
		cleanup       string
	}{
		{
			name: "started and retired", start: targetKnownStarted,
			targetOutcome: ownerprotocol.TargetTerminalEvidenceLost,
			inputOutcome:  ownerprotocol.InputEvidenceLost,
			treeState:     ownerprotocol.TreeProvenEmpty, cleanup: ownerprotocol.CleanupCompleted,
		},
		{
			name: "start unknown and cleanup lost", start: targetStartUnknown,
			cleanupErr:    errors.New("job-empty proof failed"),
			targetOutcome: ownerprotocol.TargetStartEvidenceLost,
			inputOutcome:  ownerprotocol.InputEvidenceLost,
			treeState:     ownerprotocol.TreeUnknown, cleanup: ownerprotocol.CleanupFailed,
		},
		{
			name: "known unstarted and retired", start: targetKnownUnstarted,
			targetOutcome: ownerprotocol.TargetNotStarted,
			inputOutcome:  ownerprotocol.InputNotStarted,
			treeState:     ownerprotocol.TreeProvenEmpty, cleanup: ownerprotocol.CleanupCompleted,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			cause := &authorityTerminationError{
				cause: errors.New("authority lost"), cleanupErr: test.cleanupErr, start: test.start,
			}
			settlement := ownerFailedSettlement(request, cause)
			if err := ownerprotocol.ValidateSettlementForRequest(settlement, request.Protocol); err != nil {
				t.Fatal(err)
			}
			if settlement.Target.Outcome != test.targetOutcome || settlement.Input.Outcome != test.inputOutcome ||
				settlement.TreeState != test.treeState || settlement.Cleanup.Outcome != test.cleanup {
				t.Fatalf("owner failure settlement = %#v", settlement)
			}
		})
	}
}

func TestSettlementPublicationIsCanonicalAndNoClobber(t *testing.T) {
	request := validSupervisionRequest(t)
	var output bytes.Buffer
	sink, err := newSettlementSink(&output, request.Protocol)
	if err != nil {
		t.Fatal(err)
	}
	settlement := spawnFailedSettlement(request, errors.New("missing"))
	if err := sink.publish(settlement); err != nil {
		t.Fatal(err)
	}
	decoded, err := ownerprotocol.DecodeLine[ownerprotocol.Settlement](output.Bytes())
	if err != nil || decoded.Identity != request.Identity {
		t.Fatalf("published settlement = %#v, %v", decoded, err)
	}
	if err := sink.publish(settlement); err == nil {
		t.Fatal("settlement endpoint accepted a second publication")
	}
}

func TestSettlementPublicationConsumesStreamAfterPartialWrite(t *testing.T) {
	request := validSupervisionRequest(t)
	writer := &partialFailureWriter{remaining: 17}
	sink, err := newSettlementSink(writer, request.Protocol)
	if err != nil {
		t.Fatal(err)
	}
	settlement := spawnFailedSettlement(request, errors.New("missing"))
	if err := sink.publish(settlement); err == nil {
		t.Fatal("partial settlement write unexpectedly succeeded")
	}
	partial := writer.output.String()
	if partial == "" || !sink.publicationAttempted() {
		t.Fatalf("partial publication = %q, attempted=%v", partial, sink.publicationAttempted())
	}
	if err := sink.publish(settlement); err == nil {
		t.Fatal("partial publication was followed by a recovery document")
	}
	if writer.output.String() != partial {
		t.Fatal("second publication appended bytes after a partial settlement")
	}
}

func TestSettlementPublicationSettlesOneConcurrentPublisher(t *testing.T) {
	request := validSupervisionRequest(t)
	var output bytes.Buffer
	sink, err := newSettlementSink(&output, request.Protocol)
	if err != nil {
		t.Fatal(err)
	}
	settlement := spawnFailedSettlement(request, errors.New("missing"))
	results := make(chan error, 2)
	var publishers sync.WaitGroup
	publishers.Add(2)
	for range 2 {
		go func() {
			defer publishers.Done()
			results <- sink.publish(settlement)
		}()
	}
	publishers.Wait()
	close(results)
	succeeded := 0
	for err := range results {
		if err == nil {
			succeeded++
		}
	}
	if succeeded != 1 {
		t.Fatalf("successful concurrent publishers = %d", succeeded)
	}
	if _, err := ownerprotocol.DecodeLine[ownerprotocol.Settlement](output.Bytes()); err != nil {
		t.Fatalf("single concurrent settlement = %q: %v", output.Bytes(), err)
	}
}

type partialFailureWriter struct {
	remaining int
	output    bytes.Buffer
}

func (writer *partialFailureWriter) Write(value []byte) (int, error) {
	if writer.remaining <= 0 {
		return 0, errors.New("injected settlement write failure")
	}
	written := min(writer.remaining, len(value))
	_, _ = writer.output.Write(value[:written])
	writer.remaining -= written
	if written != len(value) {
		return written, errors.New("injected settlement write failure")
	}
	return written, nil
}

func TestSettlementPublicationBindsInputToRequest(t *testing.T) {
	request := validSupervisionRequest(t)
	root := rootStatus{PID: 42, ExitCode: 0}
	withoutInput := completedSettlement(request, root, ownerprotocol.TerminationNatural,
		ownerprotocol.InputEvidence{Outcome: ownerprotocol.InputNotRequested}, nil)
	withoutInput.Input = ownerprotocol.InputEvidence{Outcome: ownerprotocol.InputDelivered}
	sink, _ := newTestSettlementSink(t, request)
	if err := sink.publish(withoutInput); err == nil {
		t.Fatal("input-free request published delivered input evidence")
	}

	request.Stdin = &ownerprotocol.Stdin{ByteLength: 1}
	request.Protocol.Command.Stdin = request.Stdin
	withInput := completedSettlement(request, root, ownerprotocol.TerminationNatural,
		ownerprotocol.InputEvidence{Outcome: ownerprotocol.InputDelivered}, nil)
	withInput.Input = ownerprotocol.InputEvidence{Outcome: ownerprotocol.InputNotRequested}
	sink, _ = newTestSettlementSink(t, request)
	if err := sink.publish(withInput); err == nil {
		t.Fatal("declared input published not-requested evidence")
	}
}

func TestDiagnosticBoundPreservesUnicode(t *testing.T) {
	message := boundedDiagnostic(errors.New(strings.Repeat("界", maximumDiagnosticBytes)))
	if len(message) > maximumDiagnosticBytes || !utf8.ValidString(message) {
		t.Fatalf("bounded diagnostic length=%d valid=%v", len(message), utf8.ValidString(message))
	}
}

func TestSuperviseOptionsRequireAttachReadiness(t *testing.T) {
	base := []string{
		commandSupervise,
		"--status-handle", "1",
		"--control-handle", "2",
		"--parent-handle", "3",
		"--start-evidence-handle", "4",
		"--start-decision-handle", "5",
	}
	if _, err := parseSuperviseOptions(base); err == nil {
		t.Fatal("supervise options omitted readiness")
	}
	withReady := append(base, "--ready-stdout")
	if _, err := parseSuperviseOptions(withReady); err != nil {
		t.Fatal(err)
	}
}

func newTestSettlementSink(t *testing.T, request supervisionRequest) (*settlementSink, *bytes.Buffer) {
	t.Helper()
	output := &bytes.Buffer{}
	sink, err := newSettlementSink(output, request.Protocol)
	if err != nil {
		t.Fatal(err)
	}
	return sink, output
}

func decodeTestSettlement(t *testing.T, output *bytes.Buffer) ownerprotocol.Settlement {
	t.Helper()
	settlement, err := ownerprotocol.DecodeLine[ownerprotocol.Settlement](output.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	if err := ownerprotocol.ValidateSettlement(settlement); err != nil {
		t.Fatal(err)
	}
	return settlement
}
