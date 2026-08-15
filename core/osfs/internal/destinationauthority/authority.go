// Package destinationauthority owns the retained authority for ordinary native
// output. Display paths describe placement; only freshly guarded handles may
// authorize public namespace work.
package destinationauthority

import (
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"sync"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer/receivecontract"
)

const controlDirectoryName = ".windshare-output"

var (
	ErrInvalidBinding          = errors.New("destination authority binding is invalid")
	ErrInvalidConfiguration    = errors.New("destination authority configuration is invalid")
	ErrRetainedRootChanged     = errors.New("destination authority retained root changed")
	ErrControlNamespaceChanged = errors.New("destination authority control namespace changed")
	ErrAuthorityClosed         = errors.New("destination authority is closed")
)

// Binding keeps display location separate from native authority. The path helps
// users locate output but cannot be used to authorize reopening or mutation.
type Binding struct {
	id           outputcap.DestinationAuthorityID
	authorityRef receivecontract.AuthorityRef
	capabilities outputcap.DestinationCapabilities
	displayPath  string
}

func NewBinding(
	id outputcap.DestinationAuthorityID,
	capabilities outputcap.DestinationCapabilities,
	displayPath string,
) (Binding, error) {
	if id.IsZero() || !capabilities.Valid() || displayPath == "" || !filepath.IsAbs(displayPath) ||
		filepath.Clean(displayPath) != displayPath {
		return Binding{}, ErrInvalidBinding
	}
	authorityRef, err := id.AuthorityRef()
	if err != nil {
		return Binding{}, errors.Join(ErrInvalidBinding, err)
	}
	return Binding{
		id: id, authorityRef: authorityRef, capabilities: capabilities,
		displayPath: displayPath,
	}, nil
}

func (binding Binding) ID() outputcap.DestinationAuthorityID            { return binding.id }
func (binding Binding) AuthorityRef() receivecontract.AuthorityRef      { return binding.authorityRef }
func (binding Binding) Capabilities() outputcap.DestinationCapabilities { return binding.capabilities }
func (binding Binding) ExecutionMode() (outputcap.ExecutionMode, error) {
	if !binding.Valid() {
		return 0, ErrInvalidBinding
	}
	return outputcap.SelectExecutionMode(binding.capabilities)
}
func (binding Binding) DisplayPath() string { return binding.displayPath }
func (binding Binding) Valid() bool {
	rebuilt, err := NewBinding(binding.id, binding.capabilities, binding.displayPath)
	return err == nil && rebuilt.id == binding.id && rebuilt.authorityRef == binding.authorityRef &&
		rebuilt.capabilities == binding.capabilities
}

type BindConfig struct {
	Platform               outputcap.Platform
	DisplayPath            string
	OpenLiveCleanupJournal LiveCleanupJournalOpener
	RecyclePrivateState    PrivateStateRecycler
	ControlUseNonceSource  io.Reader
}

// BoundDestination retains only identity/private capabilities. A retained root
// duplicate witnesses replacement; it is deliberately never used for mutation.
type BoundDestination struct {
	mu sync.RWMutex

	binding     Binding
	platform    outputcap.Platform
	rootWitness outputcap.Directory
	control     outputcap.Directory
	proof       outputcap.Directory
	journal     LiveCleanupJournalHandle
	resumable   io.Closer
	controlUse  *controlUseLease
	recycler    PrivateStateRecycler
	profile     checkpointmodel.LiveCleanupNativeProfile
	closed      bool
}

func destinationFacts(
	config BindConfig,
) (outputcap.DestinationCapabilities, checkpointmodel.LiveCleanupNativeProfile, error) {
	if config.Platform == nil || config.OpenLiveCleanupJournal == nil ||
		config.DisplayPath == "" || !filepath.IsAbs(config.DisplayPath) ||
		filepath.Clean(config.DisplayPath) != config.DisplayPath {
		return outputcap.DestinationCapabilities{}, 0, ErrInvalidConfiguration
	}
	capabilitySource, ok := config.Platform.(destinationCapabilitySource)
	if !ok {
		return outputcap.DestinationCapabilities{}, 0, errors.Join(
			ErrInvalidConfiguration, errors.New("native destination capability source is unavailable"),
		)
	}
	profileSource, ok := config.Platform.(liveCleanupProfileSource)
	if !ok {
		return outputcap.DestinationCapabilities{}, 0, errors.Join(
			ErrInvalidConfiguration, errors.New("native live-cleanup profile source is unavailable"),
		)
	}
	capabilities, err := capabilitySource.DestinationCapabilities()
	if err != nil || !capabilities.Valid() {
		return outputcap.DestinationCapabilities{}, 0, errors.Join(ErrInvalidConfiguration, err)
	}
	profile := profileSource.LiveCleanupNativeProfile()
	if !profile.Valid() {
		return outputcap.DestinationCapabilities{}, 0, ErrInvalidConfiguration
	}
	return capabilities, profile, nil
}

func BindDestination(config BindConfig) (_ *BoundDestination, resultErr error) {
	capabilities, profile, err := destinationFacts(config)
	if err != nil {
		return nil, err
	}

	guard, err := config.Platform.AcquirePublicOperationGuard()
	if err != nil {
		return nil, err
	}
	if guard == nil {
		return nil, ErrRetainedRootChanged
	}
	guardClosed := false
	defer func() {
		if !guardClosed {
			resultErr = errors.Join(resultErr, guard.Close())
		}
	}()
	root := guard.Root()
	if root == nil {
		return nil, ErrRetainedRootChanged
	}
	rootWitness, err := root.Duplicate()
	if err != nil || rootWitness == nil {
		return nil, errors.Join(ErrRetainedRootChanged, err)
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, rootWitness.Close())
		}
	}()

	control, created, controlUse, err := bindControlUse(
		root, config.RecyclePrivateState != nil, config.ControlUseNonceSource,
	)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, abortControlUse(control, controlUse), control.Close())
		}
	}()
	rootClaim, controlClaim, err := bindIdentityClaims(root, control)
	if err != nil {
		return nil, err
	}
	if created {
		// The enrollment cut becomes recovery authority only after both the private
		// namespace and its parent are durable.
		if err := errors.Join(control.Sync(), root.Sync()); err != nil {
			return nil, err
		}
	}
	id, err := outputcap.NewDestinationAuthorityID(rootClaim, controlClaim)
	if err != nil {
		return nil, err
	}

	journal, err := config.OpenLiveCleanupJournal(control)
	if err != nil || !journal.valid() {
		return nil, errors.Join(ErrInvalidConfiguration, err)
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, journal.Close())
		}
	}()
	proof, err := openExactPrivateDirectory(control, checkpointmodel.LiveCleanupNamespaceV1)
	if err != nil {
		return nil, err
	}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, proof.Close())
		}
	}()
	if err := validateNativeMethodSets(root, proof, capabilities); err != nil {
		return nil, err
	}
	capabilities, err = reconcileLiveCleanup(journal.journal, proof, capabilities, profile)
	if err != nil {
		return nil, err
	}
	binding, err := NewBinding(id, capabilities, config.DisplayPath)
	if err != nil {
		return nil, err
	}
	if err := guard.Close(); err != nil {
		guardClosed = true
		return nil, err
	}
	guardClosed = true
	return &BoundDestination{
		binding: binding, platform: config.Platform, rootWitness: rootWitness,
		control: control, proof: proof, journal: journal, controlUse: controlUse,
		recycler: config.RecyclePrivateState, profile: profile,
	}, nil
}

func validateNativeMethodSets(
	root outputcap.Directory,
	proof outputcap.Directory,
	capabilities outputcap.DestinationCapabilities,
) error {
	if capabilities.SafePublish().Supported() {
		if _, ok := root.(fileNoReplacePublisher); !ok {
			return errors.Join(ErrInvalidConfiguration, errors.New("file publication is unavailable"))
		}
		if _, ok := root.(publicDirectoryReserver); !ok {
			return errors.Join(ErrInvalidConfiguration, errors.New("public directory reservation is unavailable"))
		}
	}
	if capabilities.CrashCleanup().Supported() {
		if _, ok := root.(liveCleanupStageCreator); !ok {
			return errors.Join(ErrInvalidConfiguration, errors.New("live-cleanup stage creation is unavailable"))
		}
		if _, ok := proof.(liveCleanupStageRemover); !ok {
			return errors.Join(ErrInvalidConfiguration, errors.New("live-cleanup stage removal is unavailable"))
		}
	}
	return nil
}

func (authority *BoundDestination) Binding() Binding {
	if authority == nil {
		return Binding{}
	}
	authority.mu.RLock()
	defer authority.mu.RUnlock()
	if authority.closed {
		return Binding{}
	}
	return authority.binding
}

// FileCheckpointOwnership projects the certified root facts needed by the
// unchanged FileCheckpointV2 binding without exposing native handles.
func (authority *BoundDestination) FileCheckpointOwnership(
	disposition outputcap.RootOpenDisposition,
) (checkpointmodel.Ownership, error) {
	if authority == nil || !disposition.Valid() {
		return checkpointmodel.Ownership{}, ErrInvalidConfiguration
	}
	authority.mu.RLock()
	defer authority.mu.RUnlock()
	if authority.closed || authority.platform == nil || !authority.binding.Valid() {
		return checkpointmodel.Ownership{}, ErrAuthorityClosed
	}
	return checkpointmodel.NewOwnership(checkpointmodel.OwnershipSpec{
		Materializer:        checkpointmodel.MaterializerNativeTree,
		Certification:       authority.platform.Certification(),
		AuthorityRef:        authority.binding.AuthorityRef().Bytes(),
		RootOpenDisposition: disposition,
	})
}

func (authority *BoundDestination) LiveCleanupProfile() checkpointmodel.LiveCleanupNativeProfile {
	if authority == nil {
		return 0
	}
	authority.mu.RLock()
	defer authority.mu.RUnlock()
	if authority.closed {
		return 0
	}
	return authority.profile
}

// withGuardedRoot serializes close against one public operation and proves that
// a fresh placement guard still names the root retained at bind time.
func (authority *BoundDestination) withGuardedRoot(
	operation func(outputcap.Directory) error,
) (resultErr error) {
	if authority == nil || operation == nil {
		return ErrInvalidConfiguration
	}
	authority.mu.RLock()
	defer authority.mu.RUnlock()
	if authority.closed || authority.platform == nil || authority.rootWitness == nil {
		return ErrAuthorityClosed
	}
	guard, err := authority.platform.AcquirePublicOperationGuard()
	if err != nil {
		return err
	}
	if guard == nil {
		return ErrRetainedRootChanged
	}
	defer func() { resultErr = errors.Join(resultErr, guard.Close()) }()
	root := guard.Root()
	if root == nil {
		return ErrRetainedRootChanged
	}
	same, err := root.SameDirectory(authority.rootWitness)
	if err != nil || !same {
		return errors.Join(ErrRetainedRootChanged, err)
	}
	return operation(root)
}

func (authority *BoundDestination) Close() error {
	if authority == nil {
		return nil
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.closed {
		return nil
	}
	authority.closed = true
	// Dependencies close from the record codec and deepest private namespace
	// outward. The control-use lease is released only after those handles stop
	// authorizing work, so the last user can safely recycle an empty namespace.
	var err error
	err = errors.Join(err, closeCloser(authority.resumable))
	err = errors.Join(err, authority.journal.Close())
	err = errors.Join(err, closeDirectory(authority.proof))
	err = errors.Join(err, authority.recycleControlState())
	err = errors.Join(err, closeDirectory(authority.control))
	err = errors.Join(err, closeDirectory(authority.rootWitness))
	err = errors.Join(err, authority.platform.Close())
	authority.binding = Binding{}
	authority.platform, authority.rootWitness, authority.control, authority.proof = nil, nil, nil, nil
	authority.controlUse, authority.recycler = nil, nil
	return err
}

type ResumableStateOpener func(outputcap.Directory) (io.Closer, error)

// OpenResumableState lets composition create a rich registry without exposing
// the authenticated control handle. The opener may capture its concrete return
// in outputruntime; BoundDestination retains only its ownership closer.
func (authority *BoundDestination) OpenResumableState(opener ResumableStateOpener) error {
	if authority == nil || opener == nil {
		return ErrInvalidConfiguration
	}
	authority.mu.Lock()
	defer authority.mu.Unlock()
	if authority.closed {
		return ErrAuthorityClosed
	}
	if authority.resumable != nil {
		return ErrInvalidConfiguration
	}
	mode, err := authority.binding.ExecutionMode()
	if err != nil || mode != outputcap.ExecutionResumable {
		return errors.Join(ErrInvalidConfiguration, err)
	}
	closer, err := opener(authority.control)
	if err != nil || closer == nil {
		return errors.Join(ErrInvalidConfiguration, err, closeCloser(closer))
	}
	authority.resumable = closer
	return nil
}

func openOrCreateExactPrivateDirectory(
	parent outputcap.Directory,
	name string,
) (outputcap.Directory, bool, error) {
	kind, exact, err := parent.ClassifyExactEntry(name)
	if err != nil || !exact {
		return nil, false, errors.Join(ErrControlNamespaceChanged, err)
	}
	switch kind {
	case outputcap.EntryAbsent:
		created, createErr := parent.CreateDirectory(name, true)
		if createErr != nil || created == nil {
			return nil, false, errors.Join(ErrControlNamespaceChanged, createErr, closeDirectory(created))
		}
		return created, true, nil
	case outputcap.EntryDirectory:
		opened, openErr := openExactPrivateDirectory(parent, name)
		return opened, false, openErr
	default:
		return nil, false, fmt.Errorf("%w: %q has entry kind %d", ErrControlNamespaceChanged, name, kind)
	}
}

func openExactPrivateDirectory(parent outputcap.Directory, name string) (outputcap.Directory, error) {
	kind, exact, err := parent.ClassifyExactEntry(name)
	if err != nil || !exact || kind != outputcap.EntryDirectory {
		return nil, errors.Join(ErrControlNamespaceChanged, err)
	}
	reference, err := parent.OpenEntry(name)
	if err != nil || reference == nil || reference.Kind() != outputcap.EntryDirectory {
		return nil, errors.Join(ErrControlNamespaceChanged, err, closeEntry(reference))
	}
	opened, openErr := parent.OpenPinnedDirectory(reference, true)
	closeErr := reference.Close()
	if openErr != nil || closeErr != nil || opened == nil {
		return nil, errors.Join(ErrControlNamespaceChanged, openErr, closeErr, closeDirectory(opened))
	}
	return opened, nil
}

func bindIdentityClaims(
	root outputcap.Directory,
	control outputcap.Directory,
) ([]byte, []byte, error) {
	rootPreparer, rootOK := root.(outputcap.PersistentDirectoryIdentityPreparer)
	controlPreparer, controlOK := control.(outputcap.PersistentDirectoryIdentityPreparer)
	if !rootOK || !controlOK {
		return nil, nil, errors.Join(
			ErrInvalidConfiguration,
			errors.New("persistent destination identity is unavailable"),
		)
	}
	// Native Object-ID state is deliberately handle-local. Rehydrating it is
	// idempotent and lets a fresh process compare the same durable identity;
	// later authenticated registry binding still rejects copied or replaced roots.
	rootClaim, rootErr := rootPreparer.PreparePersistentDirectoryIdentityClaim()
	controlClaim, controlErr := controlPreparer.PreparePersistentDirectoryIdentityClaim()
	if rootErr != nil || controlErr != nil {
		return nil, nil, errors.Join(rootErr, controlErr)
	}
	return validateIdentityClaims(rootClaim, controlClaim)
}

func validateIdentityClaims(rootClaim, controlClaim []byte) ([]byte, []byte, error) {
	if len(rootClaim) == 0 || len(rootClaim) > outputcap.MaxRootIdentityClaimBytes ||
		len(controlClaim) == 0 || len(controlClaim) > outputcap.MaxNamespaceIdentityClaimBytes {
		return nil, nil, outputcap.ErrInvalidDestinationAuthorityID
	}
	return rootClaim, controlClaim, nil
}

func downgradedCleanupCapabilities(
	capabilities outputcap.DestinationCapabilities,
	reason outputcap.CapabilityReason,
) (outputcap.DestinationCapabilities, error) {
	cleanup, err := outputcap.UnsupportedCapability(reason)
	if err != nil {
		return outputcap.DestinationCapabilities{}, err
	}
	return outputcap.NewDestinationCapabilities(
		capabilities.SafePublish(), capabilities.OperationRecovery(), capabilities.RangeRecovery(), cleanup,
	)
}

func closeDirectory(directory outputcap.Directory) error {
	if directory == nil {
		return nil
	}
	return directory.Close()
}

func closeEntry(reference outputcap.CurrentEntryReference) error {
	if reference == nil {
		return nil
	}
	return reference.Close()
}

func closeCloser(closer io.Closer) error {
	if closer == nil {
		return nil
	}
	return closer.Close()
}
