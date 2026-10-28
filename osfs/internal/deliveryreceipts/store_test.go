package deliveryreceipts

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"os"
	"testing"

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/osfs/internal/fileexecution"
)

type memoryScratch struct {
	data                        []byte
	closed                      bool
	writeErr, readErr, closeErr error
	shortWrite                  bool
}

func (file *memoryScratch) Write(data []byte) (int, error) {
	if file.writeErr != nil {
		return 0, file.writeErr
	}
	if file.shortWrite {
		return 0, nil
	}
	file.data = append(file.data, data...)
	return len(data), nil
}
func (file *memoryScratch) ReadAt(data []byte, offset int64) (int, error) {
	if file.readErr != nil {
		return 0, file.readErr
	}
	if file.closed {
		return 0, os.ErrClosed
	}
	return bytes.NewReader(file.data).ReadAt(data, offset)
}
func (file *memoryScratch) Close() error {
	file.closed = true
	return file.closeErr
}

func receiptEntry(index int) entry {
	return entry{
		key: transfer.MaterializationLocatorDigest(sha256.Sum256(fmt.Appendf(nil, "path-%d", index))),
		receipt: fileexecution.DeliveryReceipt{
			DescriptorDigest: sha256.Sum256(fmt.Appendf(nil, "descriptor-%d", index)),
			Object:           transfer.OwnedObjectID(sha256.Sum256(fmt.Appendf(nil, "object-%d", index))),
		},
	}
}

func TestReceiptsSpillAndMergeWithoutLosingHistoryOrGrowingMemory(t *testing.T) {
	store := New()
	var files []*memoryScratch
	store.newFile = func() (scratchFile, error) {
		file := &memoryScratch{}
		files = append(files, file)
		return file, nil
	}
	const count = maximumMemoryReceipts*9 + 1
	for index := 0; index < count; index++ {
		value := receiptEntry(index)
		if err := store.Record(value.key, value.receipt); err != nil {
			t.Fatal(err)
		}
		if len(store.memory) > maximumMemoryReceipts || len(store.runs) > bits.Len(uint(index/maximumMemoryReceipts)) {
			t.Fatal("receipt history escaped its memory budget")
		}
		if index < maximumMemoryReceipts && len(files) != 0 {
			t.Fatal("small operation allocated scratch storage")
		}
	}
	for index := 0; index < count; index++ {
		value := receiptEntry(index)
		receipt, found, err := store.Lookup(value.key)
		if err != nil || !found || receipt != value.receipt {
			t.Fatalf("receipt %d was lost during merge: %t %v", index, found, err)
		}
	}
	missing := receiptEntry(count)
	if _, found, err := store.Lookup(missing.key); err != nil || found {
		t.Fatalf("missing delivery became completed: %t %v", found, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	for _, file := range files {
		if !file.closed {
			t.Fatal("scratch handle survived store closure or merge")
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.Lookup(missing.key); !errors.Is(err, errClosed) {
		t.Fatalf("closed history remained readable: %v", err)
	}
}

func TestReceiptFlushFailuresPreserveAcknowledgedHistory(t *testing.T) {
	for _, failureKind := range []string{"create", "short-write", "write", "merge-create", "merge-read"} {
		t.Run(failureKind, func(t *testing.T) {
			store := New()
			t.Cleanup(func() { _ = store.Close() })
			var files []*memoryScratch
			failure := errors.New("scratch unavailable")
			inject := false
			creates := 0
			store.newFile = func() (scratchFile, error) {
				creates++
				if inject && (failureKind == "create" || failureKind == "merge-create" && creates == 3) {
					return nil, failure
				}
				file := &memoryScratch{}
				if inject && failureKind == "write" {
					file.writeErr = failure
				}
				if inject && failureKind == "short-write" {
					file.shortWrite = true
				}
				files = append(files, file)
				return file, nil
			}
			count := maximumMemoryReceipts
			if failureKind == "merge-create" || failureKind == "merge-read" {
				count *= 2
			}
			for index := 0; index < count; index++ {
				value := receiptEntry(index)
				if err := store.Record(value.key, value.receipt); err != nil {
					t.Fatal(err)
				}
			}
			inject = true
			if failureKind == "merge-read" {
				// Force a read failure only after candidate creation starts, so
				// this exercises rollback of merging acknowledged disk history.
				original := store.newFile
				store.newFile = func() (scratchFile, error) {
					file, err := original()
					files[0].readErr = failure
					return file, err
				}
			}
			next := receiptEntry(count)
			if err := store.Record(next.key, next.receipt); err == nil {
				t.Fatal("failed flush acknowledged new delivery")
			}
			inject = false
			for _, file := range files {
				file.readErr = nil
			}
			store.newFile = func() (scratchFile, error) {
				file := &memoryScratch{}
				files = append(files, file)
				return file, nil
			}
			for index := 0; index < count; index++ {
				value := receiptEntry(index)
				actual, found, err := store.Lookup(value.key)
				if err != nil || !found || actual != value.receipt {
					t.Fatalf("failure lost acknowledged receipt %d: %v", index, err)
				}
			}
			if _, found, err := store.Lookup(next.key); err != nil || found {
				t.Fatalf("failed receipt appeared committed: %t %v", found, err)
			}
			if err := store.Record(next.key, next.receipt); err != nil {
				t.Fatalf("recovered scratch could not retry: %v", err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			for _, file := range files {
				if !file.closed {
					t.Fatal("failed candidate leaked a handle")
				}
			}
		})
	}
}

func TestReceiptIdentityIsImmutableAndScratchIsProcessLocal(t *testing.T) {
	store := New()
	t.Cleanup(func() { _ = store.Close() })
	for index := 0; index <= maximumMemoryReceipts; index++ {
		value := receiptEntry(index)
		if err := store.Record(value.key, value.receipt); err != nil {
			t.Fatal(err)
		}
	}
	first := receiptEntry(0)
	if err := store.Record(first.key, first.receipt); err != nil {
		t.Fatal(err)
	}
	first.receipt.Object[0] ^= 1
	if err := store.Record(first.key, first.receipt); !errors.Is(err, errConflict) {
		t.Fatalf("conflicting receipt replaced historical identity: %v", err)
	}
	if err := store.Record(transfer.MaterializationLocatorDigest{}, first.receipt); !errors.Is(err, errInvalid) {
		t.Fatalf("zero receipt locator accepted: %v", err)
	}
	file, ok := store.runs[0].file.(*os.File)
	if !ok {
		t.Fatal("native scratch factory did not retain a file handle")
	}
	name := file.Name()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(name); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("live history left a recovery-looking artifact: %v", err)
	}
}

func TestTruncatedRunCannotSilentlyDropReceiptsDuringMerge(t *testing.T) {
	create := func() (scratchFile, error) { return &memoryScratch{}, nil }
	left, err := writeRun(create, []entry{receiptEntry(1)})
	if err != nil {
		t.Fatal(err)
	}
	right, err := writeRun(create, []entry{receiptEntry(2)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = left.file.Close(); _ = right.file.Close() })
	left.file.(*memoryScratch).data = nil
	if merged, err := mergeRuns(create, left, right); merged != nil || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated acknowledged run was silently compacted: %v", err)
	}
}

func TestReceiptCloseReportsScratchReleaseFailure(t *testing.T) {
	failure := errors.New("scratch close failed")
	store := New()
	store.newFile = func() (scratchFile, error) { return &memoryScratch{closeErr: failure}, nil }
	for index := 0; index <= maximumMemoryReceipts*2; index++ {
		value := receiptEntry(index)
		if err := store.Record(value.key, value.receipt); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.Close(); !errors.Is(err, failure) {
		t.Fatalf("scratch close failure disappeared: %v", err)
	}
}
