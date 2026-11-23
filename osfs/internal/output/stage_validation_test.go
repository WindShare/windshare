//go:build windows || linux || darwin

package output

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/diagnostics"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

func makePatternPayload(size int, seed byte) []byte {
	buf := make([]byte, size)
	for i := range buf {
		buf[i] = byte((i*31 + int(seed)*17 + i/257) & 0xff)
	}
	return buf
}

func setupPausedPartialFile(
	t *testing.T,
	seed byte,
	fileName string,
	exactSize uint64,
	writes []struct {
		offset uint64
		data   []byte
	},
	explicitCheckpoint bool,
) (root string, sel transfer.SelectionSpec, intent transfer.ReceiveIntent, partPath, finalPath string) {
	t.Helper()
	ctx := context.Background()
	root = t.TempDir()
	auth, err := New(Config{RootPath: root, CreateRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	sel = testSelectionSpec(t, seed)
	lookup, err := auth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != LookupCreateReady {
		t.Fatalf("select = (%v, %v)", lookup.Kind(), err)
	}
	art, err := receivecontract.NewResultRootDirectoryTree(receivecontract.NewSyntheticSelectionResultRoot())
	if err != nil {
		t.Fatal(err)
	}
	op, err := auth.CreateOperation(ctx, lookup, art)
	if err != nil {
		t.Fatal(err)
	}
	intent, _ = op.ReceiveIntent()
	session, err := auth.OpenOperation(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	rootAdm := testAdmitRoot(t, session, intent, seed+1)
	file := testMaterializationFile(t, session, intent, seed+2, fileName, exactSize, rootAdm)
	start, err := session.BeginFile(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	tx, _, ok := start.Transaction()
	if !ok {
		t.Fatal("expected active file transaction")
	}
	for _, w := range writes {
		if err := tx.WriteRange(ctx, w.offset, w.data); err != nil {
			t.Fatal(err)
		}
	}
	if explicitCheckpoint {
		if _, err := tx.Checkpoint(ctx); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := tx.Pause(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := session.PauseTree(ctx); err != nil {
		t.Fatal(err)
	}
	if err := auth.Close(); err != nil {
		t.Fatal(err)
	}

	opDir := filepath.Join(root, ControlDirName, hex.EncodeToString(intent.OperationID().Bytes()))
	partPath = filepath.Join(opDir, partFileName(file.Descriptor().FileID(), file.Target().Locator().Digest()))
	res, _ := intent.MaterializationPlan().DestinationReservation()
	finalPath = filepath.Join(root, res.PhysicalName(), fileName)
	return root, sel, intent, partPath, finalPath
}

func TestStageRangeSampleWindowsNeverReadUnwrittenHoles(t *testing.T) {
	if fp, err := sampleStageRangesFingerprint(bytes.NewReader(nil), nil); err != nil || fp != ([stageFingerprintBytes]byte{}) {
		t.Fatalf("empty ranges fingerprint = (%x, %v), want zero and nil", fp, err)
	}

	if small := stageRangeSampleWindows([]content.Range{{Offset: 0, End: 100}}); len(small) != 1 || small[0] != (content.Range{Offset: 0, End: 100}) {
		t.Fatalf("small range windows = %+v, want single [0,100)", small)
	}
	if large := stageRangeSampleWindows([]content.Range{{Offset: 0, End: 32768}}); len(large) != 3 ||
		large[0] != (content.Range{Offset: 0, End: 4096}) ||
		large[1] != (content.Range{Offset: 14336, End: 18432}) ||
		large[2] != (content.Range{Offset: 28672, End: 32768}) {
		t.Fatalf("large contiguous windows = %+v", large)
	}
	tiny := []content.Range{{Offset: 0, End: 10}, {Offset: 100, End: 110}, {Offset: 200, End: 210}}
	if got := stageRangeSampleWindows(tiny); !slices.Equal(got, tiny) {
		t.Fatalf("tiny disjoint windows = %+v, want %+v", got, tiny)
	}

	ranges := []content.Range{
		{Offset: 0, End: 4096},
		{Offset: 16384, End: 20480},
	}
	windows := stageRangeSampleWindows(ranges)
	if len(windows) == 0 || len(windows) > 3 {
		t.Fatalf("unexpected sample window count: %v", windows)
	}
	for _, w := range windows {
		inside := false
		for _, r := range ranges {
			if w.Offset >= r.Offset && w.End <= r.End && w.End-w.Offset <= stageSampleBytes {
				inside = true
				break
			}
		}
		if !inside {
			t.Fatalf("sample window %+v escapes covered ranges %+v", w, ranges)
		}
	}

	base := makePatternPayload(20480, 0x11)
	fpBase, err := sampleStageRangesFingerprint(bytes.NewReader(base), ranges)
	if err != nil {
		t.Fatal(err)
	}

	// Mutating the unwritten hole and extending the file beyond maxRangeEnd must
	// not alter the checkpoint fingerprint.
	grown := append(slices.Clone(base), makePatternPayload(4096, 0x22)...)
	for i := 4096; i < 16384; i++ {
		grown[i] ^= 0xff
	}
	fpGrown, err := sampleStageRangesFingerprint(bytes.NewReader(grown), ranges)
	if err != nil {
		t.Fatal(err)
	}
	if fpGrown != fpBase {
		t.Fatal("hole fill or post-checkpoint growth altered stage range fingerprint")
	}

	// Mutating bytes inside a covered range must change the fingerprint.
	corrupted := slices.Clone(base)
	corrupted[16384+10] ^= 0xff
	fpCorrupted, err := sampleStageRangesFingerprint(bytes.NewReader(corrupted), ranges)
	if err != nil {
		t.Fatal(err)
	}
	if fpCorrupted == fpBase {
		t.Fatal("mutated covered range produced identical fingerprint")
	}
}

func TestResumeRejectsReplacedOrZeroedStageAndRedownloadsCleanly(t *testing.T) {
	const (
		totalSize  = 32 * 1024
		prefixSize = 16 * 1024
	)
	expected := makePatternPayload(totalSize, 0x31)

	for _, tc := range []struct {
		name        string
		seed        byte
		replacement []byte
	}{
		{
			name:        "different content same length",
			seed:        0x41,
			replacement: makePatternPayload(prefixSize, 0x99),
		},
		{
			name:        "different content larger within exact size",
			seed:        0x45,
			replacement: makePatternPayload(24*1024, 0xaa),
		},
		{
			name:        "zero filled placeholder",
			seed:        0x49,
			replacement: make([]byte, prefixSize),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			root, sel, _, partPath, finalPath := setupPausedPartialFile(
				t, tc.seed, "replaced.bin", totalSize,
				[]struct {
					offset uint64
					data   []byte
				}{{offset: 0, data: expected[:prefixSize]}},
				false,
			)
			if err := os.WriteFile(partPath, tc.replacement, 0o600); err != nil {
				t.Fatal(err)
			}

			stream, err := diagnostics.NewStream(64, nil)
			if err != nil {
				t.Fatal(err)
			}
			reopen, err := New(Config{RootPath: root, Diagnostics: stream.Sink()})
			if err != nil {
				t.Fatal(err)
			}
			defer reopen.Close()
			if err := reopen.BindDestination(ctx); err != nil {
				t.Fatal(err)
			}
			lookup, err := reopen.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
			if err != nil || lookup.Kind() != LookupReopened {
				t.Fatalf("reopen = (%v, %v)", lookup.Kind(), err)
			}
			reIntent, _ := lookup.Operation().ReceiveIntent()
			session, err := reopen.OpenOperation(ctx, lookup.Operation())
			if err != nil {
				t.Fatal(err)
			}
			rootAdm := testAdmitRoot(t, session, reIntent, tc.seed+1)
			file := testMaterializationFile(t, session, reIntent, tc.seed+2, "replaced.bin", totalSize, rootAdm)

			start, err := session.BeginFile(ctx, file)
			if err != nil {
				t.Fatal(err)
			}
			tx, durable, ok := start.Transaction()
			if !ok {
				t.Fatalf("expected fresh transaction after rejecting replaced stage, got %+v", start)
			}
			if !durable.Ranges().IsEmpty() {
				t.Fatalf("replaced stage unexpectedly reused ranges: %v", durable.Ranges().Ranges())
			}

			sawInvalidTrace := false
			for {
				select {
				case rec := <-stream.Records():
					p := rec.Payload()
					if p.Get("runtime_operation").Text() == "recover_file" &&
						p.Get("checkpoint_decision").Text() == "invalid" {
						sawInvalidTrace = true
					}
				default:
					goto drained
				}
			}
		drained:
			if !sawInvalidTrace {
				t.Fatal("expected filesystem_output trace with checkpoint_decision=invalid")
			}

			if err := tx.WriteRange(ctx, 0, expected); err != nil {
				t.Fatal(err)
			}
			if settled, err := tx.Commit(ctx); err != nil || settled.Kind() != transfer.FilePublished {
				t.Fatalf("commit after clean redownload = (%v, %v)", settled.Kind(), err)
			}
			if _, err := session.FinalizeDirectory(ctx, rootAdm); err != nil {
				t.Fatal(err)
			}
			if _, err := session.FinalizeTree(ctx, transfer.DirectTreeOutcomeSuccess); err != nil {
				t.Fatal(err)
			}
			got, err := os.ReadFile(finalPath)
			if err != nil || !bytes.Equal(got, expected) {
				t.Fatalf("published file mismatch: err=%v len=%d want=%d", err, len(got), len(expected))
			}
		})
	}
}

func TestResumeRejectsOversizedStageWithoutPublishingTail(t *testing.T) {
	t.Run("oversized partial with matching prefix", func(t *testing.T) {
		ctx := context.Background()
		const (
			seed       byte = 0x51
			totalSize       = 32 * 1024
			prefixSize      = 16 * 1024
		)
		expected := makePatternPayload(totalSize, seed)
		root, sel, _, partPath, finalPath := setupPausedPartialFile(
			t, seed, "oversized.bin", totalSize,
			[]struct {
				offset uint64
				data   []byte
			}{{offset: 0, data: expected[:prefixSize]}},
			false,
		)
		// Keep the exact checkpointed prefix intact but append bytes past exactSize.
		oversized := append(slices.Clone(expected), []byte("UNEXPECTED-TAIL-BYTES")...)
		if err := os.WriteFile(partPath, oversized, 0o600); err != nil {
			t.Fatal(err)
		}

		reopen, err := New(Config{RootPath: root})
		if err != nil {
			t.Fatal(err)
		}
		defer reopen.Close()
		if err := reopen.BindDestination(ctx); err != nil {
			t.Fatal(err)
		}
		lookup, err := reopen.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
		if err != nil || lookup.Kind() != LookupReopened {
			t.Fatalf("reopen = (%v, %v)", lookup.Kind(), err)
		}
		reIntent, _ := lookup.Operation().ReceiveIntent()
		session, err := reopen.OpenOperation(ctx, lookup.Operation())
		if err != nil {
			t.Fatal(err)
		}
		rootAdm := testAdmitRoot(t, session, reIntent, seed+1)
		file := testMaterializationFile(t, session, reIntent, seed+2, "oversized.bin", totalSize, rootAdm)

		start, err := session.BeginFile(ctx, file)
		if err != nil {
			t.Fatal(err)
		}
		tx, durable, ok := start.Transaction()
		if !ok || !durable.Ranges().IsEmpty() {
			t.Fatalf("oversized stage must discard ranges and recreate clean stage: ok=%v ranges=%v", ok, durable.Ranges().Ranges())
		}
		if err := tx.WriteRange(ctx, 0, expected); err != nil {
			t.Fatal(err)
		}
		if settled, err := tx.Commit(ctx); err != nil || settled.Kind() != transfer.FilePublished {
			t.Fatalf("commit = (%v, %v)", settled.Kind(), err)
		}
		got, err := os.ReadFile(finalPath)
		if err != nil || len(got) != totalSize || !bytes.Equal(got, expected) {
			t.Fatalf("published oversized file: err=%v len=%d want=%d", err, len(got), totalSize)
		}
	})

	t.Run("zero-byte file with non-empty staged part", func(t *testing.T) {
		ctx := context.Background()
		const seed byte = 0x59
		root, sel, _, partPath, finalPath := setupPausedPartialFile(t, seed, "empty.bin", 0, nil, true)
		if err := os.WriteFile(partPath, []byte("unexpected-tail"), 0o600); err != nil {
			t.Fatal(err)
		}

		reopen, err := New(Config{RootPath: root})
		if err != nil {
			t.Fatal(err)
		}
		defer reopen.Close()
		if err := reopen.BindDestination(ctx); err != nil {
			t.Fatal(err)
		}
		lookup, err := reopen.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
		if err != nil || lookup.Kind() != LookupReopened {
			t.Fatalf("reopen = (%v, %v)", lookup.Kind(), err)
		}
		reIntent, _ := lookup.Operation().ReceiveIntent()
		session, err := reopen.OpenOperation(ctx, lookup.Operation())
		if err != nil {
			t.Fatal(err)
		}
		rootAdm := testAdmitRoot(t, session, reIntent, seed+1)
		file := testMaterializationFile(t, session, reIntent, seed+2, "empty.bin", 0, rootAdm)

		start, err := session.BeginFile(ctx, file)
		if err != nil {
			t.Fatal(err)
		}
		tx, _, ok := start.Transaction()
		if !ok {
			t.Fatal("expected clean transaction for zero-byte file")
		}
		if settled, err := tx.Commit(ctx); err != nil || settled.Kind() != transfer.FilePublished {
			t.Fatalf("commit zero-byte file = (%v, %v)", settled.Kind(), err)
		}
		got, err := os.ReadFile(finalPath)
		if err != nil || len(got) != 0 {
			t.Fatalf("zero-byte file published with len=%d err=%v", len(got), err)
		}
	})
}

func TestResumePreservesCheckpointAfterUncheckpointedWritesAndNonContiguousHoles(t *testing.T) {
	t.Run("contiguous prefix survives later uncheckpointed writes within exact size", func(t *testing.T) {
		ctx := context.Background()
		const (
			seed       byte = 0x61
			totalSize       = 32 * 1024
			prefixSize      = 12 * 1024
		)
		expected := makePatternPayload(totalSize, seed)
		root, sel, _, partPath, finalPath := setupPausedPartialFile(
			t, seed, "crash-grow.bin", totalSize,
			[]struct {
				offset uint64
				data   []byte
			}{{offset: 0, data: expected[:prefixSize]}},
			false,
		)
		// Simulate an in-place write after the last checkpoint before a hard crash.
		f, err := os.OpenFile(partPath, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt(makePatternPayload(8*1024, 0xee), prefixSize); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}

		reopen, err := New(Config{RootPath: root})
		if err != nil {
			t.Fatal(err)
		}
		defer reopen.Close()
		if err := reopen.BindDestination(ctx); err != nil {
			t.Fatal(err)
		}
		lookup, err := reopen.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
		if err != nil || lookup.Kind() != LookupReopened {
			t.Fatalf("reopen = (%v, %v)", lookup.Kind(), err)
		}
		reIntent, _ := lookup.Operation().ReceiveIntent()
		session, err := reopen.OpenOperation(ctx, lookup.Operation())
		if err != nil {
			t.Fatal(err)
		}
		rootAdm := testAdmitRoot(t, session, reIntent, seed+1)
		file := testMaterializationFile(t, session, reIntent, seed+2, "crash-grow.bin", totalSize, rootAdm)

		start, err := session.BeginFile(ctx, file)
		if err != nil {
			t.Fatal(err)
		}
		tx, durable, ok := start.Transaction()
		if !ok || rangeTotalBytes(durable.Ranges().Ranges()) != prefixSize {
			t.Fatalf("expected %d reused bytes after post-checkpoint growth, got ok=%v ranges=%v", prefixSize, ok, durable.Ranges().Ranges())
		}
		if err := tx.WriteRange(ctx, prefixSize, expected[prefixSize:]); err != nil {
			t.Fatal(err)
		}
		if settled, err := tx.Commit(ctx); err != nil || settled.Kind() != transfer.FilePublished {
			t.Fatalf("commit = (%v, %v)", settled.Kind(), err)
		}
		got, err := os.ReadFile(finalPath)
		if err != nil || !bytes.Equal(got, expected) {
			t.Fatalf("published content mismatch: err=%v len=%d", err, len(got))
		}
	})

	t.Run("non-contiguous checkpoint survives later uncheckpointed hole fill", func(t *testing.T) {
		ctx := context.Background()
		const (
			seed      byte = 0x69
			totalSize      = 24 * 1024
		)
		expected := makePatternPayload(totalSize, seed)
		root, sel, _, partPath, finalPath := setupPausedPartialFile(
			t, seed, "sparse.bin", totalSize,
			[]struct {
				offset uint64
				data   []byte
			}{
				{offset: 0, data: expected[:4096]},
				{offset: 16384, data: expected[16384:20480]},
			},
			false,
		)
		// Write into the unwritten hole between the two checkpointed ranges before crash.
		f, err := os.OpenFile(partPath, os.O_RDWR, 0)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := f.WriteAt(makePatternPayload(8192, 0xdd), 4096); err != nil {
			_ = f.Close()
			t.Fatal(err)
		}
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}

		reopen, err := New(Config{RootPath: root})
		if err != nil {
			t.Fatal(err)
		}
		defer reopen.Close()
		if err := reopen.BindDestination(ctx); err != nil {
			t.Fatal(err)
		}
		lookup, err := reopen.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
		if err != nil || lookup.Kind() != LookupReopened {
			t.Fatalf("reopen = (%v, %v)", lookup.Kind(), err)
		}
		reIntent, _ := lookup.Operation().ReceiveIntent()
		session, err := reopen.OpenOperation(ctx, lookup.Operation())
		if err != nil {
			t.Fatal(err)
		}
		rootAdm := testAdmitRoot(t, session, reIntent, seed+1)
		file := testMaterializationFile(t, session, reIntent, seed+2, "sparse.bin", totalSize, rootAdm)

		start, err := session.BeginFile(ctx, file)
		if err != nil {
			t.Fatal(err)
		}
		tx, durable, ok := start.Transaction()
		if !ok || rangeTotalBytes(durable.Ranges().Ranges()) != 8192 {
			t.Fatalf("expected 8192 reused bytes across hole fill, got ok=%v ranges=%v", ok, durable.Ranges().Ranges())
		}
		if err := tx.WriteRange(ctx, 4096, expected[4096:16384]); err != nil {
			t.Fatal(err)
		}
		if err := tx.WriteRange(ctx, 20480, expected[20480:]); err != nil {
			t.Fatal(err)
		}
		if settled, err := tx.Commit(ctx); err != nil || settled.Kind() != transfer.FilePublished {
			t.Fatalf("commit = (%v, %v)", settled.Kind(), err)
		}
		got, err := os.ReadFile(finalPath)
		if err != nil || !bytes.Equal(got, expected) {
			t.Fatalf("published content mismatch: err=%v len=%d", err, len(got))
		}
	})
}

func TestCommitRejectsExternallyResizedStageWithoutPublishing(t *testing.T) {
	for _, tc := range []struct {
		name   string
		seed   byte
		mutate func(t *testing.T, partPath string, exactSize int64)
	}{
		{
			name: "extended past exact size",
			seed: 0x71,
			mutate: func(t *testing.T, partPath string, exactSize int64) {
				t.Helper()
				f, err := os.OpenFile(partPath, os.O_RDWR, 0)
				if err != nil {
					t.Fatal(err)
				}
				defer f.Close()
				if _, err := f.WriteAt([]byte("EXTRA-TAIL"), exactSize); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "truncated below exact size",
			seed: 0x75,
			mutate: func(t *testing.T, partPath string, exactSize int64) {
				t.Helper()
				if err := os.Truncate(partPath, exactSize/2); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			const totalSize = 16 * 1024
			expected := makePatternPayload(totalSize, tc.seed)
			root := t.TempDir()
			auth, err := New(Config{RootPath: root, CreateRoot: true})
			if err != nil {
				t.Fatal(err)
			}
			if err := auth.BindDestination(ctx); err != nil {
				t.Fatal(err)
			}
			sel := testSelectionSpec(t, tc.seed)
			lookup, err := auth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
			if err != nil || lookup.Kind() != LookupCreateReady {
				t.Fatalf("select = (%v, %v)", lookup.Kind(), err)
			}
			art, _ := receivecontract.NewResultRootDirectoryTree(receivecontract.NewSyntheticSelectionResultRoot())
			op, err := auth.CreateOperation(ctx, lookup, art)
			if err != nil {
				t.Fatal(err)
			}
			intent, _ := op.ReceiveIntent()
			session, err := auth.OpenOperation(ctx, op)
			if err != nil {
				t.Fatal(err)
			}
			rootAdm := testAdmitRoot(t, session, intent, tc.seed+1)
			file := testMaterializationFile(t, session, intent, tc.seed+2, "resized.bin", totalSize, rootAdm)
			start, err := session.BeginFile(ctx, file)
			if err != nil {
				t.Fatal(err)
			}
			tx, _, ok := start.Transaction()
			if !ok {
				t.Fatal("expected active file transaction")
			}
			if err := tx.WriteRange(ctx, 0, expected); err != nil {
				t.Fatal(err)
			}

			opDir := filepath.Join(root, ControlDirName, hex.EncodeToString(intent.OperationID().Bytes()))
			partPath := filepath.Join(opDir, partFileName(file.Descriptor().FileID(), file.Target().Locator().Digest()))
			tc.mutate(t, partPath, totalSize)

			if _, err := tx.Commit(ctx); err == nil {
				t.Fatal("expected Commit to reject externally resized staging file")
			} else if diag, ok := DiagnosticFor(err); !ok || diag.Stage != FailureDurability {
				t.Fatalf("Commit diagnostic = (%+v, %v), want FailureDurability", diag, ok)
			}

			res, _ := intent.MaterializationPlan().DestinationReservation()
			finalPath := filepath.Join(root, res.PhysicalName(), "resized.bin")
			if _, err := os.Stat(finalPath); !errors.Is(err, fs.ErrNotExist) {
				t.Fatalf("corrupted stage was published: %v", err)
			}
			if _, err := session.PauseTree(ctx); err != nil {
				t.Fatalf("PauseTree after rejected Commit failed: %v", err)
			}
			if err := auth.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestCommitCollisionPreservesStageFingerprintForZeroRedownloadRetry(t *testing.T) {
	ctx := context.Background()
	const (
		seed      byte = 0x81
		totalSize      = 16 * 1024
	)
	expected := makePatternPayload(totalSize, seed)
	root := t.TempDir()
	auth, err := New(Config{RootPath: root, CreateRoot: true})
	if err != nil {
		t.Fatal(err)
	}
	if err := auth.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	sel := testSelectionSpec(t, seed)
	lookup, err := auth.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != LookupCreateReady {
		t.Fatalf("select = (%v, %v)", lookup.Kind(), err)
	}
	art, _ := receivecontract.NewResultRootDirectoryTree(receivecontract.NewSyntheticSelectionResultRoot())
	op, err := auth.CreateOperation(ctx, lookup, art)
	if err != nil {
		t.Fatal(err)
	}
	intent, _ := op.ReceiveIntent()
	session, err := auth.OpenOperation(ctx, op)
	if err != nil {
		t.Fatal(err)
	}
	rootAdm := testAdmitRoot(t, session, intent, seed+1)
	file := testMaterializationFile(t, session, intent, seed+2, "collide.bin", totalSize, rootAdm)
	start, err := session.BeginFile(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	tx, _, _ := start.Transaction()
	if err := tx.WriteRange(ctx, 0, expected); err != nil {
		t.Fatal(err)
	}
	res, _ := intent.MaterializationPlan().DestinationReservation()
	finalPath := filepath.Join(root, res.PhysicalName(), "collide.bin")
	if err := os.WriteFile(finalPath, []byte("occupy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if settled, err := tx.Commit(ctx); err != nil || settled.Kind() != transfer.FileCollision {
		t.Fatalf("commit on collision = (%v, %v), want FileCollision", settled.Kind(), err)
	}
	if _, err := session.PauseTree(ctx); err != nil {
		t.Fatal(err)
	}
	if err := auth.Close(); err != nil {
		t.Fatal(err)
	}

	// Clear the colliding destination file and verify retry reuses 100% of the stage.
	if err := os.Remove(finalPath); err != nil {
		t.Fatal(err)
	}
	reopen, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer reopen.Close()
	if err := reopen.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	reLookup, err := reopen.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || reLookup.Kind() != LookupReopened {
		t.Fatalf("reopen = (%v, %v)", reLookup.Kind(), err)
	}
	reIntent, _ := reLookup.Operation().ReceiveIntent()
	reSession, err := reopen.OpenOperation(ctx, reLookup.Operation())
	if err != nil {
		t.Fatal(err)
	}
	reRoot := testAdmitRoot(t, reSession, reIntent, seed+1)
	reFile := testMaterializationFile(t, reSession, reIntent, seed+2, "collide.bin", totalSize, reRoot)

	reStart, err := reSession.BeginFile(ctx, reFile)
	if err != nil {
		t.Fatal(err)
	}
	reTx, durable, ok := reStart.Transaction()
	if !ok || rangeTotalBytes(durable.Ranges().Ranges()) != totalSize {
		t.Fatalf("retry after collision did not reuse full stage: ok=%v ranges=%v", ok, durable.Ranges().Ranges())
	}
	if settled, err := reTx.Commit(ctx); err != nil || settled.Kind() != transfer.FilePublished {
		t.Fatalf("retry commit = (%v, %v)", settled.Kind(), err)
	}
	got, err := os.ReadFile(finalPath)
	if err != nil || !bytes.Equal(got, expected) {
		t.Fatalf("published content mismatch: err=%v len=%d", err, len(got))
	}
}

func TestInvalidStagePreflightsDestinationCollision(t *testing.T) {
	ctx := context.Background()
	const (
		seed      byte = 0x89
		totalSize      = 16 * 1024
	)
	expected := makePatternPayload(totalSize, seed)
	root, sel, _, partPath, finalPath := setupPausedPartialFile(
		t, seed, "occupied.bin", totalSize,
		[]struct {
			offset uint64
			data   []byte
		}{{offset: 0, data: expected[:8192]}},
		false,
	)
	if err := os.WriteFile(partPath, makePatternPayload(8192, 0xee), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finalPath, []byte("keep-user-file"), 0o600); err != nil {
		t.Fatal(err)
	}

	reopen, err := New(Config{RootPath: root})
	if err != nil {
		t.Fatal(err)
	}
	defer reopen.Close()
	if err := reopen.BindDestination(ctx); err != nil {
		t.Fatal(err)
	}
	lookup, err := reopen.SelectOperation(ctx, sel, receivecontract.AutomaticOperation())
	if err != nil || lookup.Kind() != LookupReopened {
		t.Fatalf("reopen = (%v, %v)", lookup.Kind(), err)
	}
	reIntent, _ := lookup.Operation().ReceiveIntent()
	session, err := reopen.OpenOperation(ctx, lookup.Operation())
	if err != nil {
		t.Fatal(err)
	}
	rootAdm := testAdmitRoot(t, session, reIntent, seed+1)
	file := testMaterializationFile(t, session, reIntent, seed+2, "occupied.bin", totalSize, rootAdm)

	start, err := session.BeginFile(ctx, file)
	if err != nil {
		t.Fatal(err)
	}
	settled, immediate := start.ImmediateSettlement()
	if !immediate || settled.Kind() != transfer.FileCollision {
		t.Fatalf("invalid stage with occupied destination = (immediate=%v, kind=%v), want FileCollision", immediate, settled.Kind())
	}
	if got, err := os.ReadFile(finalPath); err != nil || string(got) != "keep-user-file" {
		t.Fatalf("foreign destination file altered: %q %v", got, err)
	}
}
