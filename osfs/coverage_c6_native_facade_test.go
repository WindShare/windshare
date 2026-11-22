//go:build windows || linux || darwin

package osfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestCoverageC6OutputAuthorityProjectsLeaseAndDiagnostics(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "authority-created-output")
	stream, err := diagnostics.NewStream(256, nil)
	if err != nil {
		t.Fatal(err)
	}
	cfg := OutputConfig{RootPath: root, CreateRoot: true, Diagnostics: stream.Sink()}
	authority, err := NewOutputAuthority(cfg)
	if err != nil {
		t.Fatal(err)
	}
	operation := reserveTestOperation(t, authority, 0xa1)
	intent, ok := operation.ReceiveIntent()
	if !ok {
		t.Fatal("created operation omitted receive intent")
	}

	first, err := operation.OpenDirectTree(ctx, intent)
	if err != nil {
		t.Fatal(err)
	}
	contender, err := NewOutputAuthority(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := contender.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	lookup, err := contender.SelectOperation(ctx, intent.SelectionSpec(), receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != OutputLookupAlreadyRunning {
		t.Fatalf("staged authority contention = (%d, %v)", lookup.Kind(), err)
	}
	if err := contender.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := first.PauseTree(ctx); err != nil {
		t.Fatal(err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}
	reopener, err := NewOutputAuthority(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := reopener.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	reopenedLookup, err := reopener.SelectOperation(ctx, intent.SelectionSpec(), receivecontract.AutomaticOperation())
	if err != nil || reopenedLookup.Kind() != OutputLookupReopened {
		t.Fatalf("reopen lookup = (%d, %v)", reopenedLookup.Kind(), err)
	}
	reopenedIntent, ok := reopenedLookup.Operation().ReceiveIntent()
	if !ok || !reopenedIntent.Equal(intent) {
		t.Fatal("reopen changed its frozen intent")
	}
	reopened, err := reopenedLookup.Operation().OpenDirectTree(ctx, reopenedIntent)
	if err != nil {
		t.Fatalf("lease was not released: %v", err)
	}
	if _, err := reopened.PauseTree(ctx); err != nil {
		t.Fatal(err)
	}
	if err := reopener.Close(); err != nil {
		t.Fatal(err)
	}
	stream.Close()

	wantIntent := diagnostics.Identity(intent.Digest().Bytes()).Text()
	wantOp := diagnostics.Identity(intent.OperationID().Bytes()).Text()
	var destinationBound, destinationAdmitted, pauseMilestones int
	for record := range stream.Records() {
		if record.Name() != "filesystem_output" {
			continue
		}
		corr := record.Correlation()
		if gotIntent := corr.Get("receive_intent_digest").Text(); gotIntent != "" && gotIntent != wantIntent {
			t.Fatalf("diagnostic crossed receive-intent authority: %+v", record)
		}
		payload := record.Payload()
		if payload.Get("operation").Text() != "runtime_decision" {
			continue
		}
		switch payload.Get("runtime_operation").Text() {
		case "admit_destination":
			if corr.Get("receive_operation_id").Text() == "" {
				// Binding precedes operation allocation and reports destination-wide proofs.
				caps := payload.Get("capabilities")
				if !caps.Get("safe_publish").Get("supported").Bool() ||
					!caps.Get("operation_recovery").Get("supported").Bool() ||
					payload.Get("identity_scheme").Text() != "portable-witness-v1" {
					t.Fatalf("destination binding omitted capability or identity_scheme evidence: %+v", payload)
				}
				destinationBound++
				continue
			}
			destinationAdmitted++
			if payload.Get("runtime_decision").Text() != "admitted" ||
				corr.Get("receive_operation_id").Text() != wantOp {
				t.Fatalf("destination admission diagnostic = %+v", record)
			}
		case "pause_tree":
			pauseMilestones++
			if corr.Get("receive_operation_id").Text() != wantOp ||
				corr.Get("output_session_id").Text() == "" {
				t.Fatalf("pause diagnostic = %+v", record)
			}
		}
	}
	if destinationBound < 3 || destinationAdmitted != 2 || pauseMilestones < 2 {
		t.Fatalf(
			"diagnostic counts bound/admitted/pause = %d/%d/%d",
			destinationBound, destinationAdmitted, pauseMilestones,
		)
	}
}

func TestCoverageC6OutputAuthorityRejectsZeroArtifact(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "output-root")
	authority, err := NewOutputAuthority(OutputConfig{RootPath: root, CreateRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	defer authority.Close()
	if err := authority.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := transfer.NewSelectionSpec(
		coverageC6Identity[catalog.ShareInstance](0xa2),
		coverageC6Identity[catalog.DirectoryID](0xa3),
		rules,
	)
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := authority.SelectOperation(ctx, selection, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != OutputLookupCreateReady {
		t.Fatalf("lookup = (%d, %v)", lookup.Kind(), err)
	}
	if _, err := lookup.Create(ctx, receivecontract.ArtifactSpec{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
		t.Fatalf("zero artifact creation = %v, want invalid binding", err)
	}
	if entries, err := os.ReadDir(root); err != nil || len(entries) != 0 {
		t.Fatalf("rejected artifact created output entries: %v, %v", entries, err)
	}
}

func reserveTestOperation(
	t *testing.T,
	authority *OutputAuthority,
	seed byte,
) *OutputOperation {
	t.Helper()
	ctx := context.Background()
	if err := authority.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	rules, err := transfer.NewSelectionRules(true, nil)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := transfer.NewSelectionSpec(
		coverageC6Identity[catalog.ShareInstance](seed),
		coverageC6Identity[catalog.DirectoryID](seed+1),
		rules,
	)
	if err != nil {
		t.Fatal(err)
	}
	lookup, err := authority.SelectOperation(ctx, selection, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != OutputLookupCreateReady {
		t.Fatalf("lookup = (%d, %v)", lookup.Kind(), err)
	}
	artifact, err := receivecontract.NewResultRootDirectoryTree(
		receivecontract.NewSyntheticSelectionResultRoot(),
	)
	if err != nil {
		t.Fatal(err)
	}
	operation, err := lookup.Create(ctx, artifact)
	if err != nil {
		t.Fatal(err)
	}
	return operation
}

func coverageC6Identity[T ~[catalog.IdentityBytes]byte](seed byte) T {
	var identity T
	for index := range identity {
		identity[index] = seed + byte(index)
	}
	return identity
}
