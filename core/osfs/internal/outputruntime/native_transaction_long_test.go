package outputruntime

import (
	"bytes"
	"context"
	"crypto/sha256"
	"io"
	"os"
	"testing"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
)

const (
	longStreamingFileSize  = uint64(32 * 1024 * 1024)
	longStreamingBlockSize = uint64(1024 * 1024)
)

func TestLongNativeOrdinaryRestartResumesLargeStreamingFile(t *testing.T) {
	if testing.Short() {
		t.Skip("long native restart scenario")
	}
	root := newRuntimeTestRootSpec(t).path
	fixture := openOrdinaryResumeSession(t, root, 0x71, longStreamingFileSize)
	block := bytes.Repeat([]byte{0xa7}, int(longStreamingBlockSize))
	restartCut := longStreamingFileSize / 2

	for offset := uint64(0); offset < restartCut; offset += longStreamingBlockSize {
		if err := fixture.transaction.WriteRange(context.Background(), offset, block); err != nil {
			t.Fatal(err)
		}
	}
	durable, err := fixture.transaction.Checkpoint(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	assertSingleDurableRange(t, durable, content.Range{Offset: 0, End: restartCut})
	pauseOrdinaryResumeFixture(t, fixture)

	reopened := reopenOrdinaryResumeFile(t, root, 0x71, longStreamingFileSize)
	if reopened.transaction == nil {
		t.Fatalf("partial file reopened as settlement %d", reopened.settlement.Kind())
	}
	assertSingleDurableRange(t, reopened.durable, content.Range{Offset: 0, End: restartCut})

	// Descending writes prove resume is genuinely WriteAt-based while the one
	// reusable block keeps memory independent of the artifact size.
	for offset := int64(longStreamingFileSize - longStreamingBlockSize); offset >= int64(restartCut); offset -= int64(longStreamingBlockSize) {
		if err := reopened.transaction.WriteRange(context.Background(), uint64(offset), block); err != nil {
			t.Fatal(err)
		}
	}
	settlement, err := reopened.transaction.Commit(context.Background())
	if err != nil || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("large commit = %d, %v", settlement.Kind(), err)
	}
	assertRepeatedBlockFile(t, reopened.finalPath, block, longStreamingFileSize/longStreamingBlockSize)
	if tree, err := reopened.session.PauseTree(
		context.Background(), transfer.JobPauseInterrupted,
	); err != nil || tree.Kind() != transfer.DirectTreeSettlementPaused {
		t.Fatalf("post-publish pause = %d, %v", tree.Kind(), err)
	}
	if err := reopened.authority.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestLongNativeOrdinaryRestartReusesPublishedFile(t *testing.T) {
	if testing.Short() {
		t.Skip("long native restart scenario")
	}
	root := newRuntimeTestRootSpec(t).path
	payload := []byte("already-published")
	fixture := openOrdinaryResumeSession(t, root, 0x79, uint64(len(payload)))
	if err := fixture.transaction.WriteRange(context.Background(), 0, payload); err != nil {
		t.Fatal(err)
	}
	if _, err := fixture.transaction.Checkpoint(context.Background()); err != nil {
		t.Fatal(err)
	}
	settlement, err := fixture.transaction.Commit(context.Background())
	if err != nil || settlement.Kind() != transfer.FilePublished {
		t.Fatalf("first publication = %d, %v", settlement.Kind(), err)
	}
	if tree, err := fixture.session.PauseTree(
		context.Background(), transfer.JobPauseInterrupted,
	); err != nil || tree.Kind() != transfer.DirectTreeSettlementPaused {
		t.Fatalf("published-tree pause = %d, %v", tree.Kind(), err)
	}
	if err := fixture.authority.Close(); err != nil {
		t.Fatal(err)
	}

	reopened := reopenOrdinaryResumeFile(t, root, 0x79, uint64(len(payload)))
	if reopened.transaction != nil || reopened.settlement.Kind() != transfer.FilePublished {
		t.Fatalf("published file reopened as transaction=%t settlement=%d", reopened.transaction != nil, reopened.settlement.Kind())
	}
	checkpoint, ok := reopened.settlement.VerifiedCheckpoint()
	if !ok {
		t.Fatal("published settlement omitted its verified checkpoint")
	}
	assertSingleDurableRange(t, checkpoint, content.Range{Offset: 0, End: uint64(len(payload))})
	actual, err := os.ReadFile(reopened.finalPath)
	if err != nil || !bytes.Equal(actual, payload) {
		t.Fatalf("reused final = %q, %v", actual, err)
	}
	if tree, err := reopened.session.PauseTree(
		context.Background(), transfer.JobPauseInterrupted,
	); err != nil || tree.Kind() != transfer.DirectTreeSettlementPaused {
		t.Fatalf("reused-tree pause = %d, %v", tree.Kind(), err)
	}
	if err := reopened.authority.Close(); err != nil {
		t.Fatal(err)
	}
}

func assertSingleDurableRange(
	t *testing.T,
	durable transfer.VerifiedDurableRanges,
	want content.Range,
) {
	t.Helper()
	ranges := durable.Ranges().Ranges()
	if len(ranges) != 1 || ranges[0] != want || durable.CheckpointGeneration() == 0 {
		t.Fatalf("durable ranges = %+v generation=%d, want %+v", ranges, durable.CheckpointGeneration(), want)
	}
}

func assertRepeatedBlockFile(t *testing.T, path string, block []byte, count uint64) {
	t.Helper()
	expected := sha256.New()
	for range count {
		_, _ = expected.Write(block)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	actual := sha256.New()
	written, copyErr := io.Copy(actual, file)
	closeErr := file.Close()
	if copyErr != nil || closeErr != nil || written != int64(uint64(len(block))*count) ||
		!bytes.Equal(actual.Sum(nil), expected.Sum(nil)) {
		t.Fatalf("streamed final = bytes %d, copy %v, close %v", written, copyErr, closeErr)
	}
}
