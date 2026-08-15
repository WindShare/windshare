package destinationauthority

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"strings"
	"sync"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

var errDestinationFake = errors.New("destination fake failure")

type destinationNode struct {
	id       uint64
	private  bool
	identity []byte
	entries  map[string]*destinationNode
	file     *destinationFile
	closed   int
}

type destinationPlatform struct {
	mu sync.Mutex

	root         *destinationNode
	guardRoot    *destinationNode
	nextID       uint64
	capabilities outputcap.DestinationCapabilities
	profile      checkpointmodel.LiveCleanupNativeProfile
	guardCalls   int
	capCalls     int
	closeOrder   *[]string
	closed       bool

	directoryOutcome outputcap.PublishNoReplaceOutcome
	directoryErr     error
	fileOutcome      outputcap.PublishNoReplaceOutcome
	fileErr          error
	createStageErr   error
	stageParent      *destinationNode
}

func newDestinationPlatform() *destinationPlatform {
	supported := outputcap.SupportedCapability()
	capabilities, _ := outputcap.NewDestinationCapabilities(supported, supported, supported, supported)
	root := &destinationNode{id: 1, identity: []byte("root-identity"), entries: map[string]*destinationNode{}}
	return &destinationPlatform{
		root: root, guardRoot: root, nextID: 2, capabilities: capabilities,
		profile: checkpointmodel.LiveCleanupWindowsNTFSV1,
	}
}

func (platform *destinationPlatform) DestinationCapabilities() (outputcap.DestinationCapabilities, error) {
	platform.capCalls++
	return platform.capabilities, nil
}
func (platform *destinationPlatform) LiveCleanupNativeProfile() checkpointmodel.LiveCleanupNativeProfile {
	return platform.profile
}
func (platform *destinationPlatform) Root() outputcap.Directory {
	return &destinationDirectory{platform: platform, node: platform.root}
}
func (*destinationPlatform) RootOpenDisposition() outputcap.RootOpenDisposition {
	return outputcap.CallerProvidedContainer
}
func (platform *destinationPlatform) AcquirePublicOperationGuard() (outputcap.PublicOperationGuard, error) {
	platform.guardCalls++
	return &destinationGuard{root: &destinationDirectory{platform: platform, node: platform.guardRoot}}, nil
}
func (*destinationPlatform) RootBinding() (outputcap.OutputRootBinding, error) {
	return outputcap.OutputRootBinding{}, errDestinationFake
}
func (*destinationPlatform) Certification() outputcap.CertificationID {
	return outputcap.CertificationWindowsNTFSProcessRestart
}
func (*destinationPlatform) Durability() transfer.DurabilityLevel {
	return transfer.DurabilityProcessRestart
}
func (*destinationPlatform) ProbeRecoverableFeatures() error                 { return nil }
func (*destinationPlatform) ValidateModifiedTime(catalog.ModifiedTime) error { return nil }
func (*destinationPlatform) CanonicalLocatorKey(path string) (string, error) {
	return strings.ToUpper(path), nil
}
func (*destinationPlatform) CanonicalComponentKey(name string) (string, error) {
	if name == "" || strings.ContainsAny(name, "/\\\x00") {
		return "", errDestinationFake
	}
	return strings.ToUpper(name), nil
}
func (platform *destinationPlatform) Close() error {
	platform.closed = true
	if platform.closeOrder != nil {
		*platform.closeOrder = append(*platform.closeOrder, "platform")
	}
	return nil
}

type destinationGuard struct{ root outputcap.Directory }

func (guard *destinationGuard) Root() outputcap.Directory { return guard.root }
func (*destinationGuard) Close() error                    { return nil }

type destinationDirectory struct {
	platform *destinationPlatform
	node     *destinationNode
}

type destinationLiveStageParent struct {
	directory outputcap.Directory
	before    error
	after     error
	calls     int
}

func (parent *destinationLiveStageParent) WithExactParent(
	ctx context.Context,
	operation func(outputcap.Directory) error,
) error {
	if parent == nil || ctx == nil || operation == nil || parent.directory == nil {
		return errDestinationFake
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	parent.calls++
	if parent.before != nil {
		return parent.before
	}
	return errors.Join(operation(parent.directory), parent.after)
}

func destinationRootLiveStageParent(platform *destinationPlatform) *destinationLiveStageParent {
	return &destinationLiveStageParent{
		directory: &destinationDirectory{platform: platform, node: platform.guardRoot},
	}
}

func (directory *destinationDirectory) Close() error {
	directory.node.closed++
	return nil
}
func (directory *destinationDirectory) Duplicate() (outputcap.Directory, error) {
	return &destinationDirectory{platform: directory.platform, node: directory.node}, nil
}
func (*destinationDirectory) Sync() error { return nil }
func (directory *destinationDirectory) Names(limit int) ([]string, error) {
	if len(directory.node.entries) > limit {
		return nil, errDestinationFake
	}
	names := make([]string, 0, len(directory.node.entries))
	for name := range directory.node.entries {
		names = append(names, name)
	}
	return names, nil
}
func (directory *destinationDirectory) ObserveEntry(name string) (outputcap.EntryKind, error) {
	kind, _, err := directory.ClassifyExactEntry(name)
	return kind, err
}
func (directory *destinationDirectory) ClassifyExactEntry(name string) (outputcap.EntryKind, bool, error) {
	node := directory.node.entries[name]
	if node == nil {
		return outputcap.EntryAbsent, true, nil
	}
	if node.file != nil {
		return outputcap.EntryRegularFile, true, nil
	}
	return outputcap.EntryDirectory, true, nil
}
func (directory *destinationDirectory) OpenEntry(name string) (outputcap.CurrentEntryReference, error) {
	node := directory.node.entries[name]
	if node == nil {
		return nil, fs.ErrNotExist
	}
	kind := outputcap.EntryDirectory
	if node.file != nil {
		kind = outputcap.EntryRegularFile
	}
	return &destinationEntryReference{node: node, kind: kind}, nil
}
func (directory *destinationDirectory) EntryMatches(name string, expected outputcap.CurrentEntryReference) (bool, error) {
	reference, ok := expected.(*destinationEntryReference)
	return ok && directory.node.entries[name] == reference.node, nil
}
func (directory *destinationDirectory) OpenPinnedDirectory(expected outputcap.CurrentEntryReference, private bool) (outputcap.Directory, error) {
	reference, ok := expected.(*destinationEntryReference)
	if !ok || reference.kind != outputcap.EntryDirectory || reference.node.private != private {
		return nil, errDestinationFake
	}
	return &destinationDirectory{platform: directory.platform, node: reference.node}, nil
}
func (*destinationDirectory) RemoveEntry(string, outputcap.CurrentEntryReference) error {
	return errDestinationFake
}
func (directory *destinationDirectory) SameDirectory(other outputcap.Directory) (bool, error) {
	right, ok := other.(*destinationDirectory)
	return ok && right.node == directory.node, nil
}
func (*destinationDirectory) SetModifiedTime(catalog.ModifiedTime) error { return nil }
func (directory *destinationDirectory) OpenDirectory(name string, private bool) (outputcap.Directory, error) {
	reference, err := directory.OpenEntry(name)
	if err != nil {
		return nil, err
	}
	return directory.OpenPinnedDirectory(reference, private)
}
func (directory *destinationDirectory) CreateDirectory(name string, private bool) (outputcap.Directory, error) {
	if directory.node.entries[name] != nil {
		return nil, outputcap.ErrNamespaceCollision
	}
	node := &destinationNode{
		id: directory.platform.nextID, private: private,
		identity: []byte("identity-" + name), entries: map[string]*destinationNode{},
	}
	directory.platform.nextID++
	directory.node.entries[name] = node
	return &destinationDirectory{platform: directory.platform, node: node}, nil
}
func (*destinationDirectory) InstallDirectoryNoReplace(outputcap.Directory, string) (outputcap.Directory, error) {
	return nil, errDestinationFake
}
func (*destinationDirectory) RemoveDirectory(string, outputcap.Directory) error {
	return errDestinationFake
}
func (*destinationDirectory) CreateFile(string, bool, int64) (outputcap.File, error) {
	return nil, errDestinationFake
}
func (directory *destinationDirectory) OpenFile(name string, _ bool, _ bool) (outputcap.File, error) {
	node := directory.node.entries[name]
	if node == nil || node.file == nil {
		return nil, fs.ErrNotExist
	}
	return node.file, nil
}
func (*destinationDirectory) LinkFileNoReplace(outputcap.File, string) (outputcap.File, error) {
	return nil, errDestinationFake
}
func (*destinationDirectory) ReplacePrivateFile(outputcap.File, string) error {
	return errDestinationFake
}
func (*destinationDirectory) RemoveFile(string, outputcap.File) error { return errDestinationFake }
func (*destinationDirectory) AcquireLock(string, bool) (outputcap.Lock, bool, error) {
	return nil, false, errDestinationFake
}
func (directory *destinationDirectory) PersistentDirectoryIdentityClaim() ([]byte, error) {
	return append([]byte(nil), directory.node.identity...), nil
}
func (directory *destinationDirectory) PreparePersistentDirectoryIdentityClaim() ([]byte, error) {
	return directory.PersistentDirectoryIdentityClaim()
}
func (directory *destinationDirectory) ReservePublicDirectoryNoReplace(name string) (outputcap.Directory, outputcap.PublishNoReplaceOutcome, error) {
	if directory.platform.directoryOutcome != 0 {
		switch directory.platform.directoryOutcome {
		case outputcap.PublishNoReplaceCollision:
			return nil, outputcap.PublishNoReplaceCollision, directory.platform.directoryErr
		case outputcap.PublishNoReplaceIndeterminate:
			created, _ := directory.CreateDirectory(name, false)
			return created, outputcap.PublishNoReplaceIndeterminate, directory.platform.directoryErr
		case outputcap.PublishNoReplaceCommitted:
			created, createErr := directory.CreateDirectory(name, false)
			return created, outputcap.PublishNoReplaceCommitted, errors.Join(createErr, directory.platform.directoryErr)
		}
	}
	created, err := directory.CreateDirectory(name, false)
	if errors.Is(err, outputcap.ErrNamespaceCollision) {
		return nil, outputcap.PublishNoReplaceCollision, nil
	}
	return created, outputcap.PublishNoReplaceCommitted, err
}
func (directory *destinationDirectory) PublishFileNoReplace(outputcap.File, string) (outputcap.PublishNoReplaceOutcome, error) {
	if directory.platform.fileOutcome != 0 {
		return directory.platform.fileOutcome, directory.platform.fileErr
	}
	return outputcap.PublishNoReplaceCommitted, directory.platform.fileErr
}
func (directory *destinationDirectory) CreateLiveCleanupStage(proof outputcap.Directory, ticket checkpointmodel.LiveCleanupTicket) error {
	if directory.platform.createStageErr != nil {
		return directory.platform.createStageErr
	}
	directory.platform.stageParent = directory.node
	target := proof.(*destinationDirectory)
	file := &destinationFile{size: ticket.ExactSize()}
	target.node.entries[ticket.StageName()] = &destinationNode{id: directory.platform.nextID, file: file}
	directory.platform.nextID++
	return nil
}
func (directory *destinationDirectory) RemoveLiveCleanupStage(ticket checkpointmodel.LiveCleanupTicket, expected outputcap.File) error {
	node := directory.node.entries[ticket.StageName()]
	if node == nil || node.file != expected {
		return outputcap.ErrUnsafeNamespace
	}
	delete(directory.node.entries, ticket.StageName())
	return nil
}

type destinationEntryReference struct {
	node *destinationNode
	kind outputcap.EntryKind
}

func (reference *destinationEntryReference) Kind() outputcap.EntryKind { return reference.kind }
func (*destinationEntryReference) Close() error                        { return nil }

type destinationFile struct {
	data   []byte
	size   uint64
	closed int
}

func (file *destinationFile) ReadAt(p []byte, offset int64) (int, error) {
	if offset >= int64(len(file.data)) {
		return 0, io.EOF
	}
	return copy(p, file.data[offset:]), nil
}
func (file *destinationFile) WriteAt(p []byte, offset int64) (int, error) {
	end := int(offset) + len(p)
	if end > len(file.data) {
		file.data = append(file.data, make([]byte, end-len(file.data))...)
	}
	copy(file.data[offset:], p)
	return len(p), nil
}
func (file *destinationFile) Close() error                                          { file.closed++; return nil }
func (*destinationFile) Sync() error                                                { return nil }
func (file *destinationFile) Size() (uint64, error)                                 { return file.size, nil }
func (*destinationFile) SetModifiedTime(catalog.ModifiedTime) error                 { return nil }
func (*destinationFile) MetadataMatches(uint64, catalog.ModifiedTime) (bool, error) { return true, nil }
func (file *destinationFile) SameFile(other outputcap.File) (bool, error) {
	return file == other, nil
}

type destinationJournal struct {
	snapshot    LiveCleanupSnapshot
	tickets     map[string]checkpointmodel.LiveCleanupTicket
	closed      bool
	order       *[]string
	snapshotErr error
	createErr   error
	replaceErr  error
	deleteErr   error
}

func (journal *destinationJournal) Snapshot(int) (LiveCleanupSnapshot, error) {
	return journal.snapshot, journal.snapshotErr
}
func (journal *destinationJournal) Create(ticket checkpointmodel.LiveCleanupTicket) error {
	if journal.createErr != nil {
		return journal.createErr
	}
	if journal.tickets == nil {
		journal.tickets = map[string]checkpointmodel.LiveCleanupTicket{}
	}
	journal.tickets[ticket.StageName()] = ticket
	return nil
}
func (journal *destinationJournal) Replace(previous, next checkpointmodel.LiveCleanupTicket) error {
	if journal.replaceErr != nil {
		return journal.replaceErr
	}
	journal.tickets[previous.StageName()] = next
	return nil
}
func (journal *destinationJournal) Delete(ticket checkpointmodel.LiveCleanupTicket) error {
	if journal.deleteErr != nil {
		return journal.deleteErr
	}
	delete(journal.tickets, ticket.StageName())
	return nil
}
func (journal *destinationJournal) Close() error {
	journal.closed = true
	if journal.order != nil {
		*journal.order = append(*journal.order, "journal")
	}
	return nil
}

type reservationClaimer struct {
	collisions      int
	claims          []*reservationClaimHandle
	specs           []ReservationClaimSpec
	collisionHandle bool
}

func (claimer *reservationClaimer) BeginReservation(spec ReservationClaimSpec) (ReservationClaimHandle, ReservationMetadataClaimOutcome, error) {
	if !spec.Valid() {
		return nil, 0, errDestinationFake
	}
	claimer.specs = append(claimer.specs, spec)
	if claimer.collisions > 0 {
		claimer.collisions--
		if claimer.collisionHandle {
			var token ReservationClaimToken
			token[0] = 0xff
			handle := &reservationClaimHandle{claim: ReservationClaim{Token: token, Generation: 1}}
			claimer.claims = append(claimer.claims, handle)
			return handle, ReservationMetadataClaimCollision, nil
		}
		return nil, ReservationMetadataClaimCollision, nil
	}
	var token ReservationClaimToken
	token[0] = byte(len(claimer.claims) + 1)
	handle := &reservationClaimHandle{claim: ReservationClaim{Token: token, Generation: 1}}
	claimer.claims = append(claimer.claims, handle)
	return handle, ReservationMetadataClaimCommitted, nil
}

type reservationClaimHandle struct {
	claim      ReservationClaim
	bound      bool
	identity   []byte
	rolledBack bool
	closed     bool
}

func (handle *reservationClaimHandle) Claim() ReservationClaim { return handle.claim }
func (handle *reservationClaimHandle) BindReservation(receivecontract.DestinationReservation) (ReservationMetadataClaimOutcome, error) {
	handle.bound = true
	handle.claim.Generation++
	return ReservationMetadataClaimCommitted, nil
}
func (handle *reservationClaimHandle) BindDirectoryIdentity(identity []byte) (ReservationMetadataClaimOutcome, error) {
	handle.identity = append([]byte(nil), identity...)
	handle.claim.Generation++
	return ReservationMetadataClaimCommitted, nil
}
func (handle *reservationClaimHandle) Rollback() (ReservationMetadataClaimOutcome, error) {
	handle.rolledBack = true
	return ReservationMetadataClaimCommitted, nil
}
func (handle *reservationClaimHandle) Close() error { handle.closed = true; return nil }

type orderedCloser struct{ order *[]string }

func (closer orderedCloser) Close() error {
	*closer.order = append(*closer.order, "resumable")
	return nil
}
