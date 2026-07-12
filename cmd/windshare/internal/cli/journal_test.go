package cli

import (
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"io"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/layout"
	"github.com/windshare/windshare/core/manifest"
	"github.com/windshare/windshare/core/session"
	"github.com/windshare/windshare/core/share"
)

type journalSource map[string][]byte

func manifestFingerprint(sealed []byte) (manifest.Fingerprint, error) {
	return manifest.SealedFingerprint(sealed)
}

func (s journalSource) ReadRange(path string, off, n int64) ([]byte, error) {
	data := s[path]
	if off < 0 || n < 0 || off+n > int64(len(data)) {
		return nil, io.ErrUnexpectedEOF
	}
	return bytes.Clone(data[off : off+n]), nil
}

type journalFileSink struct{}

func (journalFileSink) EnsureDir(string) error                 { return nil }
func (journalFileSink) WriteRange(string, int64, []byte) error { return nil }
func (journalFileSink) SetMTime(string, int64) error           { return nil }

func newJournalPlan(t *testing.T, selectors []string) (*share.TransferPlan, manifest.Fingerprint) {
	t.Helper()
	files := []share.FileMeta{
		{Path: "a.txt", Size: 4, MTime: 1},
		{Path: "b.txt", Size: 4, MTime: 1},
	}
	sharer, err := share.NewSharer(files, journalSource{"a.txt": []byte("aaaa"), "b.txt": []byte("bbbb")}, share.Options{ChunkSize: layout.MinChunkSize})
	if err != nil {
		t.Fatalf("NewSharer: %v", err)
	}
	sealed, err := sharer.SealedManifest()
	if err != nil {
		t.Fatalf("SealedManifest: %v", err)
	}
	receiver, err := share.NewReceiver(sharer.Link(), sealed, journalFileSink{})
	if err != nil {
		t.Fatalf("NewReceiver: %v", err)
	}
	plan, err := receiver.Plan(selectors)
	if err != nil {
		t.Fatalf("Plan: %v", err)
	}
	fingerprint, err := manifest.SealedFingerprint(sealed)
	if err != nil {
		t.Fatalf("manifestFingerprint: %v", err)
	}
	return plan, fingerprint
}

func TestJournalV2RoundTrip(t *testing.T) {
	plan, fingerprint := newJournalPlan(t, []string{"a.txt"})
	have := session.NewBitfield(plan.Sink().Have().Len())
	have.Set(0)
	state := journalState{
		fingerprint: fingerprint,
		planID:      plan.PlanID(),
		have:        have,
		owned:       []string{"a.txt"},
	}
	path := journalPath(t.TempDir(), fingerprint)
	if !strings.HasPrefix(filepath.Base(path), journalNamePrefix) {
		t.Fatalf("journal path %q lacks reserved prefix", path)
	}
	if err := writeJournal(path, state); err != nil {
		t.Fatalf("writeJournal: %v", err)
	}
	got, err := readJournal(path)
	if err != nil {
		t.Fatalf("readJournal: %v", err)
	}
	if got.fingerprint != state.fingerprint || got.planID != state.planID {
		t.Fatalf("identity mismatch: got fingerprint=%x plan=%s", got.fingerprint, got.planID)
	}
	if got.have.Len() != have.Len() || got.have.Count() != have.Count() || !got.have.Get(0) {
		t.Fatalf("have-state mismatch: len=%d count=%d", got.have.Len(), got.have.Count())
	}
	if !slices.Equal(got.owned, state.owned) {
		t.Fatalf("owned paths = %v, want %v", got.owned, state.owned)
	}
	if _, err := os.Stat(path + ".tmp"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("temporary journal remains: %v", err)
	}
}

func TestJournalRejectsOldAndMalformedFormats(t *testing.T) {
	plan, fingerprint := newJournalPlan(t, nil)
	state := journalState{
		fingerprint: fingerprint,
		planID:      plan.PlanID(),
		have:        session.NewBitfield(plan.Sink().Have().Len()),
		owned:       []string{"a.txt"},
	}
	encoded, err := encodeJournal(state)
	if err != nil {
		t.Fatal(err)
	}
	oldVersion := bytes.Clone(encoded)
	oldVersion[len(journalMagic)] = 1
	oldChecksum := sha256.Sum256(oldVersion[:len(oldVersion)-journalChecksumBytes])
	copy(oldVersion[len(oldVersion)-journalChecksumBytes:], oldChecksum[:])
	trailing := append(bytes.Clone(encoded[:len(encoded)-journalChecksumBytes]), 0)
	trailingChecksum := sha256.Sum256(trailing)
	trailing = append(trailing, trailingChecksum[:]...)
	tampered := bytes.Clone(encoded)
	tampered[len(tampered)-journalChecksumBytes-1] ^= 0x01 // remains valid UTF-8/path syntax without an integrity check
	oversizedHave := bytes.Clone(encoded)
	bitfieldOffset := len(journalMagic) + 1 + manifest.FingerprintBytes + share.PlanIDBytes + 8
	binary.LittleEndian.PutUint64(oversizedHave[bitfieldOffset:], layout.MaxChunkCount+1)
	oversizedHaveChecksum := sha256.Sum256(oversizedHave[:len(oversizedHave)-journalChecksumBytes])
	copy(oversizedHave[len(oversizedHave)-journalChecksumBytes:], oversizedHaveChecksum[:])
	badOwnedOrder := journalState{
		fingerprint: fingerprint,
		planID:      plan.PlanID(),
		have:        state.have,
		owned:       []string{"b.txt", "a.txt"},
	}
	if _, err := encodeJournal(badOwnedOrder); err == nil {
		t.Fatal("encoder accepted non-canonical owned path order")
	}

	cases := map[string][]byte{
		"empty":                {},
		"old version":          oldVersion,
		"truncated":            encoded[:len(encoded)-1],
		"trailing":             trailing,
		"integrity mismatch":   tampered,
		"oversized have state": oversizedHave,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "journal")
			if err := os.WriteFile(path, data, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readJournal(path); !errors.Is(err, errJournalCorrupt) {
				t.Fatalf("readJournal = %v, want errJournalCorrupt", err)
			}
		})
	}
}

func TestJournalRejectsHostileOwnedCountWithoutEagerAllocation(t *testing.T) {
	const (
		hostileOwnedCount = 1 << 12
		// The malformed first path should dominate allocation. This leaves ample
		// runtime headroom while detecting a slice sized from the hostile count.
		maxDecodeAllocBytes = 8 << 10
	)
	encoded, err := encodeJournal(journalState{have: session.NewBitfield(0)})
	if err != nil {
		t.Fatalf("encodeJournal: %v", err)
	}
	payload := bytes.Clone(encoded[:len(encoded)-journalChecksumBytes])
	ownedCountOffset := len(payload) - 8 // an empty journal ends with its owned count
	binary.LittleEndian.PutUint64(payload[ownedCountOffset:], hostileOwnedCount)
	payload = append(payload, make([]byte, hostileOwnedCount*journalPathLengthBytes)...)
	checksum := sha256.Sum256(payload)
	hostile := append(payload, checksum[:]...)

	if _, err := decodeJournal(hostile); err == nil {
		t.Fatal("journal accepted zero-length hostile owned paths")
	}
	result := testing.Benchmark(func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if _, err := decodeJournal(hostile); err == nil {
				b.Fatal("journal accepted hostile owned paths")
			}
		}
	})
	if got := result.AllocedBytesPerOp(); got > maxDecodeAllocBytes {
		t.Fatalf("hostile owned count allocated %d bytes per decode, limit %d", got, maxDecodeAllocBytes)
	}
}

func TestJournalPathEncodedSizeIsOverflowSafe(t *testing.T) {
	if _, ok := journalPathEncodedSize(maxJournalBytes, math.MaxInt); ok {
		t.Fatal("maximum int path length fit within the bounded journal budget")
	}
	if got, ok := journalPathEncodedSize(journalPathLengthBytes, 0); !ok || got != journalPathLengthBytes {
		t.Fatalf("empty path encoding size = (%d, %v), want (%d, true)", got, ok, journalPathLengthBytes)
	}
}

func TestWriteJournalPreservesPredictableTempName(t *testing.T) {
	plan, fingerprint := newJournalPlan(t, nil)
	dir := t.TempDir()
	path := journalPath(dir, fingerprint)
	predictableTemp := path + ".tmp"
	sentinel := []byte("pre-existing user data")
	if err := os.WriteFile(predictableTemp, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := writeJournal(path, journalState{
		fingerprint: fingerprint,
		planID:      plan.PlanID(),
		have:        session.NewBitfield(plan.Sink().Have().Len()),
	}); err != nil {
		t.Fatalf("writeJournal: %v", err)
	}
	got, err := os.ReadFile(predictableTemp)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, sentinel) {
		t.Fatalf("journal update overwrote predictable temp path: got %q", got)
	}
}

func TestResumeJournalBindsManifestPlanHaveAndOwnership(t *testing.T) {
	plan, fingerprint := newJournalPlan(t, []string{"a.txt"})
	path := journalPath(t.TempDir(), fingerprint)
	journal, err := loadResume(path, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Bind(plan); err != nil {
		t.Fatal(err)
	}
	if err := journal.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordCreated("a.txt"); err != nil {
		t.Fatal(err)
	}
	plan.Sink().Have().Set(0)
	if err := journal.Checkpoint(); err != nil {
		t.Fatal(err)
	}

	resumedPlan, _ := newJournalPlan(t, []string{"a.txt"})
	loaded, err := loadResume(path, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if !loaded.Resuming() {
		t.Fatal("existing v2 journal was not recognized as resume state")
	}
	if err := loaded.Bind(resumedPlan); err != nil {
		t.Fatalf("Bind resumed plan: %v", err)
	}
	if !loaded.Owns("a.txt") || loaded.Owns("b.txt") {
		t.Fatalf("ownership mismatch: a=%v b=%v", loaded.Owns("a.txt"), loaded.Owns("b.txt"))
	}
	if !resumedPlan.Sink().Have().Get(0) {
		t.Fatal("have-state was not restored into plan sink")
	}
}

func TestResumeJournalRejectsPlanChangeAndFingerprintCollision(t *testing.T) {
	plan, fingerprint := newJournalPlan(t, []string{"a.txt"})
	path := journalPath(t.TempDir(), fingerprint)
	have := session.NewBitfield(plan.Sink().Have().Len())
	have.Set(0) // a.txt and b.txt share this boundary chunk, but have is plan-local.
	state := journalState{
		fingerprint: fingerprint,
		planID:      plan.PlanID(),
		have:        have,
		owned:       []string{"a.txt"},
	}
	if err := writeJournal(path, state); err != nil {
		t.Fatal(err)
	}

	changedPlan, _ := newJournalPlan(t, []string{"b.txt"})
	journal, err := loadResume(path, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Bind(changedPlan); !errors.Is(err, errJournalPlan) {
		t.Fatalf("Bind changed selection = %v, want errJournalPlan", err)
	}

	other := fingerprint
	other[len(other)-1] ^= 0xff
	if _, err := loadResume(path, other); !errors.Is(err, errJournalFingerprint) {
		t.Fatalf("loadResume fingerprint mismatch = %v", err)
	}
}

func TestResumeJournalDoesNotGrantOwnershipAfterPersistenceFailure(t *testing.T) {
	plan, fingerprint := newJournalPlan(t, []string{"a.txt"})
	path := filepath.Join(t.TempDir(), "missing", "journal")
	journal, err := loadResume(path, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Bind(plan); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordCreated("a.txt"); err == nil {
		t.Fatal("RecordCreated succeeded without a journal directory")
	}
	if journal.Owns("a.txt") {
		t.Fatal("failed persistence granted overwrite authority")
	}
}

func TestResumeJournalCannotExpandOwnershipBeyondBoundPlan(t *testing.T) {
	plan, fingerprint := newJournalPlan(t, []string{"a.txt"})
	path := journalPath(t.TempDir(), fingerprint)
	journal, err := loadResume(path, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Bind(plan); err != nil {
		t.Fatal(err)
	}
	if err := journal.RecordCreated("b.txt"); !errors.Is(err, errJournalPlan) {
		t.Fatalf("RecordCreated outside plan = %v, want errJournalPlan", err)
	}
	if journal.Owns("b.txt") {
		t.Fatal("path outside bound plan gained reopen authority")
	}
}

func TestResumeJournalRemovesOnlyEmptyTransactions(t *testing.T) {
	plan, fingerprint := newJournalPlan(t, []string{"a.txt"})
	dir := t.TempDir()
	path := journalPath(dir, fingerprint)
	journal, err := loadResume(path, fingerprint)
	if err != nil {
		t.Fatal(err)
	}
	if err := journal.Bind(plan); err != nil {
		t.Fatal(err)
	}
	if err := journal.Checkpoint(); err != nil {
		t.Fatal(err)
	}
	if err := journal.RemoveIfUnowned(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("empty journal still exists: %v", err)
	}

	if err := journal.RecordCreated("a.txt"); err != nil {
		t.Fatal(err)
	}
	if err := journal.RemoveIfUnowned(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("owned journal was removed: %v", err)
	}
}

func TestManifestFingerprintRejectsShortEnvelope(t *testing.T) {
	if _, err := manifest.SealedFingerprint(make([]byte, manifest.FingerprintBytes-1)); err == nil {
		t.Fatal("short sealed manifest produced a fingerprint")
	}
}
