package osfs

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/resumestate"
	"github.com/windshare/windshare/core/transfer"
)

func TestOutputV3TerminalRecoveryPreflightsGlobalFileStateBudgetBeforeMutation(t *testing.T) {
	root := v3RecoveryRoot(t)
	path := v3RecoveryPathBeforeTerminalOverflowShard(t)
	selection := v3RecoverySelectionPaths(t, []string{path}, 1)
	sessionIDs := &v3RecoverySessionIDs{}
	authority := v3RecoveryAuthority(t, root, sessionIDs)
	opened := v3RecoveryOpen(t, authority, root, selection)
	transaction := v3RecoveryBeginTransaction(
		t, opened.Session, v3RecoveryOutputFileAt(t, opened.Session, selection, 0),
	).(*filesystemFileTransaction)
	recordName := resumestate.FileRecordName(transaction.resumable.Bound().Record().LocatorDigest())
	if err := transaction.WriteRange(context.Background(), 0, []byte{0x7a}); err != nil {
		t.Fatal(err)
	}
	settlement, err := transaction.Commit(context.Background())
	if err != nil || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("publish early-shard record = (%v, %v), want FilePublished", settlement.Kind(), err)
	}

	sessionPath := v3RecoverySessionPath(root, selection, opened.Session.SessionID())
	recordPath := filepath.Join(
		sessionPath, resumestate.FilesDirectoryName, recordName.Shard(), recordName.Name(),
	)
	publishedBytes, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	published, err := resumestate.DecodeFileRecord(publishedBytes)
	if err != nil || published.Phase() != resumestate.FilePublished {
		t.Fatalf("early record = (phase=%v, err=%v), want FilePublished", published.Phase(), err)
	}
	nonce, err := resumestate.UpdateNonceFromBytes(bytes.Repeat([]byte{0x62}, resumestate.UpdateNonceBytes))
	if err != nil {
		t.Fatal(err)
	}
	temporaryName := resumestate.UpdateTemporaryName(published.LocatorDigest(), nonce)
	if temporaryName.Shard() != recordName.Shard() {
		t.Fatal("canonical update temporary escaped the published record shard")
	}
	earlyShard, err := opened.Session.filesDir.OpenDirectory(recordName.Shard(), true)
	if err != nil {
		t.Fatal(err)
	}
	temporary, err := earlyShard.CreateFile(temporaryName.Name(), true, int64(len(publishedBytes)))
	if err != nil {
		t.Fatal(err)
	}
	written, err := temporary.WriteAt(publishedBytes, 0)
	if err != nil || written != len(publishedBytes) {
		t.Fatalf("write recoverable update temporary = (%d, %v)", written, err)
	}
	if err := errors.Join(temporary.Sync(), earlyShard.Sync(), temporary.Close(), earlyShard.Close()); err != nil {
		t.Fatal(err)
	}

	lateShard, err := opened.Session.filesDir.CreateDirectory(v3RecoveryTerminalOverflowShard, true)
	if err != nil {
		t.Fatal(err)
	}
	opaqueBytes := []byte("late auxiliary entry")
	opaque, err := lateShard.CreateFile("opaque", true, int64(len(opaqueBytes)))
	if err != nil {
		t.Fatal(err)
	}
	written, err = opaque.WriteAt(opaqueBytes, 0)
	if err != nil || written != len(opaqueBytes) {
		t.Fatalf("write late auxiliary entry = (%d, %v)", written, err)
	}
	if err := errors.Join(
		opaque.Sync(), lateShard.Sync(), opened.Session.filesDir.Sync(), opaque.Close(), lateShard.Close(),
	); err != nil {
		t.Fatal(err)
	}

	temporaryPath := filepath.Join(
		sessionPath, resumestate.FilesDirectoryName, temporaryName.Shard(), temporaryName.Name(),
	)
	earlyBefore := v3RecoveryReadTerminalPreflightEntries(t, recordPath, temporaryPath)
	if err := opened.Session.installLifecycle(resumestate.SessionCompleting); err != nil {
		t.Fatal(err)
	}
	v3RecoveryCloseSession(t, opened.Session)

	_, err = v3OpenSelection(
		context.Background(), v3RecoveryAuthority(t, root, sessionIDs), selection,
	)
	if !errors.Is(err, resumestate.ErrFileStateNamespaceLimit) ||
		v3RecoveryFaultScope(err) != transfer.OutputFaultSession {
		t.Fatalf("terminal global-budget recovery error = %v, want session-scoped namespace limit", err)
	}
	earlyAfter := v3RecoveryReadTerminalPreflightEntries(t, recordPath, temporaryPath)
	for index := range earlyBefore {
		if !bytes.Equal(earlyAfter[index], earlyBefore[index]) {
			t.Fatalf("early-shard entry %d changed before global namespace preflight completed", index)
		}
	}
}

const v3RecoveryTerminalOverflowShard = "ff"

func v3RecoveryPathBeforeTerminalOverflowShard(t *testing.T) string {
	t.Helper()
	for index := 0; index < resumestate.MaxFileStateShardDirectories; index++ {
		candidate := fmt.Sprintf("terminal-budget-%03d.bin", index)
		name := resumestate.FileRecordName(resumestate.DigestCanonicalLocator(candidate))
		if name.Shard() < v3RecoveryTerminalOverflowShard {
			return candidate
		}
	}
	t.Fatal("could not construct an early file-state shard")
	return ""
}

func v3RecoveryReadTerminalPreflightEntries(t *testing.T, paths ...string) [][]byte {
	t.Helper()
	contents := make([][]byte, len(paths))
	for index, path := range paths {
		encoded, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read terminal preflight entry %q: %v", path, err)
		}
		contents[index] = encoded
	}
	return contents
}
