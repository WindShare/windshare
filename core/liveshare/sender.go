package liveshare

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/osfs"
	"github.com/windshare/windshare/core/senderobject"
	"github.com/windshare/windshare/core/session/catalogflow"
	"github.com/windshare/windshare/core/session/contentflow"
	"github.com/windshare/windshare/core/session/sessionruntime"
)

const defaultSharedBlockCacheBytes = uint64(64) << 20

type SenderConfig struct {
	Paths     []string
	Relays    []string
	ChunkSize uint32
	Random    io.Reader
	Now       func() time.Time

	preparation senderPreparationDependencies
}

type RegistrationMaterial struct {
	ShareID          []byte
	ShareInstance    []byte
	PKHash           []byte
	Descriptor       []byte
	SenderPrivateKey ed25519.PrivateKey
}

type RuntimeFactoryConfig struct {
	TerminalConnectivity sessionruntime.TerminalConnectivity
	PeerHandlers         sessionruntime.SenderPeerHandlerFactory
	TerminalObserver     sessionruntime.SenderTerminalObserver
}

type senderPreparationDependencies struct {
	newCatalogObjects func(catalogflow.SealedCatalogStoreConfig) (*catalogflow.SealedCatalogStore, error)
	newRecordSealer   func(records.SealerConfig) (*records.Sealer, error)
	sessionAuthKey    func(*content.KeyTree) (content.DerivedKey, error)
}

func productionSenderPreparationDependencies() senderPreparationDependencies {
	return senderPreparationDependencies{
		newCatalogObjects: catalogflow.NewSealedCatalogStore,
		newRecordSealer:   records.NewSealer,
		sessionAuthKey: func(keys *content.KeyTree) (content.DerivedKey, error) {
			return keys.SessionAuthKey()
		},
	}
}

type selectedCatalogSource interface {
	catalog.DirectoryScanner
	SelectedRoots() []catalog.NodeRecord
	Close() error
}

type PreparedSender struct {
	mu sync.Mutex

	capability       link.Link
	descriptor       catalog.ShareDescriptor
	descriptorObject []byte
	committedRoot    catalog.CommittedRoot
	privateKey       ed25519.PrivateKey
	shareIDRaw       []byte
	pkHash           []byte
	sessionAuthKey   []byte
	random           *lockedReader

	selectedSource  selectedCatalogSource
	revisionSource  *osfs.RootedRevisionSource
	catalogStore    *catalog.CatalogStore
	revisionStore   *content.RevisionStore
	keyTree         *content.KeyTree
	cache           *contentflow.SharedBlockCache
	catalogAccess   *senderCatalogAccess
	catalogObjects  *catalogflow.SealedCatalogStore
	recordSealer    *records.Sealer
	runtimeFactory  senderRuntimeLifecycle
	closed          bool
	closeDone       chan struct{}
	closeResult     error
	sessionSequence atomic.Uint64
}

type senderAuthority struct {
	publicKey      ed25519.PublicKey
	privateKey     ed25519.PrivateKey
	capability     link.Link
	shareIDRaw     []byte
	shareInstance  catalog.ShareInstance
	syntheticRoot  catalog.DirectoryID
	rootGeneration catalog.DirectoryGeneration
}

func (authority *senderAuthority) destroy() {
	clear(authority.privateKey)
	clear(authority.capability.ReadSecret)
	authority.privateKey = nil
	authority.capability.ReadSecret = nil
}

type senderCatalog struct {
	descriptor       catalog.ShareDescriptor
	descriptorObject []byte
	committedRoot    catalog.CommittedRoot
}

func PrepareSender(ctx context.Context, config SenderConfig) (*PreparedSender, error) {
	dependencies := config.preparation
	if dependencies.newCatalogObjects == nil && dependencies.newRecordSealer == nil && dependencies.sessionAuthKey == nil {
		dependencies = productionSenderPreparationDependencies()
	}
	return prepareSender(ctx, config, dependencies)
}

func prepareSender(
	ctx context.Context,
	config SenderConfig,
	dependencies senderPreparationDependencies,
) (result *PreparedSender, resultErr error) {
	if dependencies.newCatalogObjects == nil || dependencies.newRecordSealer == nil || dependencies.sessionAuthKey == nil {
		return nil, errors.New("live share sender preparation dependencies are incomplete")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(config.Paths) == 0 || len(config.Relays) == 0 {
		return nil, errors.New("live share sender requires selected paths and at least one relay")
	}
	if config.ChunkSize == 0 {
		config.ChunkSize = catalog.DefaultChunkSize
	}
	if _, err := content.NewFileGeometry(0, config.ChunkSize); err != nil {
		return nil, err
	}
	if config.Random == nil {
		config.Random = rand.Reader
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	random := &lockedReader{reader: config.Random}
	sender := &PreparedSender{random: random}
	defer func() {
		if resultErr != nil {
			resultErr = errors.Join(resultErr, sender.Close())
		}
	}()
	authority, err := prepareSenderAuthority(config, random, sender)
	if err != nil {
		return nil, err
	}
	defer authority.destroy()
	catalogState, err := prepareSenderCatalog(ctx, config, random, sender, authority, nil, dependencies)
	if err != nil {
		return nil, err
	}
	if err := prepareSenderContent(config, random, sender, authority, dependencies); err != nil {
		return nil, err
	}
	commitSenderPreparation(sender, authority, catalogState)
	return sender, nil
}

func prepareSenderAuthority(config SenderConfig, random *lockedReader, sender *PreparedSender) (authority senderAuthority, resultErr error) {
	defer func() {
		if resultErr != nil {
			authority.destroy()
		}
	}()
	var err error
	authority.publicKey, authority.privateKey, err = ed25519.GenerateKey(random)
	if err != nil {
		return authority, fmt.Errorf("generate sender identity: %w", err)
	}
	readSecret, err := link.NewReadSecret(random)
	if err != nil {
		return authority, err
	}
	defer clear(readSecret)
	authority.capability, err = link.NewSenderAuthenticated(readSecret, authority.publicKey, config.Relays)
	if err != nil {
		return authority, err
	}
	authority.shareIDRaw, err = base64.RawURLEncoding.Strict().DecodeString(authority.capability.ShareID)
	if err != nil {
		return authority, err
	}
	authority.shareInstance, err = randomShareInstance(random)
	if err != nil {
		return authority, err
	}
	authority.syntheticRoot, err = randomDirectoryID(random)
	if err != nil {
		return authority, err
	}
	authority.rootGeneration, err = randomDirectoryGeneration(random)
	if err != nil {
		return authority, err
	}
	selected, err := osfs.NewSelectedCatalogSource(osfs.SelectedCatalogSourceConfig{
		Paths: config.Paths, SyntheticRoot: authority.syntheticRoot,
		Identities: osfs.CatalogIdentitySourceFunc(func() ([catalog.IdentityBytes]byte, error) {
			var identity [catalog.IdentityBytes]byte
			_, err := io.ReadFull(random, identity[:])
			return identity, err
		}),
	})
	if err != nil {
		return authority, err
	}
	sender.selectedSource = selected
	sender.revisionSource, err = selected.RevisionSource()
	if err != nil {
		return authority, err
	}
	sender.keyTree, err = content.NewKeyTree(readSecret, authority.shareInstance)
	return authority, err
}

func prepareSenderCatalog(
	ctx context.Context,
	config SenderConfig,
	random *lockedReader,
	sender *PreparedSender,
	authority senderAuthority,
	spillFactory catalog.SpillFactory,
	dependencies senderPreparationDependencies,
) (senderCatalog, error) {
	catalogKey, err := sender.keyTree.CatalogKey()
	if err != nil {
		return senderCatalog{}, err
	}
	defer catalogKey.Destroy()
	objects, err := dependencies.newCatalogObjects(catalogflow.SealedCatalogStoreConfig{
		ShareInstance: authority.shareInstance, CatalogKey: catalogKey.Bytes(),
		SenderPrivateKey: authority.privateKey, NonceSource: random,
	})
	if err != nil {
		return senderCatalog{}, err
	}
	sender.catalogObjects = objects
	processBudget, err := catalog.NewBudgetAccount("live-share-process", catalog.DefaultProcessBudgetLimits())
	if err != nil {
		return senderCatalog{}, err
	}
	shareBudget, err := catalog.NewBudgetAccount("live-share", catalog.DefaultShareBudgetLimits())
	if err != nil {
		return senderCatalog{}, err
	}
	startupBudget, err := catalog.NewBudgetAccount("live-share-startup", catalog.DefaultSessionBudgetLimits())
	if err != nil {
		return senderCatalog{}, err
	}
	sender.catalogStore, err = catalog.NewCatalogStore(catalog.StoreConfig{
		ShareInstance: authority.shareInstance, Backend: catalog.NewMemoryCatalogBackend(),
		ProcessBudget: processBudget, ShareBudget: shareBudget, PageSealer: objects, SpillFactory: spillFactory,
	})
	if err != nil {
		return senderCatalog{}, err
	}
	rootCommit, err := catalog.NewSyntheticRootCommit(catalog.SyntheticRootCommitSpec{
		ShareInstance: authority.shareInstance, SyntheticRoot: authority.syntheticRoot,
		Generation: authority.rootGeneration, SelectedRoots: sender.selectedSource.SelectedRoots(),
	})
	if err != nil {
		return senderCatalog{}, err
	}
	committedRoot, err := sender.catalogStore.CommitSyntheticRoot(ctx, rootCommit, startupBudget)
	if err != nil {
		return senderCatalog{}, err
	}
	descriptor, err := catalog.NewShareDescriptor(catalog.DescriptorSpec{
		WireVersion: catalog.WireVersionV2, Suite: catalog.SuiteV2,
		ShareInstance: authority.shareInstance, SyntheticRoot: authority.syntheticRoot, RootCommit: committedRoot,
		ChunkSize:       config.ChunkSize,
		Capabilities:    catalog.CapabilityCatalog | catalog.CapabilityRanges | catalog.CapabilityAuthenticatedModifiedTime,
		SenderPublicKey: authority.publicKey, CreatedAtSeconds: uint64(config.Now().Unix()), PathPolicy: catalog.PathPolicyV1,
	})
	if err != nil {
		return senderCatalog{}, err
	}
	descriptorObject, err := sealPreparedDescriptor(random, sender.keyTree, authority, descriptor)
	if err != nil {
		return senderCatalog{}, err
	}
	sender.catalogAccess, err = newSenderCatalogAccess(
		authority.shareInstance,
		sender.catalogStore,
		sender.selectedSource,
		sender.selectedSource.SelectedRoots(),
	)
	if err != nil {
		return senderCatalog{}, err
	}
	return senderCatalog{descriptor: descriptor, descriptorObject: descriptorObject, committedRoot: committedRoot}, nil
}

func sealPreparedDescriptor(
	random io.Reader,
	keyTree *content.KeyTree,
	authority senderAuthority,
	descriptor catalog.ShareDescriptor,
) ([]byte, error) {
	descriptorKey, err := keyTree.DescriptorKey(authority.capability.PKHash)
	if err != nil {
		return nil, err
	}
	defer descriptorKey.Destroy()
	nonce := make([]byte, senderobject.NonceBytes)
	if _, err := io.ReadFull(random, nonce); err != nil {
		return nil, err
	}
	return catalogflow.SealDescriptor(descriptor, catalogflow.DescriptorObjectConfig{
		PKHash: authority.capability.PKHash, ShareIDRaw: authority.shareIDRaw,
		DescriptorKey: descriptorKey.Bytes(), SenderPrivateKey: authority.privateKey, Nonce: nonce,
	})
}

func prepareSenderContent(
	config SenderConfig,
	random io.Reader,
	sender *PreparedSender,
	authority senderAuthority,
	dependencies senderPreparationDependencies,
) error {
	processQuota, err := content.NewQuotaAccount("live-share-content-process", content.DefaultProcessQuotaLimits())
	if err != nil {
		return err
	}
	shareQuota, err := content.NewQuotaAccount("live-share-content", content.DefaultShareQuotaLimits())
	if err != nil {
		return err
	}
	sender.revisionStore, err = content.NewRevisionStore(content.RevisionStoreConfig{
		ShareInstance: authority.shareInstance, ChunkSize: config.ChunkSize, Catalog: sender.catalogStore,
		Source: sender.revisionSource, ProcessQuota: processQuota, ShareQuota: shareQuota,
	})
	if err != nil {
		return err
	}
	sender.recordSealer, err = dependencies.newRecordSealer(records.SealerConfig{
		ShareInstance: authority.shareInstance, Keys: sender.keyTree,
		SigningKey: authority.privateKey, NonceSource: random,
	})
	if err != nil {
		return err
	}
	cacheBudget, err := contentflow.NewProcessCacheBudget(defaultSharedBlockCacheBytes)
	if err != nil {
		return err
	}
	sender.cache, err = contentflow.NewSharedBlockCache(authority.shareInstance, defaultSharedBlockCacheBytes, cacheBudget)
	if err != nil {
		return err
	}
	sessionAuthKey, err := dependencies.sessionAuthKey(sender.keyTree)
	if err != nil {
		return err
	}
	defer sessionAuthKey.Destroy()
	sender.sessionAuthKey = sessionAuthKey.Bytes()
	return nil
}

func commitSenderPreparation(sender *PreparedSender, authority senderAuthority, catalogState senderCatalog) {
	sender.capability = authority.capability
	sender.capability.ReadSecret = append([]byte(nil), authority.capability.ReadSecret...)
	sender.capability.PKHash = append([]byte(nil), authority.capability.PKHash...)
	sender.capability.Relays = append([]string(nil), authority.capability.Relays...)
	sender.descriptor = catalogState.descriptor
	sender.descriptorObject = catalogState.descriptorObject
	sender.committedRoot = catalogState.committedRoot
	sender.privateKey = append(ed25519.PrivateKey(nil), authority.privateKey...)
	sender.shareIDRaw = append([]byte(nil), authority.shareIDRaw...)
	sender.pkHash = append([]byte(nil), authority.capability.PKHash...)
}

func (sender *PreparedSender) Capability() link.Link {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if sender.closed {
		return link.Link{}
	}
	result := sender.capability
	result.ReadSecret = append([]byte(nil), result.ReadSecret...)
	result.PKHash = append([]byte(nil), result.PKHash...)
	result.Relays = append([]string(nil), result.Relays...)
	return result
}

func (sender *PreparedSender) Registration() RegistrationMaterial {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if sender.closed {
		return RegistrationMaterial{}
	}
	return RegistrationMaterial{
		ShareID: append([]byte(nil), sender.shareIDRaw...), ShareInstance: sender.descriptor.ShareInstance().Bytes(),
		PKHash: append([]byte(nil), sender.pkHash...), Descriptor: append([]byte(nil), sender.descriptorObject...),
		SenderPrivateKey: append(ed25519.PrivateKey(nil), sender.privateKey...),
	}
}

func (sender *PreparedSender) AuthorizeRegistration() error {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if sender.closed {
		return errors.New("live share sender is closed")
	}
	return sender.committedRoot.AuthorizeRegistration(sender.descriptor)
}

// StartRootPrefetch is intentionally separate from preparation and registration
// authorization. The relay owner calls it only after REGISTERED and link
// publication, so the ready path cannot observe a descendant.
func (sender *PreparedSender) StartRootPrefetch() {
	if sender == nil {
		return
	}
	sender.mu.Lock()
	if sender.closed {
		sender.mu.Unlock()
		return
	}
	access := sender.catalogAccess
	sender.mu.Unlock()
	access.StartRootPrefetch()
}

func (sender *PreparedSender) NewRuntimeFactory(config RuntimeFactoryConfig) (*sessionruntime.SenderFactory, error) {
	sender.mu.Lock()
	defer sender.mu.Unlock()
	if sender.closed || sender.runtimeFactory != nil || config.TerminalConnectivity == nil || config.PeerHandlers == nil {
		return nil, errors.New("live share sender runtime factory is unavailable")
	}
	contentFactory := sessionruntime.SenderContentFactoryFunc(func() (*contentflow.SenderService, error) {
		sequence := sender.sessionSequence.Add(1)
		quota, err := content.NewQuotaAccount(fmt.Sprintf("live-share-session-%d", sequence), content.DefaultSessionQuotaLimits())
		if err != nil {
			return nil, err
		}
		return contentflow.NewSenderService(contentflow.SenderServiceConfig{
			Store: sender.revisionStore, SessionQuota: quota, Sealer: sender.recordSealer, Cache: sender.cache,
		})
	})
	factory, err := sessionruntime.NewSenderFactory(sessionruntime.SenderFactoryConfig{
		ShareInstance: sender.descriptor.ShareInstance(), SessionAuthKey: sender.sessionAuthKey,
		SenderPrivateKey: sender.privateKey, Catalog: sender.catalogAccess, Content: contentFactory,
		Peers:            config.PeerHandlers,
		TerminalObserver: config.TerminalObserver,
		Random:           sender.random, TerminalConnectivity: prefetchTerminalConnectivity{
			prefetch: sender.catalogAccess,
			delegate: config.TerminalConnectivity,
		},
	})
	if err == nil {
		// The prepared sender retains the unique factory authority so its own
		// Close boundary can stop admission and join every accepted session.
		sender.runtimeFactory = factory
	}
	return factory, err
}

func randomShareInstance(random io.Reader) (catalog.ShareInstance, error) {
	identity, err := randomIdentity(random)
	if err != nil {
		return catalog.ShareInstance{}, err
	}
	return catalog.ShareInstanceFromBytes(identity[:])
}

func randomDirectoryID(random io.Reader) (catalog.DirectoryID, error) {
	identity, err := randomIdentity(random)
	if err != nil {
		return catalog.DirectoryID{}, err
	}
	return catalog.DirectoryIDFromBytes(identity[:])
}

func randomDirectoryGeneration(random io.Reader) (catalog.DirectoryGeneration, error) {
	identity, err := randomIdentity(random)
	if err != nil {
		return catalog.DirectoryGeneration{}, err
	}
	return catalog.DirectoryGenerationFromBytes(identity[:])
}

func randomIdentity(random io.Reader) ([catalog.IdentityBytes]byte, error) {
	var identity [catalog.IdentityBytes]byte
	_, err := io.ReadFull(random, identity[:])
	if err != nil {
		return identity, fmt.Errorf("generate live-share identity: %w", err)
	}
	if identity == [catalog.IdentityBytes]byte{} {
		return identity, errors.New("generated a zero live-share identity")
	}
	return identity, nil
}
