package catalog

import (
	"crypto/sha256"
	"crypto/subtle"
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"
)

const (
	MaxCatalogPageEntries     = 256
	MaxCatalogPageObjectBytes = 60 << 10
	MaxDirectoryEntries       = 1_048_576
	PageCommitmentBytes       = 32
	// These charges deliberately exceed the current Go struct headers so a
	// receiver budget bounds decoded catalog state, not merely compact wire bytes.
	CatalogPageMemoryOverhead       = 192
	CatalogEntryMemoryOverhead      = 128
	CatalogNameMemoryOverhead       = 16
	DirectorySnapshotMemoryOverhead = 128
)

var (
	ErrPageLimit        = errors.New("catalog page exceeds its entry limit")
	ErrPageSequence     = errors.New("catalog page sequence is invalid")
	ErrSiblingCollision = errors.New("catalog siblings collide under portable name matching")
)

type PageCommitment [PageCommitmentBytes]byte

// SealedPageObject is the exact transport-neutral sender object committed for a
// page. Catalog backends persist it opaquely in the same transaction as the
// semantic page; crypto remains owned by the consumer-provided PageSealer.
type SealedPageObject struct {
	encoded    []byte
	commitment PageCommitment
}

var (
	ErrCommittedRootUnavailable = errors.New("catalog committed-root capability is unavailable")
	ErrCommittedRootMismatch    = errors.New("catalog committed-root capability does not match the descriptor")
)

// CommittedRoot is the local capability proving that the synthetic root,
// private NodeRecords, and their retained budget were atomically published.
// It deliberately has no public constructor: startup and registration code
// cannot manufacture readiness before CatalogStore commits it.
type CommittedRoot struct {
	state *committedRootState
}

type committedRootState struct {
	store       *CatalogStore
	share       ShareInstance
	directory   DirectoryID
	generation  DirectoryGeneration
	reservation *BudgetReservation
}

func (root CommittedRoot) IsZero() bool { return root.state == nil }

func (root CommittedRoot) binding() (ShareInstance, DirectoryID, DirectoryGeneration, error) {
	if root.state == nil || root.state.store == nil {
		return ShareInstance{}, DirectoryID{}, DirectoryGeneration{}, ErrCommittedRootUnavailable
	}
	state := root.state
	state.store.mu.Lock()
	live := !state.store.closed && state.store.committedRoot == state && state.reservation.active()
	state.store.mu.Unlock()
	if !live {
		return ShareInstance{}, DirectoryID{}, DirectoryGeneration{}, ErrCommittedRootUnavailable
	}
	return state.share, state.directory, state.generation, nil
}

// AuthorizeRegistration must be called by the v2 registration boundary
// immediately before it emits REGISTER. ShareDescriptor is transport-neutral,
// so the local commit capability remains separate and cannot be serialized or
// replayed as if it were sender-authenticated wire data.
func (root CommittedRoot) AuthorizeRegistration(descriptor ShareDescriptor) error {
	share, directory, _, err := root.binding()
	if err != nil {
		return err
	}
	if descriptor.ShareInstance() != share || descriptor.SyntheticRoot() != directory {
		return ErrCommittedRootMismatch
	}
	return nil
}

type SyntheticRootCommitSpec struct {
	ShareInstance ShareInstance
	SyntheticRoot DirectoryID
	Generation    DirectoryGeneration
	SelectedRoots []NodeRecord
}

// NewSyntheticRootCommit builds the only catalog generation required before
// registration. It consumes already-opened selected-root records and therefore
// cannot accidentally turn share readiness into descendant traversal.
func NewSyntheticRootCommit(spec SyntheticRootCommitSpec) (DirectoryCommit, error) {
	if spec.ShareInstance.IsZero() || spec.SyntheticRoot.IsZero() || spec.Generation.IsZero() {
		return DirectoryCommit{}, errors.New("synthetic root commit requires share, root, and generation identities")
	}
	if len(spec.SelectedRoots) == 0 || len(spec.SelectedRoots) > MaxRootSlots {
		return DirectoryCommit{}, fmt.Errorf("synthetic root has %d selected roots; required range is 1..%d", len(spec.SelectedRoots), MaxRootSlots)
	}
	selected := slices.Clone(spec.SelectedRoots)
	var nameBytes uint64
	for _, record := range selected {
		if !record.valid() || record.IsSyntheticRoot() || record.Parent() != spec.SyntheticRoot {
			return DirectoryCommit{}, errors.New("synthetic root commit contains an invalid selected-root record")
		}
		if reservedOutputRootName(record.Entry().Name()) {
			return DirectoryCommit{}, errors.New("synthetic root contains an output-reserved name")
		}
		nameBytes += uint64(len(record.Entry().Name()))
		if nameBytes > MaxSelectedRootNamesBytes {
			return DirectoryCommit{}, fmt.Errorf("synthetic root names exceed %d UTF-8 bytes", MaxSelectedRootNamesBytes)
		}
	}
	sort.Slice(selected, func(left, right int) bool {
		return selected[left].Entry().Name() < selected[right].Entry().Name()
	})
	entries := make([]Entry, len(selected))
	for index, record := range selected {
		entries[index] = record.Entry()
	}
	if err := validateEntryOrder(entries); err != nil {
		return DirectoryCommit{}, err
	}
	root, err := NewSyntheticRootNodeRecord(spec.SyntheticRoot)
	if err != nil {
		return DirectoryCommit{}, err
	}
	return DirectoryCommit{
		directory: root, generation: spec.Generation, children: newSliceNodeSource(selected), synthetic: true,
	}, nil
}

func NewSealedPageObject(encoded []byte) (SealedPageObject, error) {
	if len(encoded) == 0 || len(encoded) > MaxCatalogPageObjectBytes {
		return SealedPageObject{}, fmt.Errorf("catalog sealed page object has invalid length %d", len(encoded))
	}
	digest := sha256.Sum256(encoded)
	return SealedPageObject{
		encoded: append([]byte(nil), encoded...), commitment: PageCommitment(digest),
	}, nil
}

func (o SealedPageObject) Bytes() []byte                { return append([]byte(nil), o.encoded...) }
func (o SealedPageObject) Commitment() PageCommitment   { return o.commitment }
func (o SealedPageObject) IsZero() bool                 { return len(o.encoded) == 0 }
func (o SealedPageObject) EstimatedMemoryBytes() uint64 { return uint64(len(o.encoded)) }

func NewPageCommitment(raw []byte) (PageCommitment, error) {
	if len(raw) != PageCommitmentBytes {
		return PageCommitment{}, fmt.Errorf("catalog page commitment must be %d bytes", PageCommitmentBytes)
	}
	var commitment PageCommitment
	copy(commitment[:], raw)
	return commitment, nil
}

func (c PageCommitment) Bytes() []byte { return append([]byte(nil), c[:]...) }
func (c PageCommitment) IsZero() bool {
	var zero PageCommitment
	return subtle.ConstantTimeCompare(c[:], zero[:]) == 1
}

type PageCommitInput struct {
	ShareInstance ShareInstance
	DirectoryID   DirectoryID
	Generation    DirectoryGeneration
	PageIndex     uint32
	Previous      PageCommitment
	Entries       []Entry
	Terminal      bool
	OmittedCount  uint64
}

type PageCommitter interface {
	Commit(PageCommitInput) (PageCommitment, error)
}

type PageCommitterFunc func(PageCommitInput) (PageCommitment, error)

func (function PageCommitterFunc) Commit(input PageCommitInput) (PageCommitment, error) {
	if function == nil {
		return PageCommitment{}, errors.New("catalog page committer is nil")
	}
	return function(input)
}

// PageSealer is a consumer-side cryptographic boundary. Catalog owns atomic
// persistence and replay of the returned bytes, but never interprets them.
type PageSealer interface {
	Seal(PageCommitInput) (SealedPageObject, error)
}

type PageSealerFunc func(PageCommitInput) (SealedPageObject, error)

func (function PageSealerFunc) Seal(input PageCommitInput) (SealedPageObject, error) {
	return function(input)
}

type CatalogPageSpec struct {
	ShareInstance ShareInstance
	DirectoryID   DirectoryID
	Generation    DirectoryGeneration
	PageIndex     uint32
	Previous      PageCommitment
	Entries       []Entry
	Terminal      bool
	OmittedCount  uint64
}

type CatalogPage struct {
	shareInstance ShareInstance
	directoryID   DirectoryID
	generation    DirectoryGeneration
	pageIndex     uint32
	previous      PageCommitment
	entries       []Entry
	terminal      bool
	omittedCount  uint64
	commitment    PageCommitment
}

func NewCatalogPage(spec CatalogPageSpec, committer PageCommitter) (CatalogPage, error) {
	entries, input, err := prepareCatalogPage(spec)
	if err != nil {
		return CatalogPage{}, err
	}
	if committer == nil {
		return CatalogPage{}, errors.New("catalog page requires a semantic committer")
	}
	commitment, err := committer.Commit(input)
	if err != nil {
		return CatalogPage{}, fmt.Errorf("commit catalog page: %w", err)
	}
	return catalogPageWithCommitment(spec, entries, commitment)
}

func sealCatalogPage(spec CatalogPageSpec, sealer PageSealer) (CatalogPage, SealedPageObject, error) {
	entries, input, err := prepareCatalogPage(spec)
	if err != nil {
		return CatalogPage{}, SealedPageObject{}, err
	}
	if sealer == nil {
		return CatalogPage{}, SealedPageObject{}, errors.New("catalog page requires a page sealer")
	}
	object, err := sealer.Seal(input)
	if err != nil {
		return CatalogPage{}, SealedPageObject{}, fmt.Errorf("seal catalog page: %w", err)
	}
	if object.IsZero() || object.Commitment().IsZero() {
		return CatalogPage{}, SealedPageObject{}, errors.New("catalog page sealer returned an empty object")
	}
	page, err := catalogPageWithCommitment(spec, entries, object.Commitment())
	return page, object, err
}

func prepareCatalogPage(spec CatalogPageSpec) ([]Entry, PageCommitInput, error) {
	if spec.ShareInstance.IsZero() || spec.DirectoryID.IsZero() || spec.Generation.IsZero() {
		return nil, PageCommitInput{}, errors.New("catalog page requires share, directory, and generation identities")
	}
	if len(spec.Entries) > MaxCatalogPageEntries {
		return nil, PageCommitInput{}, fmt.Errorf("%w: got %d", ErrPageLimit, len(spec.Entries))
	}
	if len(spec.Entries) == 0 && (!spec.Terminal || spec.PageIndex != 0) {
		return nil, PageCommitInput{}, fmt.Errorf("%w: only an empty directory may have an empty page", ErrPageSequence)
	}
	if !spec.Terminal && spec.OmittedCount != 0 {
		return nil, PageCommitInput{}, fmt.Errorf("%w: omitted count is only valid on the terminal page", ErrPageSequence)
	}
	if spec.PageIndex == 0 && !spec.Previous.IsZero() {
		return nil, PageCommitInput{}, fmt.Errorf("%w: first page has a predecessor", ErrPageSequence)
	}
	if spec.PageIndex > 0 && spec.Previous.IsZero() {
		return nil, PageCommitInput{}, fmt.Errorf("%w: later page has no predecessor", ErrPageSequence)
	}
	entries := slices.Clone(spec.Entries)
	if err := validateEntryOrder(entries); err != nil {
		return nil, PageCommitInput{}, err
	}
	input := PageCommitInput{
		ShareInstance: spec.ShareInstance, DirectoryID: spec.DirectoryID, Generation: spec.Generation,
		PageIndex: spec.PageIndex, Previous: spec.Previous, Entries: slices.Clone(entries), Terminal: spec.Terminal,
		OmittedCount: spec.OmittedCount,
	}
	return entries, input, nil
}

func catalogPageWithCommitment(spec CatalogPageSpec, entries []Entry, commitment PageCommitment) (CatalogPage, error) {
	if commitment.IsZero() {
		return CatalogPage{}, errors.New("catalog page committer returned an empty commitment")
	}
	return CatalogPage{
		shareInstance: spec.ShareInstance, directoryID: spec.DirectoryID, generation: spec.Generation,
		pageIndex: spec.PageIndex, previous: spec.Previous, entries: entries,
		terminal: spec.Terminal, omittedCount: spec.OmittedCount, commitment: commitment,
	}, nil
}

func validateEntryOrder(entries []Entry) error {
	seenNames := make(map[string]struct{}, len(entries))
	seenNodes := make(map[NodeID]struct{}, len(entries))
	for index, entry := range entries {
		if !entry.valid() {
			return fmt.Errorf("%w: entry %d is invalid", ErrPageSequence, index)
		}
		collisionKey := siblingCollisionKey(entry.name)
		if _, exists := seenNames[collisionKey]; exists {
			return fmt.Errorf("%w: %q", ErrSiblingCollision, entry.name)
		}
		seenNames[collisionKey] = struct{}{}
		if _, exists := seenNodes[entry.nodeID]; exists {
			return fmt.Errorf("%w: duplicate node identity", ErrSiblingCollision)
		}
		seenNodes[entry.nodeID] = struct{}{}
		if index > 0 && strings.Compare(entries[index-1].name, entry.name) >= 0 {
			return fmt.Errorf("%w: entries are not in canonical byte order", ErrPageSequence)
		}
	}
	return nil
}

func (p CatalogPage) ShareInstance() ShareInstance    { return p.shareInstance }
func (p CatalogPage) DirectoryID() DirectoryID        { return p.directoryID }
func (p CatalogPage) Generation() DirectoryGeneration { return p.generation }
func (p CatalogPage) PageIndex() uint32               { return p.pageIndex }
func (p CatalogPage) Previous() PageCommitment        { return p.previous }
func (p CatalogPage) Entries() []Entry                { return slices.Clone(p.entries) }
func (p CatalogPage) EntryCount() int                 { return len(p.entries) }
func (p CatalogPage) Entry(index uint32) (Entry, bool) {
	if uint64(index) >= uint64(len(p.entries)) {
		return Entry{}, false
	}
	return p.entries[index], true
}
func (p CatalogPage) Terminal() bool             { return p.terminal }
func (p CatalogPage) OmittedCount() uint64       { return p.omittedCount }
func (p CatalogPage) Commitment() PageCommitment { return p.commitment }
func (p CatalogPage) EstimatedMemoryBytes() uint64 {
	total := uint64(CatalogPageMemoryOverhead)
	for _, entry := range p.entries {
		total += uint64(CatalogEntryMemoryOverhead + CatalogNameMemoryOverhead + len(entry.name))
	}
	return total
}

type DirectorySnapshot struct {
	shareInstance        ShareInstance
	directoryID          DirectoryID
	generation           DirectoryGeneration
	pages                []CatalogPage
	entryCount           uint64
	omittedCount         uint64
	estimatedMemoryBytes uint64
}

// CommittedDirectory is the small, durable identity of one immutable
// generation. Pages remain backend-addressed so directory width never dictates
// the amount of live Go memory needed to browse or replay it.
type CommittedDirectory struct {
	shareInstance      ShareInstance
	directoryID        DirectoryID
	generation         DirectoryGeneration
	pageCount          uint32
	entryCount         uint64
	omittedCount       uint64
	terminalCommitment PageCommitment
}

func (d CommittedDirectory) ShareInstance() ShareInstance        { return d.shareInstance }
func (d CommittedDirectory) DirectoryID() DirectoryID            { return d.directoryID }
func (d CommittedDirectory) Generation() DirectoryGeneration     { return d.generation }
func (d CommittedDirectory) PageCount() uint32                   { return d.pageCount }
func (d CommittedDirectory) EntryCount() uint64                  { return d.entryCount }
func (d CommittedDirectory) OmittedCount() uint64                { return d.omittedCount }
func (d CommittedDirectory) TerminalCommitment() PageCommitment  { return d.terminalCommitment }
func (d CommittedDirectory) IsZero() bool                        { return d.directoryID.IsZero() }
func (d CommittedDirectory) Equal(other CommittedDirectory) bool { return d == other }

type directorySequence struct {
	identitySet bool
	committed   CommittedDirectory
	nextPage    uint32
	previous    PageCommitment
	lastName    string
	terminal    bool
}

func (s *directorySequence) accept(page CatalogPage) error {
	if page.commitment.IsZero() {
		return fmt.Errorf("%w: page %d has no commitment", ErrPageSequence, page.pageIndex)
	}
	if s.terminal {
		return fmt.Errorf("%w: page follows the terminal page", ErrPageSequence)
	}
	if !s.identitySet {
		if page.pageIndex != 0 || page.shareInstance.IsZero() || page.directoryID.IsZero() || page.generation.IsZero() {
			return fmt.Errorf("%w: generation does not start at page zero", ErrPageSequence)
		}
		s.identitySet = true
		s.committed.shareInstance = page.shareInstance
		s.committed.directoryID = page.directoryID
		s.committed.generation = page.generation
	} else if page.shareInstance != s.committed.shareInstance || page.directoryID != s.committed.directoryID || page.generation != s.committed.generation {
		return fmt.Errorf("%w: page %d changes generation identity", ErrPageSequence, page.pageIndex)
	}
	if page.pageIndex != s.nextPage || page.previous != s.previous {
		return fmt.Errorf("%w: page %d has a gap or predecessor mismatch", ErrPageSequence, page.pageIndex)
	}
	for _, entry := range page.entries {
		if s.lastName != "" && strings.Compare(s.lastName, entry.name) >= 0 {
			return fmt.Errorf("%w: entries cross a page boundary out of order", ErrPageSequence)
		}
		s.lastName = entry.name
	}
	if uint64(len(page.entries)) > MaxDirectoryEntries-s.committed.entryCount {
		return fmt.Errorf("%w: directory has more than %d entries", ErrPageLimit, MaxDirectoryEntries)
	}
	s.committed.entryCount += uint64(len(page.entries))
	if page.terminal {
		if page.omittedCount > MaxDirectoryEntries-s.committed.entryCount {
			return fmt.Errorf("%w: directory entries plus omitted children exceed %d", ErrPageLimit, MaxDirectoryEntries)
		}
		s.committed.omittedCount = page.omittedCount
		s.committed.terminalCommitment = page.commitment
		s.terminal = true
	}
	s.nextPage++
	s.previous = page.commitment
	return nil
}

func (s *directorySequence) finish() (CommittedDirectory, error) {
	if !s.identitySet || !s.terminal {
		return CommittedDirectory{}, fmt.Errorf("%w: generation has no terminal page", ErrPageSequence)
	}
	s.committed.pageCount = s.nextPage
	return s.committed, nil
}

func NewDirectorySnapshot(pages []CatalogPage) (DirectorySnapshot, error) {
	if len(pages) == 0 {
		return DirectorySnapshot{}, fmt.Errorf("%w: snapshot has no pages", ErrPageSequence)
	}
	var sequence directorySequence
	seenNames := make(map[string]struct{})
	seenNodes := make(map[NodeID]struct{})
	estimatedMemoryBytes := uint64(DirectorySnapshotMemoryOverhead)
	for index, page := range pages {
		estimatedMemoryBytes += CatalogPageMemoryOverhead
		if page.terminal != (index == len(pages)-1) {
			return DirectorySnapshot{}, fmt.Errorf("%w: terminal marker at page %d", ErrPageSequence, index)
		}
		if err := sequence.accept(page); err != nil {
			return DirectorySnapshot{}, err
		}
		for _, entry := range page.entries {
			estimatedMemoryBytes += uint64(CatalogEntryMemoryOverhead + CatalogNameMemoryOverhead + len(entry.name))
			key := siblingCollisionKey(entry.name)
			if _, exists := seenNames[key]; exists {
				return DirectorySnapshot{}, fmt.Errorf("%w: %q", ErrSiblingCollision, entry.name)
			}
			seenNames[key] = struct{}{}
			if _, exists := seenNodes[entry.nodeID]; exists {
				return DirectorySnapshot{}, fmt.Errorf("%w: duplicate node identity", ErrSiblingCollision)
			}
			seenNodes[entry.nodeID] = struct{}{}
		}
	}
	committed, err := sequence.finish()
	if err != nil {
		return DirectorySnapshot{}, err
	}
	return DirectorySnapshot{
		shareInstance: committed.shareInstance, directoryID: committed.directoryID, generation: committed.generation,
		pages: slices.Clone(pages), entryCount: committed.entryCount, omittedCount: committed.omittedCount,
		estimatedMemoryBytes: estimatedMemoryBytes,
	}, nil
}

func (s DirectorySnapshot) ShareInstance() ShareInstance    { return s.shareInstance }
func (s DirectorySnapshot) DirectoryID() DirectoryID        { return s.directoryID }
func (s DirectorySnapshot) Generation() DirectoryGeneration { return s.generation }
func (s DirectorySnapshot) Pages() []CatalogPage            { return slices.Clone(s.pages) }
func (s DirectorySnapshot) PageCount() int                  { return len(s.pages) }
func (s DirectorySnapshot) EntryCount() uint64              { return s.entryCount }
func (s DirectorySnapshot) OmittedCount() uint64            { return s.omittedCount }
func (s DirectorySnapshot) EstimatedMemoryBytes() uint64    { return s.estimatedMemoryBytes }
func (s DirectorySnapshot) Page(index uint32) (CatalogPage, bool) {
	if uint64(index) >= uint64(len(s.pages)) {
		return CatalogPage{}, false
	}
	return s.pages[index], true
}
func (s DirectorySnapshot) TerminalCommitment() PageCommitment {
	if len(s.pages) == 0 {
		return PageCommitment{}
	}
	return s.pages[len(s.pages)-1].commitment
}

func (s DirectorySnapshot) Equal(other DirectorySnapshot) bool {
	if s.shareInstance != other.shareInstance || s.directoryID != other.directoryID || s.generation != other.generation ||
		s.entryCount != other.entryCount || s.omittedCount != other.omittedCount ||
		s.estimatedMemoryBytes != other.estimatedMemoryBytes || len(s.pages) != len(other.pages) {
		return false
	}
	for index := range s.pages {
		if s.pages[index].commitment != other.pages[index].commitment {
			return false
		}
	}
	return true
}
