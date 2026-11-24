//go:build windows || linux || darwin

package osfs

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func TestPortablePublicationCollisionRetainsDiscardAuthority(t *testing.T) {
	ctx := context.Background()
	root, intent := pausedPortableFile(t, 4)
	stage := onlyPortableStage(t, root)
	final := filepath.Join(nativeDirectTreeResultRoot(t, root, intent), "file.bin")

	authority, err := NewOutputAuthority(OutputConfig{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	lookup, err := authority.SelectOperation(ctx, intent.SelectionSpec(), receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != OutputLookupReopened {
		t.Fatalf("reopen lookup = (%v, %v)", lookup.Kind(), err)
	}
	reopenedIntent, _ := lookup.Operation().ReceiveIntent()
	session, err := lookup.Operation().OpenDirectTree(ctx, reopenedIntent)
	if err != nil {
		t.Fatal(err)
	}
	admission := admitRetainedCandidateRoot(t, session, intent, 0x62)
	file := nativeDirectTreeTestFile(t, session, intent, 0x63, "file.bin", 4, admission)
	start, err := session.BeginFile(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	tx, _, ok := start.Transaction()
	if !ok {
		t.Fatalf("expected resumable transaction before commit collision, got %+v", start)
	}
	// Occupy the final path after admission to exercise the commit-time
	// no-replace race while keeping preflight admission valid.
	if err := os.WriteFile(final, []byte("user"), 0o600); err != nil {
		t.Fatal(err)
	}
	settlement, err := tx.Commit(ctx)
	if err != nil || settlement.Kind() != transfer.FileCollision {
		t.Fatalf("commit collision = (%v, %v), want FileCollision", settlement.Kind(), err)
	}
	if _, err := session.PauseTree(ctx); err != nil {
		t.Fatal(err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}

	// Discard must delete only private staged data and preserve the user's colliding file.
	recoveryAuthority := NewRecoveryAuthority(root)
	result, err := recoveryAuthority.Discard(ctx, intent.OperationID())
	if err != nil || result.State() != RecoveryOperationDiscarded {
		t.Fatalf("discard after publication collision: %v %v", result.State(), err)
	}
	if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned partial remains after discard: %v", err)
	}
	if actual, err := os.ReadFile(final); err != nil || string(actual) != "user" {
		t.Fatalf("discard changed collision: %q %v", actual, err)
	}
}

func TestPortableResumePreflightCollisionRetainsDiscardAuthority(t *testing.T) {
	ctx := context.Background()
	root, intent := pausedPortableFile(t, 4)
	stage := onlyPortableStage(t, root)
	final := filepath.Join(nativeDirectTreeResultRoot(t, root, intent), "file.bin")
	if err := os.WriteFile(final, []byte("user"), 0o600); err != nil {
		t.Fatal(err)
	}

	authority, err := NewOutputAuthority(OutputConfig{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	if err := authority.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	lookup, err := authority.SelectOperation(ctx, intent.SelectionSpec(), receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != OutputLookupReopened {
		t.Fatalf("reopen lookup = (%v, %v)", lookup.Kind(), err)
	}
	reopenedIntent, _ := lookup.Operation().ReceiveIntent()
	session, err := lookup.Operation().OpenDirectTree(ctx, reopenedIntent)
	if err != nil {
		t.Fatal(err)
	}
	admission := admitRetainedCandidateRoot(t, session, intent, 0x62)
	file := nativeDirectTreeTestFile(t, session, intent, 0x63, "file.bin", 4, admission)
	start, err := session.BeginFile(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	settlement, immediate := start.ImmediateSettlement()
	if !immediate || settlement.Kind() != transfer.FileCollision {
		t.Fatalf("preflight collision = (immediate=%v, kind=%v), want FileCollision", immediate, settlement.Kind())
	}
	if _, err := session.PauseTree(ctx); err != nil {
		t.Fatal(err)
	}
	if err := authority.Close(); err != nil {
		t.Fatal(err)
	}

	// Discard must delete only private staged data and preserve the user's colliding file.
	recoveryAuthority := NewRecoveryAuthority(root)
	result, err := recoveryAuthority.Discard(ctx, intent.OperationID())
	if err != nil || result.State() != RecoveryOperationDiscarded {
		t.Fatalf("discard after preflight collision: %v %v", result.State(), err)
	}
	if _, err := os.Stat(stage); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("owned partial remains after discard: %v", err)
	}
	if actual, err := os.ReadFile(final); err != nil || string(actual) != "user" {
		t.Fatalf("discard changed collision: %q %v", actual, err)
	}
}
