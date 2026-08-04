package liveshare

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	mathrand "math/rand"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/content/records"
	"github.com/windshare/windshare/core/link"
	"github.com/windshare/windshare/core/session/catalogflow"
)

type senderPreparationCatalogSource struct {
	selected []catalog.NodeRecord
}

func (source *senderPreparationCatalogSource) SelectedRoots() []catalog.NodeRecord {
	return append([]catalog.NodeRecord(nil), source.selected...)
}

func (*senderPreparationCatalogSource) ScanDirectory(context.Context, catalog.ScanRequest) (catalog.ScanResult, error) {
	return catalog.ScanResult{}, errors.New("sender preparation coverage source does not scan descendants")
}

func (*senderPreparationCatalogSource) Close() error { return nil }

type senderPreparationHarness struct {
	sender       *PreparedSender
	authority    senderAuthority
	config       SenderConfig
	random       *lockedReader
	dependencies senderPreparationDependencies
}

func newSenderPreparationHarness(t *testing.T) senderPreparationHarness {
	t.Helper()
	identity := func(seed byte) [catalog.IdentityBytes]byte {
		var value [catalog.IdentityBytes]byte
		for index := range value {
			value[index] = seed + byte(index)
		}
		return value
	}

	seed := make([]byte, ed25519.SeedSize)
	seedFirstHalf, seedSecondHalf := identity(1), identity(33)
	copy(seed, seedFirstHalf[:])
	copy(seed[catalog.IdentityBytes:], seedSecondHalf[:])
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	readSecret := identity(65)
	capability, err := link.NewSenderAuthenticated(readSecret[:], publicKey, []string{"ws://127.0.0.1:8484"})
	if err != nil {
		t.Fatal(err)
	}
	shareIDRaw, err := base64.RawURLEncoding.Strict().DecodeString(capability.ShareID)
	if err != nil {
		t.Fatal(err)
	}
	shareBytes := identity(81)
	shareInstance, err := catalog.ShareInstanceFromBytes(shareBytes[:])
	if err != nil {
		t.Fatal(err)
	}
	syntheticRootBytes := identity(97)
	syntheticRoot, err := catalog.DirectoryIDFromBytes(syntheticRootBytes[:])
	if err != nil {
		t.Fatal(err)
	}
	rootGenerationBytes := identity(113)
	rootGeneration, err := catalog.DirectoryGenerationFromBytes(rootGenerationBytes[:])
	if err != nil {
		t.Fatal(err)
	}
	selectedDirectoryBytes := identity(129)
	selectedDirectory, err := catalog.DirectoryIDFromBytes(selectedDirectoryBytes[:])
	if err != nil {
		t.Fatal(err)
	}
	locator, err := catalog.NewLocator(0, "")
	if err != nil {
		t.Fatal(err)
	}
	sourceIdentity, err := catalog.NewSourceIdentity([]byte("sender-preparation-selected-root"))
	if err != nil {
		t.Fatal(err)
	}
	selectedRoot, err := catalog.NewDirectoryNodeRecord(
		selectedDirectory,
		syntheticRoot,
		"selected",
		locator,
		sourceIdentity,
		catalog.ModifiedTime{},
	)
	if err != nil {
		t.Fatal(err)
	}
	keyTree, err := content.NewKeyTree(readSecret[:], shareInstance)
	if err != nil {
		t.Fatal(err)
	}

	sender := &PreparedSender{
		selectedSource: &senderPreparationCatalogSource{selected: []catalog.NodeRecord{selectedRoot}},
		keyTree:        keyTree,
	}
	authority := senderAuthority{
		publicKey:      publicKey,
		privateKey:     privateKey,
		capability:     capability,
		shareIDRaw:     shareIDRaw,
		shareInstance:  shareInstance,
		syntheticRoot:  syntheticRoot,
		rootGeneration: rootGeneration,
	}
	t.Cleanup(func() {
		if err := sender.Close(); err != nil {
			t.Errorf("close partial sender: %v", err)
		}
		authority.destroy()
	})
	return senderPreparationHarness{
		sender:    sender,
		authority: authority,
		config: SenderConfig{
			ChunkSize: catalog.MinChunkSize,
			Now:       func() time.Time { return time.Unix(1_700_000_000, 0) },
			CatalogStorage: CatalogStorageFactoryFunc(func(
				context.Context,
				catalog.ShareInstance,
			) (catalog.CatalogBackend, error) {
				return catalog.NewMemoryCatalogBackend(), nil
			}),
		},
		random:       &lockedReader{reader: mathrand.New(mathrand.NewSource(17))},
		dependencies: productionSenderPreparationDependencies(),
	}
}

func TestPrepareSenderRejectsPartiallyInjectedDependencies(t *testing.T) {
	dependencies := productionSenderPreparationDependencies()
	dependencies.newRecordSealer = nil

	sender, err := PrepareSender(context.Background(), SenderConfig{
		Paths:       []string{"unused"},
		Relays:      []string{"ws://127.0.0.1:8484"},
		preparation: dependencies,
	})
	if sender != nil || err == nil || !strings.Contains(err.Error(), "dependencies are incomplete") {
		t.Fatalf("partial dependency injection result = %v, %v", sender, err)
	}
}

func TestPrepareSenderCatalogRejectsInvalidDependencyAndStateBoundaries(t *testing.T) {
	t.Run("destroyed key tree", func(t *testing.T) {
		harness := newSenderPreparationHarness(t)
		harness.sender.keyTree.Destroy()

		_, err := prepareSenderCatalog(
			context.Background(), harness.config, harness.random, harness.sender,
			harness.authority, nil, harness.dependencies,
		)
		if !errors.Is(err, content.ErrKeyTreeDestroyed) {
			t.Fatalf("destroyed catalog key authority = %v", err)
		}
		if harness.sender.catalogObjects != nil || harness.sender.catalogStore != nil {
			t.Fatal("catalog preparation transferred resources after key derivation failed")
		}
	})

	t.Run("catalog object constructor failure", func(t *testing.T) {
		harness := newSenderPreparationHarness(t)
		injected := errors.New("injected catalog object constructor failure")
		harness.dependencies.newCatalogObjects = func(catalogflow.SealedCatalogStoreConfig) (*catalogflow.SealedCatalogStore, error) {
			return nil, injected
		}

		_, err := prepareSenderCatalog(
			context.Background(), harness.config, harness.random, harness.sender,
			harness.authority, nil, harness.dependencies,
		)
		if !errors.Is(err, injected) {
			t.Fatalf("catalog object constructor failure = %v", err)
		}
		if harness.sender.catalogStore != nil {
			t.Fatal("catalog store was constructed after its page sealer failed")
		}
	})

	t.Run("zero catalog store share identity", func(t *testing.T) {
		harness := newSenderPreparationHarness(t)
		validShare := harness.authority.shareInstance
		constructor := harness.dependencies.newCatalogObjects
		harness.dependencies.newCatalogObjects = func(config catalogflow.SealedCatalogStoreConfig) (*catalogflow.SealedCatalogStore, error) {
			// Preserve a valid sealer so this test reaches the store's own authority
			// boundary instead of failing in the dependency constructor first.
			config.ShareInstance = validShare
			return constructor(config)
		}
		harness.authority.shareInstance = catalog.ShareInstance{}

		_, err := prepareSenderCatalog(
			context.Background(), harness.config, harness.random, harness.sender,
			harness.authority, nil, harness.dependencies,
		)
		if err == nil {
			t.Fatal("a catalog store without share authority was accepted")
		}
		if harness.sender.catalogObjects == nil || harness.sender.catalogStore != nil {
			t.Fatal("store validation failed outside its resource ownership boundary")
		}
	})

	t.Run("zero root generation", func(t *testing.T) {
		harness := newSenderPreparationHarness(t)
		harness.authority.rootGeneration = catalog.DirectoryGeneration{}

		_, err := prepareSenderCatalog(
			context.Background(), harness.config, harness.random, harness.sender,
			harness.authority, nil, harness.dependencies,
		)
		if err == nil {
			t.Fatal("a synthetic root without a generation was accepted")
		}
		if harness.sender.catalogStore == nil || harness.sender.catalogAccess != nil {
			t.Fatal("synthetic-root validation failed outside its ownership boundary")
		}
	})

	t.Run("cancelled root commit", func(t *testing.T) {
		harness := newSenderPreparationHarness(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := prepareSenderCatalog(
			ctx, harness.config, harness.random, harness.sender,
			harness.authority, nil, harness.dependencies,
		)
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("cancelled root commit = %v", err)
		}
		if harness.sender.catalogStore == nil || harness.sender.catalogAccess != nil {
			t.Fatal("cancelled commit escaped its catalog-store ownership boundary")
		}
	})

	t.Run("invalid descriptor authority", func(t *testing.T) {
		harness := newSenderPreparationHarness(t)
		harness.authority.capability.PKHash = nil

		_, err := prepareSenderCatalog(
			context.Background(), harness.config, harness.random, harness.sender,
			harness.authority, nil, harness.dependencies,
		)
		if err == nil || !strings.Contains(err.Error(), "descriptor key") {
			t.Fatalf("invalid descriptor authority = %v", err)
		}
		if harness.sender.catalogStore == nil || harness.sender.catalogAccess != nil {
			t.Fatal("descriptor sealing failure escaped its catalog ownership boundary")
		}
	})
}

type senderPreparationErrorReader struct{ err error }

func (reader senderPreparationErrorReader) Read([]byte) (int, error) { return 0, reader.err }

func TestSealPreparedDescriptorPropagatesNonceFailure(t *testing.T) {
	harness := newSenderPreparationHarness(t)
	injected := errors.New("injected descriptor nonce failure")

	object, err := sealPreparedDescriptor(
		senderPreparationErrorReader{err: injected},
		harness.sender.keyTree,
		harness.authority,
		catalog.ShareDescriptor{},
	)
	if object != nil || !errors.Is(err, injected) {
		t.Fatalf("descriptor nonce failure = %x, %v", object, err)
	}
}

func TestPrepareSenderContentRejectsZeroShareAuthority(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "selected.bin")
	if err := os.WriteFile(filename, []byte("zero-share-content-boundary"), 0o600); err != nil {
		t.Fatal(err)
	}
	config := SenderConfig{
		Paths: []string{filename}, Relays: []string{"ws://127.0.0.1:8484"}, ChunkSize: catalog.MinChunkSize,
		Now: func() time.Time { return time.Unix(1_700_000_000, 0) },
	}
	random := &lockedReader{reader: mathrand.New(mathrand.NewSource(29))}
	sender := &PreparedSender{random: random}
	t.Cleanup(func() {
		if err := sender.Close(); err != nil {
			t.Errorf("close partial sender: %v", err)
		}
	})
	dependencies := productionSenderPreparationDependencies()
	authority, err := prepareSenderAuthority(config, random, sender)
	if err != nil {
		t.Fatal(err)
	}
	defer authority.destroy()
	if _, err := prepareSenderCatalog(
		context.Background(), config, random, sender, authority, nil, dependencies,
	); err != nil {
		t.Fatal(err)
	}
	// Preserve every earlier-stage resource so only the content stage's share
	// identity guard decides this fail-closed boundary.
	authority.shareInstance = catalog.ShareInstance{}

	err = prepareSenderContent(config, random, sender, authority, dependencies)
	if err == nil {
		t.Fatal("content preparation accepted a zero share authority")
	}
	if sender.catalogStore == nil || sender.revisionSource == nil {
		t.Fatal("zero-share test did not retain valid prior-stage resources")
	}
	if sender.revisionStore != nil || sender.recordSealer != nil || sender.cache != nil {
		t.Fatal("content preparation transferred resources after revision-store validation failed")
	}
}

func TestPrepareSenderRollsBackRecordSealerConstructorFailure(t *testing.T) {
	filename := filepath.Join(t.TempDir(), "selected.bin")
	if err := os.WriteFile(filename, []byte("record-sealer-constructor"), 0o600); err != nil {
		t.Fatal(err)
	}
	dependencies := productionSenderPreparationDependencies()
	injected := errors.New("injected record sealer constructor failure")
	dependencies.newRecordSealer = func(records.SealerConfig) (*records.Sealer, error) {
		return nil, injected
	}

	sender, err := PrepareSender(context.Background(), SenderConfig{
		Paths:       []string{filename},
		Relays:      []string{"ws://127.0.0.1:8484"},
		ChunkSize:   catalog.MinChunkSize,
		Random:      mathrand.New(mathrand.NewSource(23)),
		preparation: dependencies,
	})
	if sender != nil || !errors.Is(err, injected) {
		t.Fatalf("record sealer constructor result = %v, %v", sender, err)
	}
}
