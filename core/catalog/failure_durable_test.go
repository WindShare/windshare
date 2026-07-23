package catalog

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type failureSealerProbe struct {
	calls    atomic.Int32
	delegate FailureSealer
	err      error
}

func (p *failureSealerProbe) SealFailure(record DirectoryFailureRecord) (SealedFailureObject, error) {
	p.calls.Add(1)
	if p.err != nil {
		return SealedFailureObject{}, p.err
	}
	return p.delegate.SealFailure(record)
}

func openFailureStore(
	t *testing.T,
	root string,
	process, share *BudgetAccount,
	clock Clock,
	attempts ScanAttemptIDGenerator,
	generations DirectoryGenerationGenerator,
	sealer FailureSealer,
	faults FileBackendFaults,
) (*CatalogStore, *FileCatalogBackend) {
	t.Helper()
	backend, err := NewFileCatalogBackend(FileCatalogBackendConfig{
		Root: root, ShareInstance: idValue[ShareInstance](1), Faults: faults,
	})
	if err != nil {
		t.Fatal(err)
	}
	if sealer == nil {
		sealer = semanticTestCommitter{}
	}
	store, err := NewCatalogStore(StoreConfig{
		ShareInstance: idValue[ShareInstance](1), Backend: backend,
		ProcessBudget: process, ShareBudget: share, PageSealer: semanticTestCommitter{},
		FailureSealer: sealer, SpillFactory: NewFileSpillFactory(filepath.Join(root, "sort")),
		Clock: clock, AttemptIDs: attempts, Generations: generations,
	})
	if err != nil {
		_ = backend.Close()
		t.Fatal(err)
	}
	return store, backend
}

func TestPermanentFailureReplaysExactObjectAndBudgetAfterRestart(t *testing.T) {
	rootPath := t.TempDir()
	process := generousBudget(t, "process")
	share := generousBudget(t, "share")
	session := generousBudget(t, "session")
	store, backend := fileStore(t, rootPath, idValue[ShareInstance](1), process, share, nil, 10, 20)
	directory := prepareScannableDirectory(t, store, session, 30, 32)
	var scans atomic.Int32
	scanner := DirectoryScannerFunc(func(context.Context, ScanRequest) (ScanResult, error) {
		scans.Add(1)
		return ScanResult{}, NewPermanentScanError(errors.New("permission denied"))
	})
	_, scanErr := store.ListChildren(context.Background(), directory, session, ScanOptions{}, scanner)
	var first *DirectoryFailure
	if !errors.As(scanErr, &first) || first.Kind != FailureKindPermanentIO || scans.Load() != 1 {
		t.Fatalf("first failure = %v scans=%d", scanErr, scans.Load())
	}
	firstObject, found, err := store.FailureObject(context.Background(), directory, first.AttemptID)
	if err != nil || !found {
		t.Fatalf("first failure object: found=%v err=%v", found, err)
	}
	wantUsage := share.Snapshot().Used
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}

	recoveredProcess := generousBudget(t, "recovered-process")
	recoveredShare := generousBudget(t, "recovered-share")
	recoveredSession := generousBudget(t, "recovered-session")
	sealProbe := &failureSealerProbe{err: errors.New("replay attempted to reseal")}
	recovered, _ := openFailureStore(
		t, rootPath, recoveredProcess, recoveredShare, nil, nil, nil, sealProbe, nil,
	)
	defer recovered.Close()
	defer store.Close()
	never := DirectoryScannerFunc(func(context.Context, ScanRequest) (ScanResult, error) {
		t.Fatal("permanent failure replay reached scanner")
		return ScanResult{}, nil
	})
	_, replayErr := recovered.ListChildren(context.Background(), directory, recoveredSession, ScanOptions{Retry: true}, never)
	var replay *DirectoryFailure
	if !errors.As(replayErr, &replay) || replay.AttemptID != first.AttemptID || replay.Kind != first.Kind {
		t.Fatalf("replayed failure = %v", replayErr)
	}
	replayedObject, found, err := recovered.FailureObject(context.Background(), directory, replay.AttemptID)
	if err != nil || !found || !bytes.Equal(firstObject.Bytes(), replayedObject.Bytes()) {
		t.Fatalf("replayed object changed: found=%v err=%v", found, err)
	}
	if sealProbe.calls.Load() != 0 {
		t.Fatalf("replay resealed failure %d times", sealProbe.calls.Load())
	}
	if got := recoveredShare.Snapshot().Used; got != wantUsage {
		t.Fatalf("recovered failure usage = %+v want %+v", got, wantUsage)
	}
	if recoveredSession.Snapshot().Used != (ResourceUsage{}) {
		t.Fatalf("replay charged session budget: %+v", recoveredSession.Snapshot().Used)
	}
}

func TestTransientFailureCooldownAndAttemptChainSurviveRestarts(t *testing.T) {
	const cooldown = 2 * time.Second
	rootPath := t.TempDir()
	clockA := &fakeClock{now: time.Unix(100, 0)}
	attemptA := idValue[ScanAttemptID](41)
	generationA := idValue[DirectoryGeneration](42)
	store, backend := openFailureStore(
		t, rootPath, generousBudget(t, "process-a"), generousBudget(t, "share-a"), clockA,
		ScanAttemptIDGeneratorFunc(func() (ScanAttemptID, error) { return attemptA, nil }),
		DirectoryGenerationGeneratorFunc(func() (DirectoryGeneration, error) { return generationA, nil }),
		semanticTestCommitter{}, nil,
	)
	sessionA := generousBudget(t, "session-a")
	directory := prepareScannableDirectory(t, store, sessionA, 43, 45)
	transient := DirectoryScannerFunc(func(context.Context, ScanRequest) (ScanResult, error) {
		return ScanResult{}, NewTransientScanError(errors.New("temporarily unavailable"), cooldown)
	})
	_, firstErr := store.ListChildren(context.Background(), directory, sessionA, ScanOptions{}, transient)
	var first *DirectoryFailure
	if !errors.As(firstErr, &first) || first.AttemptID != attemptA {
		t.Fatalf("first transient failure = %v", firstErr)
	}
	objectA, found, err := store.FailureObject(context.Background(), directory, attemptA)
	if err != nil || !found {
		t.Fatalf("attempt A object: found=%v err=%v", found, err)
	}
	if err := backend.Close(); err != nil {
		t.Fatal(err)
	}

	clockB := &fakeClock{now: time.Unix(200, 0)}
	attemptB := idValue[ScanAttemptID](46)
	generationB := idValue[DirectoryGeneration](47)
	sealB := &failureSealerProbe{delegate: semanticTestCommitter{}}
	recoveredB, backendB := openFailureStore(
		t, rootPath, generousBudget(t, "process-b"), generousBudget(t, "share-b"), clockB,
		ScanAttemptIDGeneratorFunc(func() (ScanAttemptID, error) { return attemptB, nil }),
		DirectoryGenerationGeneratorFunc(func() (DirectoryGeneration, error) { return generationB, nil }), sealB, nil,
	)
	sessionB := generousBudget(t, "session-b")
	var scansB atomic.Int32
	scannerB := DirectoryScannerFunc(func(context.Context, ScanRequest) (ScanResult, error) {
		scansB.Add(1)
		return ScanResult{}, NewTransientScanError(errors.New("still unavailable"), cooldown)
	})
	_, cooldownErr := recoveredB.ListChildren(context.Background(), directory, sessionB, ScanOptions{Retry: true}, scannerB)
	var cooldownFailure *DirectoryFailure
	if !errors.As(cooldownErr, &cooldownFailure) || cooldownFailure.AttemptID != attemptA || scansB.Load() != 0 {
		t.Fatalf("cooldown replay = %v scans=%d", cooldownErr, scansB.Load())
	}
	clockB.Advance(cooldown)
	_, implicitErr := recoveredB.ListChildren(context.Background(), directory, sessionB, ScanOptions{}, scannerB)
	var implicit *DirectoryFailure
	if !errors.As(implicitErr, &implicit) || implicit.AttemptID != attemptA || scansB.Load() != 0 {
		t.Fatalf("implicit retry replaced authority: %v scans=%d", implicitErr, scansB.Load())
	}
	_, secondErr := recoveredB.ListChildren(context.Background(), directory, sessionB, ScanOptions{Retry: true}, scannerB)
	var second *DirectoryFailure
	if !errors.As(secondErr, &second) || second.AttemptID != attemptB || scansB.Load() != 1 || sealB.calls.Load() != 1 {
		t.Fatalf("explicit retry = %v scans=%d seals=%d", secondErr, scansB.Load(), sealB.calls.Load())
	}
	objectB, found, err := recoveredB.FailureObject(context.Background(), directory, attemptB)
	if err != nil || !found {
		t.Fatalf("attempt B object: found=%v err=%v", found, err)
	}
	if err := backendB.Close(); err != nil {
		t.Fatal(err)
	}

	clockC := &fakeClock{now: time.Unix(300, 0)}
	sealC := &failureSealerProbe{err: errors.New("rejected retry attempted to seal")}
	recoveredC, _ := openFailureStore(
		t, rootPath, generousBudget(t, "process-c"), generousBudget(t, "share-c"), clockC,
		ScanAttemptIDGeneratorFunc(func() (ScanAttemptID, error) { return attemptA, nil }),
		DirectoryGenerationGeneratorFunc(func() (DirectoryGeneration, error) {
			return idValue[DirectoryGeneration](48), nil
		}), sealC, nil,
	)
	defer recoveredC.Close()
	defer recoveredB.Close()
	defer store.Close()
	for attempt, want := range map[ScanAttemptID][]byte{attemptA: objectA.Bytes(), attemptB: objectB.Bytes()} {
		object, found, err := recoveredC.FailureObject(context.Background(), directory, attempt)
		if err != nil || !found || !bytes.Equal(object.Bytes(), want) {
			t.Fatalf("recovered attempt %x changed: found=%v err=%v", attempt, found, err)
		}
	}
	clockC.Advance(cooldown)
	never := DirectoryScannerFunc(func(context.Context, ScanRequest) (ScanResult, error) {
		t.Fatal("A-B-A reuse reached scanner")
		return ScanResult{}, nil
	})
	if _, err := recoveredC.ListChildren(context.Background(), directory, generousBudget(t, "session-c"), ScanOptions{Retry: true}, never); err == nil {
		t.Fatal("A-B-A attempt reuse was accepted")
	}
	_, activeErr := recoveredC.ListChildren(context.Background(), directory, generousBudget(t, "session-c-replay"), ScanOptions{}, never)
	var active *DirectoryFailure
	if !errors.As(activeErr, &active) || active.AttemptID != attemptB || sealC.calls.Load() != 0 {
		t.Fatalf("rejected reuse discarded B authority: %v seals=%d", activeErr, sealC.calls.Load())
	}
}

func TestFailurePersistenceFaultsNeverPublishHalfAuthority(t *testing.T) {
	for _, test := range []struct {
		point FileBackendFaultPoint
		err   error
	}{
		{point: FileFaultStageFailure, err: syscall.ENOSPC},
		{point: FileFaultPublishFailure, err: syscall.EIO},
	} {
		t.Run(string(test.point), func(t *testing.T) {
			rootPath := t.TempDir()
			faults := &mutableFileFaults{}
			process := generousBudget(t, "process")
			share := generousBudget(t, "share")
			session := generousBudget(t, "session")
			store, backend := fileStore(t, rootPath, idValue[ShareInstance](1), process, share, faults, 50, 51)
			directory := prepareScannableDirectory(t, store, session, 52, 54)
			baseline := share.Snapshot().Used
			faults.set(test.point, test.err)
			scanner := DirectoryScannerFunc(func(context.Context, ScanRequest) (ScanResult, error) {
				return ScanResult{}, NewPermanentScanError(errors.New("scan failed"))
			})
			_, scanErr := store.ListChildren(context.Background(), directory, session, ScanOptions{}, scanner)
			var published *DirectoryFailure
			if !errors.Is(scanErr, test.err) || errors.As(scanErr, &published) {
				t.Fatalf("failure persistence fault = %v", scanErr)
			}
			for _, name := range []string{"transactions", "failures"} {
				entries, err := os.ReadDir(filepath.Join(rootPath, name))
				if err != nil || len(entries) != 0 {
					t.Fatalf("%s retained half authority: entries=%d err=%v", name, len(entries), err)
				}
			}
			want, _ := addUsage(baseline, ResourceUsage{MemoryBytes: ScanAttemptLedgerBytes})
			if share.Snapshot().Used != want || session.Snapshot().Used != (ResourceUsage{}) {
				t.Fatalf("fault budget = share %+v session %+v", share.Snapshot().Used, session.Snapshot().Used)
			}
			if err := backend.Close(); err != nil {
				t.Fatal(err)
			}
			emptyDirectory := filepath.Join(rootPath, "failures", string(bytes.Repeat([]byte{'1'}, IdentityBytes*2)))
			if err := os.Mkdir(emptyDirectory, 0o700); err != nil {
				t.Fatal(err)
			}
			recovered, _ := fileStore(
				t, rootPath, idValue[ShareInstance](1), generousBudget(t, "rp"), generousBudget(t, "rs"), nil, 50, 51,
			)
			entries, err := os.ReadDir(filepath.Join(rootPath, "failures"))
			if err != nil || len(entries) != 0 {
				t.Fatalf("recovery retained empty crash directory: entries=%d err=%v", len(entries), err)
			}
			if _, err := recovered.ListChildren(context.Background(), directory, generousBudget(t, "recovered-session"), ScanOptions{}, DirectoryScannerFunc(
				func(context.Context, ScanRequest) (ScanResult, error) { return ScanResult{}, nil },
			)); err != nil {
				t.Fatalf("unpublished failure blocked recovery scan: %v", err)
			}
			_ = recovered.Close()
			_ = store.Close()
		})
	}
}

func TestFailureRecoveryRejectsCorruptObjectsMetadataAndChains(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(t *testing.T, root, failurePath string)
	}{
		{name: "object", mutate: func(t *testing.T, _, failurePath string) {
			path := filepath.Join(failurePath, failureObjectName)
			encoded, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			encoded[len(encoded)-1] ^= 0xff
			if err := os.WriteFile(path, encoded, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "metadata checksum", mutate: func(t *testing.T, _, failurePath string) {
			path := filepath.Join(failurePath, failureMetaName)
			encoded, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			encoded[88] ^= 1
			if err := os.WriteFile(path, encoded, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "missing object", mutate: func(t *testing.T, _, failurePath string) {
			if err := os.Remove(filepath.Join(failurePath, failureObjectName)); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "broken predecessor", mutate: func(t *testing.T, _, failurePath string) {
			path := filepath.Join(failurePath, failureMetaName)
			encoded, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			copy(encoded[72:88], idValue[ScanAttemptID](99).Bytes())
			checksum := sha256.Sum256(encoded[:failureMetaBodyBytes])
			copy(encoded[failureMetaBodyBytes:], checksum[:])
			if err := os.WriteFile(path, encoded, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "unexpected file", mutate: func(t *testing.T, _, failurePath string) {
			if err := os.WriteFile(filepath.Join(failurePath, "unexpected"), []byte{1}, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "invalid directory identity", mutate: func(t *testing.T, root, _ string) {
			if err := os.Mkdir(filepath.Join(root, "failures", "invalid"), 0o700); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			rootPath := t.TempDir()
			store, backend := fileStore(
				t, rootPath, idValue[ShareInstance](1), generousBudget(t, "process"), generousBudget(t, "share"), nil, 60, 61,
			)
			directory := prepareScannableDirectory(t, store, generousBudget(t, "session"), 62, 64)
			_, scanErr := store.ListChildren(context.Background(), directory, generousBudget(t, "scan-session"), ScanOptions{}, DirectoryScannerFunc(
				func(context.Context, ScanRequest) (ScanResult, error) {
					return ScanResult{}, NewPermanentScanError(errors.New("failed"))
				},
			))
			var failure *DirectoryFailure
			if !errors.As(scanErr, &failure) {
				t.Fatalf("failure setup = %v", scanErr)
			}
			failurePath := backend.failurePath(directory, failure.AttemptID)
			if err := backend.Close(); err != nil {
				t.Fatal(err)
			}
			test.mutate(t, rootPath, failurePath)
			corruptBackend, err := NewFileCatalogBackend(FileCatalogBackendConfig{
				Root: rootPath, ShareInstance: idValue[ShareInstance](1),
			})
			if err != nil {
				t.Fatal(err)
			}
			_, err = NewCatalogStore(StoreConfig{
				ShareInstance: idValue[ShareInstance](1), Backend: corruptBackend,
				ProcessBudget: generousBudget(t, "rp"), ShareBudget: generousBudget(t, "rs"),
				PageSealer: semanticTestCommitter{},
			})
			if !errors.Is(err, ErrCorruptCatalogStorage) {
				t.Fatalf("corrupt failure authority was accepted: %v", err)
			}
			_ = corruptBackend.Close()
			_ = store.Close()
		})
	}
}
