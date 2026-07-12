package share

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"fmt"
	"slices"
	"strings"

	"github.com/windshare/windshare/core/layout"
	"github.com/windshare/windshare/core/manifest"
	"github.com/windshare/windshare/core/session"
)

// PlanIDDomain separates selection fingerprints from every other SHA-256 use.
const PlanIDDomain = "windshare/v1 transfer-plan\x00"

// PlanIDBytes is fixed by SHA-256 and is reproducible in browser implementations.
const PlanIDBytes = sha256.Size

// PlanID identifies the canonical set of selected manifest entries. Its preimage is
// PlanIDDomain followed by each selected path in UTF-8 byte order as
// u64_be(byteLength) || pathBytes. Resume state binds this value in addition to the
// sealed-manifest fingerprint.
type PlanID [PlanIDBytes]byte

func (id PlanID) String() string { return hex.EncodeToString(id[:]) }

// TransferPlan compiles selection exactly once. Its selected entries, chunk intervals,
// selected byte count, and PlanID never change; only the private have-state advances as
// authenticated blocks are successfully materialized.
type TransferPlan struct {
	receiver      *Receiver
	selected      []manifest.Entry
	selectedPaths map[string]struct{}
	chunks        layout.ChunkSet
	selectedBytes int64
	id            PlanID
	sink          *TransferSink
}

// Plan compiles selectors into one immutable transfer contract. nil selects the entire
// manifest; a non-nil empty slice selects nothing. Exact files never imply a subtree,
// while explicit and implicit directories do.
func (r *Receiver) Plan(selectors []string) (*TransferPlan, error) {
	var selectedIndices []int
	var chunks layout.ChunkSet
	if selectors == nil {
		selectedIndices = make([]int, len(r.m.Entries))
		for i := range selectedIndices {
			selectedIndices[i] = i
		}
		chunks = layout.FullChunkSet(r.lay.NumChunks())
	} else {
		canonicalSelectors := make([]string, len(selectors))
		for i, selector := range selectors {
			canonical, err := manifest.CanonicalPath(selector)
			if err != nil {
				return nil, err
			}
			canonicalSelectors[i] = canonical
		}
		selection, err := r.lay.Select(canonicalSelectors)
		if err != nil {
			return nil, err
		}
		selectedIndices = selection.EntryIndices()
		chunks = selection.Chunks()
	}

	selected := make([]manifest.Entry, 0, len(selectedIndices))
	selectedPaths := make(map[string]struct{}, len(selectedIndices))
	var selectedBytes int64
	for _, index := range selectedIndices {
		entry := r.m.Entries[index]
		selected = append(selected, entry)
		selectedPaths[entry.Path] = struct{}{}
		if !entry.IsDir {
			selectedBytes += entry.Size
		}
	}

	plan := &TransferPlan{
		receiver:      r,
		selected:      selected,
		selectedPaths: selectedPaths,
		chunks:        chunks,
		selectedBytes: selectedBytes,
		id:            computePlanID(selected),
	}
	// Geometry was bounded before this dense allocation; MaxChunkStateBytes is the
	// protocol-level memory ceiling for this state.
	plan.sink = &TransferSink{plan: plan, have: session.NewBitfield(r.lay.NumChunks())}
	return plan, nil
}

func computePlanID(entries []manifest.Entry) PlanID {
	paths := make([]string, len(entries))
	for i, entry := range entries {
		paths[i] = entry.Path
	}
	slices.Sort(paths)

	hash := sha256.New()
	_, _ = hash.Write([]byte(PlanIDDomain))
	var size [8]byte
	for _, path := range paths {
		binary.BigEndian.PutUint64(size[:], uint64(len(path)))
		_, _ = hash.Write(size[:])
		_, _ = hash.Write([]byte(path))
	}
	var id PlanID
	copy(id[:], hash.Sum(nil))
	return id
}

func (p *TransferPlan) PlanID() PlanID                    { return p.id }
func (p *TransferPlan) SelectedBytes() int64              { return p.selectedBytes }
func (p *TransferPlan) SelectedEntries() []manifest.Entry { return slices.Clone(p.selected) }
func (p *TransferPlan) Chunks() layout.ChunkSet           { return p.chunks }
func (p *TransferPlan) Sink() *TransferSink               { return p.sink }

// SelectedBytesInChunk reports the bytes this plan materializes from a chunk.
// Boundary overfetch is authenticated but excluded, so transport progress can
// describe the user's selection rather than opaque packed-stream traffic.
func (p *TransferPlan) SelectedBytesInChunk(index uint64) (int64, error) {
	if !p.chunks.Contains(index) {
		return 0, fmt.Errorf("%w: %d", ErrChunkNotSelected, index)
	}
	ranges, err := p.receiver.lay.ChunkToRanges(index)
	if err != nil {
		return 0, err
	}
	var selectedBytes int64
	for _, fileRange := range ranges {
		if _, selected := p.selectedPaths[fileRange.Path]; selected {
			selectedBytes += fileRange.N
		}
	}
	return selectedBytes, nil
}

// Accept is the direct encrypted-block path used by core consumers that do not need a
// session. It enforces plan membership before spending AEAD or touching the sink.
func (p *TransferPlan) Accept(index uint64, blockCT []byte) error {
	if index >= p.receiver.lay.NumChunks() {
		return fmt.Errorf("%w: %d", layout.ErrChunkOutOfRange, index)
	}
	if !p.chunks.Contains(index) {
		return fmt.Errorf("%w: %d", ErrChunkNotSelected, index)
	}
	plaintext, err := p.receiver.codec.Open(index, blockCT)
	if err != nil {
		return err
	}
	return p.sink.WriteBlock(index, plaintext)
}

// Finalize materializes only selected empty entries and restores metadata after every
// selected chunk is durable. Directory mtimes are restored deepest-first.
func (p *TransferPlan) Finalize() error {
	var missing uint64
	for index := range p.chunks.Iter() {
		if !p.sink.have.Get(index) {
			missing++
		}
	}
	if missing != 0 {
		return fmt.Errorf("%w: %d blocks remain", ErrMissingBlocks, missing)
	}

	for _, entry := range p.selected {
		switch {
		case entry.IsDir:
			if err := p.receiver.dst.EnsureDir(entry.Path); err != nil {
				return wrapPathOperation("materialize directory", entry.Path, err)
			}
		case entry.Size == 0:
			if err := p.receiver.dst.WriteRange(entry.Path, 0, nil); err != nil {
				return wrapPathOperation("materialize empty file", entry.Path, err)
			}
		}
	}
	for _, entry := range p.selected {
		if !entry.IsDir {
			if err := p.receiver.dst.SetMTime(entry.Path, entry.MTime); err != nil {
				return wrapPathOperation("restore file mtime for", entry.Path, err)
			}
		}
	}

	directories := make([]manifest.Entry, 0, len(p.selected))
	for _, entry := range p.selected {
		if entry.IsDir {
			directories = append(directories, entry)
		}
	}
	slices.SortStableFunc(directories, func(a, b manifest.Entry) int {
		return strings.Count(b.Path, "/") - strings.Count(a.Path, "/")
	})
	for _, entry := range directories {
		if err := p.receiver.dst.SetMTime(entry.Path, entry.MTime); err != nil {
			return wrapPathOperation("restore directory mtime for", entry.Path, err)
		}
	}
	return nil
}

// TransferSink materializes authenticated plaintext according to one TransferPlan. It
// is returned as a concrete type while satisfying session.Sink at the consumption site.
type TransferSink struct {
	plan *TransferPlan
	have session.Bitfield
}

var _ session.Sink = (*TransferSink)(nil)

func (s *TransferSink) WriteBlock(index uint64, plaintext []byte) error {
	if index >= s.plan.receiver.lay.NumChunks() {
		return fmt.Errorf("%w: %d", layout.ErrChunkOutOfRange, index)
	}
	if !s.plan.chunks.Contains(index) {
		return fmt.Errorf("%w: %d", ErrChunkNotSelected, index)
	}
	ranges, err := s.plan.receiver.lay.ChunkToRanges(index)
	if err != nil {
		return err
	}
	var expected int64
	for _, fileRange := range ranges {
		expected += fileRange.N
	}
	if int64(len(plaintext)) != expected {
		return fmt.Errorf("%w: block %d plaintext is %d bytes, geometry requires %d", ErrBlockLength, index, len(plaintext), expected)
	}

	var cursor int64
	for _, fileRange := range ranges {
		next := cursor + fileRange.N
		if _, selected := s.plan.selectedPaths[fileRange.Path]; selected {
			if err := s.plan.receiver.dst.WriteRange(fileRange.Path, fileRange.Off, plaintext[cursor:next]); err != nil {
				return wrapPathOperation("write", fileRange.Path, err)
			}
		}
		// A boundary block contains bytes for both selected and skipped siblings. The
		// plaintext cursor always follows packed-stream geometry, not write decisions.
		cursor = next
	}
	s.have.Set(index)
	return nil
}

func (s *TransferSink) Have() session.Bitfield { return s.have }

// TransferSink writes explicit file offsets, so forcing scheduler order would
// reduce parallelism without improving correctness.
func (s *TransferSink) DeliveryOrder() session.DeliveryOrder { return session.DeliveryAnyOrder }
