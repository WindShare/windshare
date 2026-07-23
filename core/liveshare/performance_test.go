package liveshare

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/link"
)

var r8ReadyDescendantScales = [...]uint64{0, 1_000, 10_000, 100_000, 1_000_000}

// Full sender setup includes crypto/runtime bookkeeping whose allocation count
// can move by a few events between identical runs. A constant envelope across
// five orders of virtual width still rejects descendant-proportional work while
// avoiding a false gate on allocator scheduling.
const r8ReadyAllocationEventEnvelope = 4

type r8VirtualCatalogSource struct {
	selected    []catalog.NodeRecord
	descendants uint64
	readDirOps  atomic.Uint64
	statOps     atomic.Uint64
	openOps     atomic.Uint64
}

type r8UnusedSpillFactory struct{}

func (r8UnusedSpillFactory) NewWorkspace(context.Context, catalog.SpillRequest) (catalog.SpillWorkspace, error) {
	return nil, errors.New("ready path requested post-ready catalog spill storage")
}

func (source *r8VirtualCatalogSource) SelectedRoots() []catalog.NodeRecord {
	return append([]catalog.NodeRecord(nil), source.selected...)
}

func (source *r8VirtualCatalogSource) ScanDirectory(context.Context, catalog.ScanRequest) (catalog.ScanResult, error) {
	// Charging the full virtual tree at this boundary makes any accidental
	// pre-ready traversal visible without materializing a million host files.
	source.readDirOps.Add(1)
	source.statOps.Add(source.descendants)
	source.openOps.Add(source.descendants)
	return catalog.ScanResult{}, errors.New("virtual descendants were enumerated before the ready boundary")
}

func (source *r8VirtualCatalogSource) Close() error { return nil }

func (source *r8VirtualCatalogSource) descendantFSOps() uint64 {
	return source.readDirOps.Load() + source.statOps.Load() + source.openOps.Load()
}

type r8ReadyMeasurement struct {
	registrationMaterialBytes uint64
	descriptorBytes           uint64
	descendantFSOps           uint64
}

type r8DeterministicReader struct{ next uint64 }

func (reader *r8DeterministicReader) Read(destination []byte) (int, error) {
	for index := range destination {
		destination[index] = byte((reader.next+uint64(index))%251) + 1
	}
	reader.next += uint64(len(destination))
	return len(destination), nil
}

func r8Identity[T ~[catalog.IdentityBytes]byte](seed byte) T {
	var identity T
	for index := range identity {
		identity[index] = seed + byte(index)
	}
	return identity
}

func r8VirtualReadySource(descendants uint64) (*r8VirtualCatalogSource, error) {
	synthetic := r8Identity[catalog.DirectoryID](31)
	directory := r8Identity[catalog.DirectoryID](63)
	locator, err := catalog.NewLocator(0, "")
	if err != nil {
		return nil, err
	}
	identity, err := catalog.NewSourceIdentity([]byte("r8-selected-root"))
	if err != nil {
		return nil, err
	}
	record, err := catalog.NewDirectoryNodeRecord(
		directory, synthetic, "selected", locator, identity, catalog.ModifiedTime{},
	)
	if err != nil {
		return nil, err
	}
	return &r8VirtualCatalogSource{selected: []catalog.NodeRecord{record}, descendants: descendants}, nil
}

func r8PrepareVirtualReady(descendants uint64) (r8ReadyMeasurement, func() error, error) {
	source, err := r8VirtualReadySource(descendants)
	if err != nil {
		return r8ReadyMeasurement{}, nil, err
	}
	seed := make([]byte, ed25519.SeedSize)
	for index := range seed {
		seed[index] = byte(index + 1)
	}
	privateKey := ed25519.NewKeyFromSeed(seed)
	publicKey := privateKey.Public().(ed25519.PublicKey)
	readSecret := make([]byte, link.ReadSecretBytes)
	for index := range readSecret {
		readSecret[index] = byte(index + 17)
	}
	capability, err := link.NewSenderAuthenticated(readSecret, publicKey, []string{"ws://127.0.0.1:8484"})
	if err != nil {
		return r8ReadyMeasurement{}, nil, err
	}
	shareID, err := base64.RawURLEncoding.Strict().DecodeString(capability.ShareID)
	if err != nil {
		return r8ReadyMeasurement{}, nil, err
	}
	authority := senderAuthority{
		publicKey: publicKey, privateKey: privateKey, capability: capability, shareIDRaw: shareID,
		shareInstance:  r8Identity[catalog.ShareInstance](15),
		syntheticRoot:  r8Identity[catalog.DirectoryID](31),
		rootGeneration: r8Identity[catalog.DirectoryGeneration](47),
	}
	sender := &PreparedSender{selectedSource: source}
	sender.keyTree, err = content.NewKeyTree(readSecret, authority.shareInstance)
	if err != nil {
		authority.destroy()
		return r8ReadyMeasurement{}, nil, err
	}
	random := &lockedReader{reader: &r8DeterministicReader{}}
	catalogState, err := prepareSenderCatalog(context.Background(), SenderConfig{
		ChunkSize: catalog.DefaultChunkSize,
		Now:       func() time.Time { return time.Unix(1_700_000_000, 0) },
	}, random, sender, authority, r8UnusedSpillFactory{}, productionSenderPreparationDependencies())
	if err != nil {
		authority.destroy()
		return r8ReadyMeasurement{}, nil, errors.Join(err, sender.Close())
	}
	commitSenderPreparation(sender, authority, catalogState)
	authority.destroy()
	if err := sender.AuthorizeRegistration(); err != nil {
		return r8ReadyMeasurement{}, nil, errors.Join(err, sender.Close())
	}
	material := sender.Registration()
	measurement := r8ReadyMeasurement{
		// This is the local input material handed to the transport. Production
		// wire bytes also include protocol framing, challenge proof, and ACK, so
		// they are measured separately at the relayv2 transport boundary.
		registrationMaterialBytes: uint64(len(material.ShareID) + len(material.ShareInstance) + len(material.PKHash) + len(material.Descriptor)),
		descriptorBytes:           uint64(len(material.Descriptor)),
		descendantFSOps:           source.descendantFSOps(),
	}
	return measurement, sender.Close, nil
}

func TestR8V2ReadyScalingBudgets(t *testing.T) {
	const allocationSamples = 3
	var baselineRegistrationMaterial uint64
	var minimumAllocations float64
	var maximumAllocations float64
	for scaleIndex, descendants := range r8ReadyDescendantScales {
		t.Run(fmt.Sprintf("descendants=%07d", descendants), func(t *testing.T) {
			measurement, closeReady, err := r8PrepareVirtualReady(descendants)
			if err != nil {
				t.Fatal(err)
			}
			if err := closeReady(); err != nil {
				t.Fatal(err)
			}
			if measurement.descendantFSOps != 0 {
				t.Fatalf("ready path performed %d descendant filesystem operations", measurement.descendantFSOps)
			}
			if measurement.descriptorBytes == 0 || measurement.descriptorBytes > catalog.MaxDescriptorObjectBytes {
				t.Fatalf("descriptor budget = %d", measurement.descriptorBytes)
			}

			if scaleIndex == 0 {
				baselineRegistrationMaterial = measurement.registrationMaterialBytes
			}
			if measurement.registrationMaterialBytes != baselineRegistrationMaterial {
				t.Fatalf("registration material grew with virtual descendants: got %d, baseline %d", measurement.registrationMaterialBytes, baselineRegistrationMaterial)
			}
			// Race instrumentation allocates synchronization metadata according to
			// scheduling, so AllocsPerRun no longer measures ready-path scaling.
			if raceDetectorEnabled {
				return
			}
			var allocationErr error
			allocations := testing.AllocsPerRun(allocationSamples, func() {
				measured, cleanup, err := r8PrepareVirtualReady(descendants)
				if err == nil && measured.descendantFSOps != 0 {
					err = fmt.Errorf("ready path performed %d descendant filesystem operations", measured.descendantFSOps)
				}
				if cleanup != nil {
					err = errors.Join(err, cleanup())
				}
				allocationErr = errors.Join(allocationErr, err)
			})
			if allocationErr != nil {
				t.Fatal(allocationErr)
			}
			if scaleIndex == 0 {
				minimumAllocations = allocations
				maximumAllocations = allocations
			}
			minimumAllocations = min(minimumAllocations, allocations)
			maximumAllocations = max(maximumAllocations, allocations)
			if maximumAllocations-minimumAllocations > r8ReadyAllocationEventEnvelope {
				t.Fatalf("ready allocation envelope grew with virtual descendants: min %.0f max %.0f", minimumAllocations, maximumAllocations)
			}
		})
	}
}

func BenchmarkR8V2ReadyScaling(b *testing.B) {
	for _, descendants := range r8ReadyDescendantScales {
		b.Run(fmt.Sprintf("descendants=%07d", descendants), func(b *testing.B) {
			b.ReportAllocs()
			var last r8ReadyMeasurement
			b.ResetTimer()
			for range b.N {
				measurement, cleanup, err := r8PrepareVirtualReady(descendants)
				if err != nil {
					b.Fatal(err)
				}
				last = measurement
				b.StopTimer()
				if err := cleanup(); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
			if last.descendantFSOps != 0 {
				b.Fatalf("ready path performed %d descendant filesystem operations", last.descendantFSOps)
			}
			b.ReportMetric(float64(descendants), "virtual-descendants")
			b.ReportMetric(float64(last.descendantFSOps), "descendant-fs-ops/op")
			b.ReportMetric(float64(last.registrationMaterialBytes), "registration-material-bytes/op")
			b.ReportMetric(float64(last.descriptorBytes), "descriptor-bytes/op")
		})
	}
}

func BenchmarkR8V2ReadyRealDisk(b *testing.B) {
	base := b.TempDir()
	run := func(b *testing.B, rootForIteration func(int) (string, func(), error)) {
		b.ReportAllocs()
		var registrationMaterialBytes int
		b.ResetTimer()
		for iteration := range b.N {
			b.StopTimer()
			root, cleanupRoot, err := rootForIteration(iteration)
			if err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
			sender, err := PrepareSender(context.Background(), SenderConfig{
				Paths: []string{root}, Relays: []string{"ws://127.0.0.1:8484"}, ChunkSize: catalog.DefaultChunkSize,
				Random: &r8DeterministicReader{}, Now: func() time.Time { return time.Unix(1_700_000_000, 0) },
			})
			if err == nil {
				err = sender.AuthorizeRegistration()
			}
			if err == nil {
				material := sender.Registration()
				registrationMaterialBytes = len(material.ShareID) + len(material.ShareInstance) + len(material.PKHash) + len(material.Descriptor)
			}
			b.StopTimer()
			if sender != nil {
				err = errors.Join(err, sender.Close())
			}
			cleanupRoot()
			if err != nil {
				b.Fatal(err)
			}
			b.StartTimer()
		}
		b.ReportMetric(float64(registrationMaterialBytes), "registration-material-bytes/op")
	}

	b.Run("path_state=fresh", func(b *testing.B) {
		run(b, func(iteration int) (string, func(), error) {
			root := filepath.Join(base, fmt.Sprintf("fresh-%d", iteration))
			if err := os.MkdirAll(filepath.Join(root, "nested"), 0o700); err != nil {
				return "", func() {}, err
			}
			if err := os.WriteFile(filepath.Join(root, "nested", "descendant.bin"), []byte("ready does not read me"), 0o600); err != nil {
				return "", func() { _ = os.RemoveAll(root) }, err
			}
			return root, func() { _ = os.RemoveAll(root) }, nil
		})
	})
	b.Run("path_state=reused", func(b *testing.B) {
		root := filepath.Join(base, "reused")
		if err := os.MkdirAll(filepath.Join(root, "nested"), 0o700); err != nil {
			b.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(root, "nested", "descendant.bin"), []byte("ready does not read me"), 0o600); err != nil {
			b.Fatal(err)
		}
		run(b, func(int) (string, func(), error) { return root, func() {}, nil })
	})
}
