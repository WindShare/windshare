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

	"github.com/windshare/windshare/core/transfer"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

const controlDirectoryName = outputcap.ControlDirectoryName

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
	id           outputcap.DestinationBindingID
	authorityRef receivecontract.AuthorityRef
	capabilities outputcap.DestinationCapabilities
	displayPath  string
}

func NewBinding(
	id outputcap.DestinationBindingID,
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

func (binding Binding) ID() outputcap.DestinationBindingID              { return binding.id }
func (binding Binding) AuthorityRef() receivecontract.AuthorityRef      { return binding.authorityRef }
func (binding Binding) Capabilities() outputcap.DestinationCapabilities { return binding.capabilities }
func (binding Binding) DisplayPath() string                             { return binding.displayPath }
func (binding Binding) Valid() bool {
	rebuilt, err := NewBinding(binding.id, binding.capabilities, binding.displayPath)
	return err == nil && rebuilt.id == binding.id && rebuilt.authorityRef == binding.authorityRef &&
		rebuilt.capabilities == binding.capabilities
}

type BindConfig struct {
	Platform              outputcap.Platform
	DisplayPath           string
	RecyclePrivateState   PrivateStateRecycler
	ControlUseNonceSource io.Reader
}

// BoundDestination retains only identity/private capabilities. A retained root
// duplicate witnesses replacement; it is deliberately never used for mutation.
type BoundDestination struct {
	mu sync.RWMutex

	binding        Binding
	identityScheme outputcap.IdentitySchemeID
	durability     transfer.DurabilityLevel
	platform       outputcap.Platform
	rootWitness    outputcap.Directory
	control        outputcap.Directory
	resumable      io.Closer
	controlUse     *controlUseLease
	recycler       PrivateStateRecycler
	closed         bool
}

func destinationFacts(config BindConfig) (outputcap.DestinationCapabilities, outputcap.IdentitySchemeID, transfer.DurabilityLevel, error) {
	if config.Platform == nil ||
		config.DisplayPath == "" || !filepath.IsAbs(config.DisplayPath) ||
		filepath.Clean(config.DisplayPath) != config.DisplayPath {
		return outputcap.DestinationCapabilities{}, "", 0, ErrInvalidConfiguration
	}
	capabilitySource, ok := config.Platform.(destinationCapabilitySource)
	if !ok {
		return outputcap.DestinationCapabilities{}, "", 0, errors.Join(
			ErrInvalidConfiguration, errors.New("native destination capability source is unavailable"),
		)
	}
	capabilities, err := capabilitySource.DestinationCapabilities()
	if err != nil || !capabilities.Valid() {
		return outputcap.DestinationCapabilities{}, "", 0, errors.Join(ErrInvalidConfiguration, err)
	}
	if err := capabilities.ValidateSupport(); err != nil {
		return outputcap.DestinationCapabilities{}, "", 0, err
	}
	evidence, ok := config.Platform.(recoveryEvidenceSource)
	if !ok {
		return outputcap.DestinationCapabilities{}, "", 0, ErrInvalidConfiguration
	}
	scheme, err := outputcap.NewIdentitySchemeID(string(evidence.IdentityScheme()))
	durability := evidence.Durability()
	if err != nil || durability < transfer.DurabilityProcessRestart || durability > transfer.DurabilityPowerLoss {
		return outputcap.DestinationCapabilities{}, "", 0, errors.Join(ErrInvalidConfiguration, err)
	}
	return capabilities, scheme, durability, nil
}

func BindDestination(config BindConfig) (_ *BoundDestination, resultErr error) {
	capabilities, scheme, durability, err := destinationFacts(config)
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
	root := guard.Root()
	if root == nil {
		return nil, errors.Join(ErrRetainedRootChanged, guard.Close())
	}
	rootWitness, err := root.Duplicate()
	if err != nil || rootWitness == nil {
		return nil, errors.Join(ErrRetainedRootChanged, err, closeDirectory(rootWitness), guard.Close())
	}
	defer func() {
		resultErr = finishDestinationBinding(guard, rootWitness, guardClosed, resultErr)
	}()

	if err := validateNativeMethodSets(root); err != nil {
		return nil, err
	}
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
	provider, ok := config.Platform.(destinationIdentityProvider)
	if !ok {
		return nil, ErrInvalidConfiguration
	}
	id, err := provider.BindDestinationIdentity(control)
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
		binding: binding, identityScheme: scheme, durability: durability,
		platform: config.Platform, rootWitness: rootWitness,
		control: control, controlUse: controlUse, recycler: config.RecyclePrivateState,
	}, nil
}

func validateNativeMethodSets(root outputcap.Directory) error {
	if _, ok := root.(fileNoReplacePublisher); !ok {
		return errors.Join(ErrInvalidConfiguration, errors.New("file publication is unavailable"))
	}
	if _, ok := root.(publicDirectoryReserver); !ok {
		return errors.Join(ErrInvalidConfiguration, errors.New("public directory reservation is unavailable"))
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

func (authority *BoundDestination) IdentityScheme() outputcap.IdentitySchemeID {
	if authority == nil {
		return ""
	}
	authority.mu.RLock()
	defer authority.mu.RUnlock()
	if authority.closed {
		return ""
	}
	return authority.identityScheme
}

// RecoveryDurability is the evidence captured at binding.
func (authority *BoundDestination) RecoveryDurability() transfer.DurabilityLevel {
	if authority == nil {
		return transfer.DurabilityNone
	}
	authority.mu.RLock()
	defer authority.mu.RUnlock()
	if authority.closed {
		return transfer.DurabilityNone
	}
	return authority.durability
}

// FileCheckpointOwnership projects the native root facts needed by the
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
		IdentityScheme:      authority.identityScheme,
		AuthorityRef:        authority.binding.AuthorityRef().Bytes(),
		RootOpenDisposition: disposition,
	})
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
	err = errors.Join(err, authority.recycleControlState())
	err = errors.Join(err, closeDirectory(authority.control))
	err = errors.Join(err, closeDirectory(authority.rootWitness))
	err = errors.Join(err, authority.platform.Close())
	authority.binding = Binding{}
	authority.platform, authority.rootWitness, authority.control = nil, nil, nil
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

// A root witness transfers to BoundDestination only after its placement guard
// closes successfully. Failed admission must release both capabilities.
func finishDestinationBinding(
	guard outputcap.PublicOperationGuard,
	witness outputcap.Directory,
	guardClosed bool,
	bindErr error,
) error {
	if !guardClosed {
		bindErr = errors.Join(bindErr, guard.Close())
	}
	if bindErr != nil {
		bindErr = errors.Join(bindErr, witness.Close())
	}
	return bindErr
}

// WithPublicContainer validates the retained destination witness before writing
// auxiliary output that must survive retirement of an individual result root.
func (authority *BoundDestination) WithPublicContainer(visit func(outputcap.Directory) error) error {
	return authority.withGuardedRoot(visit)
}
