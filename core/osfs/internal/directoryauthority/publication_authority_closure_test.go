package directoryauthority

import (
	"context"
	"errors"
	"io/fs"
	"sync"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/osfs/internal/resumeauthority"
)

type publicationFaultPlan struct {
	mu sync.Mutex

	classifyErr   error
	openEntryErr  error
	openEntryNil  bool
	openPinnedErr error
	openPinnedNil bool
	openFileErr   error
	openFileNil   bool

	entryMatchesCalls   int
	entryMatchesErrAt   int
	entryMatchesErr     error
	entryMatchesFalseAt int
	entryMatchesHookAt  int
	entryMatchesHook    func()

	wrapFile bool
	file     *publicationFaultFilePolicy
}

func newPublicationFaultPlan() *publicationFaultPlan {
	return &publicationFaultPlan{file: &publicationFaultFilePolicy{}}
}

type publicationFaultPlatform struct {
	*fakePlatform
	plan       *publicationFaultPlan
	rootNil    bool
	afterGuard func()
}

func newPublicationFaultPlatform() *publicationFaultPlatform {
	return &publicationFaultPlatform{
		fakePlatform: newFakePlatform(outputcap.CallerProvidedContainer),
		plan:         newPublicationFaultPlan(),
	}
}

func (platform *publicationFaultPlatform) AcquirePublicOperationGuard() (outputcap.PublicOperationGuard, error) {
	guard, err := platform.fakePlatform.AcquirePublicOperationGuard()
	if err != nil {
		return nil, err
	}
	if platform.afterGuard != nil {
		platform.afterGuard()
	}
	if platform.rootNil {
		return &publicationFaultGuard{PublicOperationGuard: guard}, nil
	}
	root, ok := guard.Root().(*fakeDirectory)
	if !ok || root == nil {
		return &publicationFaultGuard{PublicOperationGuard: guard}, nil
	}
	return &publicationFaultGuard{
		PublicOperationGuard: guard,
		root: &publicationFaultDirectory{
			Directory: root,
			base:      root,
			platform:  platform,
		},
	}, nil
}

type publicationFaultGuard struct {
	outputcap.PublicOperationGuard
	root outputcap.Directory
}

func (guard *publicationFaultGuard) Root() outputcap.Directory {
	if guard == nil {
		return nil
	}
	return guard.root
}

type publicationFaultDirectory struct {
	outputcap.Directory
	base     *fakeDirectory
	platform *publicationFaultPlatform
}

func (directory *publicationFaultDirectory) ClassifyExactEntry(
	name string,
) (outputcap.EntryKind, bool, error) {
	directory.platform.plan.mu.Lock()
	err := directory.platform.plan.classifyErr
	directory.platform.plan.mu.Unlock()
	if err != nil {
		return outputcap.EntryAbsent, false, err
	}
	return directory.base.ClassifyExactEntry(name)
}

func (directory *publicationFaultDirectory) OpenEntry(
	name string,
) (outputcap.CurrentEntryReference, error) {
	directory.platform.plan.mu.Lock()
	err := directory.platform.plan.openEntryErr
	returnNil := directory.platform.plan.openEntryNil
	directory.platform.plan.mu.Unlock()
	if err != nil || returnNil {
		return nil, err
	}
	reference, err := directory.base.OpenEntry(name)
	if err != nil || reference == nil {
		return nil, err
	}
	return &publicationFaultReference{CurrentEntryReference: reference}, nil
}

func (directory *publicationFaultDirectory) EntryMatches(
	name string,
	expected outputcap.CurrentEntryReference,
) (bool, error) {
	reference := expected
	if wrapped, ok := expected.(*publicationFaultReference); ok {
		reference = wrapped.CurrentEntryReference
	}
	matches, err := directory.base.EntryMatches(name, reference)
	if err != nil {
		return false, err
	}
	directory.platform.plan.mu.Lock()
	plan := directory.platform.plan
	plan.entryMatchesCalls++
	call := plan.entryMatchesCalls
	faultErr := plan.entryMatchesErr
	faultErrAt := plan.entryMatchesErrAt
	falseAt := plan.entryMatchesFalseAt
	hookAt := plan.entryMatchesHookAt
	hook := plan.entryMatchesHook
	plan.mu.Unlock()
	if call == hookAt && hook != nil {
		hook()
	}
	if call == faultErrAt {
		return false, faultErr
	}
	if call == falseAt {
		return false, nil
	}
	return matches, nil
}

func (directory *publicationFaultDirectory) OpenPinnedDirectory(
	expected outputcap.CurrentEntryReference,
	private bool,
) (outputcap.Directory, error) {
	directory.platform.plan.mu.Lock()
	err := directory.platform.plan.openPinnedErr
	returnNil := directory.platform.plan.openPinnedNil
	directory.platform.plan.mu.Unlock()
	if err != nil || returnNil {
		return nil, err
	}
	reference := expected
	if wrapped, ok := expected.(*publicationFaultReference); ok {
		reference = wrapped.CurrentEntryReference
	}
	opened, err := directory.base.OpenPinnedDirectory(reference, private)
	if err != nil || opened == nil {
		return nil, err
	}
	base := opened.(*fakeDirectory)
	return &publicationFaultDirectory{Directory: base, base: base, platform: directory.platform}, nil
}

func (directory *publicationFaultDirectory) OpenFile(
	name string,
	private bool,
	writable bool,
) (outputcap.File, error) {
	directory.platform.plan.mu.Lock()
	err := directory.platform.plan.openFileErr
	returnNil := directory.platform.plan.openFileNil
	policy := directory.platform.plan.file
	wrapFile := directory.platform.plan.wrapFile
	directory.platform.plan.mu.Unlock()
	if err != nil || returnNil {
		return nil, err
	}
	opened, err := directory.base.OpenFile(name, private, writable)
	if err != nil || opened == nil {
		return nil, err
	}
	if !wrapFile {
		return opened, nil
	}
	return &publicationFaultFile{File: opened, policy: policy}, nil
}

type publicationFaultReference struct {
	outputcap.CurrentEntryReference
}

type publicationFaultFilePolicy struct {
	mu       sync.Mutex
	sizeErr  error
	sameErr  error
	closeErr error
}

type publicationFaultFile struct {
	outputcap.File
	policy *publicationFaultFilePolicy
}

func (file *publicationFaultFile) Size() (uint64, error) {
	file.policy.mu.Lock()
	err := file.policy.sizeErr
	file.policy.mu.Unlock()
	if err != nil {
		return 0, err
	}
	return file.File.Size()
}

func (file *publicationFaultFile) SameFile(other outputcap.File) (bool, error) {
	file.policy.mu.Lock()
	err := file.policy.sameErr
	file.policy.mu.Unlock()
	if err != nil {
		return false, err
	}
	peer := other
	if wrapped, ok := other.(*publicationFaultFile); ok {
		peer = wrapped.File
	}
	return file.File.SameFile(peer)
}

func (file *publicationFaultFile) Close() error {
	file.policy.mu.Lock()
	defer file.policy.mu.Unlock()
	return file.policy.closeErr
}

type publicationCanonicalFaultPlatform struct {
	*publicationFaultPlatform
	locatorErr       error
	componentErr     error
	componentErrName string
}

type publicationClosureCheckpoint struct {
	publicationCheckpoint
}

func (checkpoint publicationClosureCheckpoint) SameOwnedFile(
	ctx context.Context,
	file outputcap.File,
) (resumeauthority.Evidence, error) {
	if wrapped, ok := file.(*publicationFaultFile); ok {
		file = wrapped.File
	}
	return checkpoint.publicationCheckpoint.SameOwnedFile(ctx, file)
}

func (platform *publicationCanonicalFaultPlatform) CanonicalLocatorKey(path string) (string, error) {
	if platform.locatorErr != nil {
		return "", platform.locatorErr
	}
	return platform.fakePlatform.CanonicalLocatorKey(path)
}

func (platform *publicationCanonicalFaultPlatform) CanonicalComponentKey(name string) (string, error) {
	if platform.componentErr != nil && name == platform.componentErrName {
		return "", platform.componentErr
	}
	return platform.fakePlatform.CanonicalComponentKey(name)
}

func TestPublicationAuthorityClosureClassifiesEveryAcquisitionCut(t *testing.T) {
	errIO := errors.New("publication authority I/O failed")
	tests := []struct {
		name      string
		path      string
		configure func(*publicationFaultPlan)
		ambiguous bool
	}{
		{
			name: "ancestor entry disappeared",
			path: "folder/final.bin",
			configure: func(plan *publicationFaultPlan) {
				plan.openEntryErr = ErrRetainedAuthorityChanged
			},
			ambiguous: true,
		},
		{
			name: "ancestor entry returned no reference",
			path: "folder/final.bin",
			configure: func(plan *publicationFaultPlan) {
				plan.openEntryNil = true
			},
			ambiguous: true,
		},
		{
			name: "ancestor entry I/O failure",
			path: "folder/final.bin",
			configure: func(plan *publicationFaultPlan) {
				plan.openEntryErr = errIO
			},
		},
		{
			name: "ancestor pinned open disappeared",
			path: "folder/final.bin",
			configure: func(plan *publicationFaultPlan) {
				plan.openPinnedErr = ErrRetainedAuthorityChanged
			},
			ambiguous: true,
		},
		{
			name: "ancestor pinned open returned no handle",
			path: "folder/final.bin",
			configure: func(plan *publicationFaultPlan) {
				plan.openPinnedNil = true
			},
			ambiguous: true,
		},
		{
			name: "ancestor pinned open I/O failure",
			path: "folder/final.bin",
			configure: func(plan *publicationFaultPlan) {
				plan.openPinnedErr = errIO
			},
		},
		{
			name: "ancestor reference changed",
			path: "folder/final.bin",
			configure: func(plan *publicationFaultPlan) {
				plan.entryMatchesFalseAt = 1
			},
			ambiguous: true,
		},
		{
			name: "ancestor comparison changed during observation",
			path: "folder/final.bin",
			configure: func(plan *publicationFaultPlan) {
				plan.entryMatchesErrAt = 1
				plan.entryMatchesErr = ErrRetainedAuthorityChanged
			},
			ambiguous: true,
		},
		{
			name: "ancestor comparison I/O failure",
			path: "folder/final.bin",
			configure: func(plan *publicationFaultPlan) {
				plan.entryMatchesErrAt = 1
				plan.entryMatchesErr = errIO
			},
		},
		{
			name: "final reference changed before open",
			path: "final.bin",
			configure: func(plan *publicationFaultPlan) {
				plan.entryMatchesFalseAt = 1
			},
			ambiguous: true,
		},
		{
			name: "final open returned no handle",
			path: "final.bin",
			configure: func(plan *publicationFaultPlan) {
				plan.openFileNil = true
			},
			ambiguous: true,
		},
		{
			name: "final changed after open",
			path: "final.bin",
			configure: func(plan *publicationFaultPlan) {
				plan.entryMatchesFalseAt = 2
			},
			ambiguous: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			platform := newPublicationFaultPlatform()
			var final *fakeNode
			if test.path == "folder/final.bin" {
				folder := platform.addDirectory(platform.rootNode(), "folder")
				final = platform.addFile(folder, "final.bin", publicationFixtureSize)
			} else {
				final = platform.addFile(platform.rootNode(), "final.bin", publicationFixtureSize)
			}
			test.configure(platform.plan)
			observer, err := NewPublicationObserver(platform)
			if err != nil {
				t.Fatal(err)
			}
			pin, err := observer.PinPublication(context.Background(), publicationCheckpoint{
				record: publicationRecord(t, test.path, 0xa1),
				owned:  final,
			})
			if test.ambiguous {
				if err != nil || pin == nil ||
					pin.Observation().FinalEvidence() != resumeauthority.EvidenceAmbiguous {
					t.Fatalf("ambiguous pin=%T evidence=%v error=%v", pin, pinEvidence(pin), err)
				}
				if err := pin.Close(); err != nil {
					t.Fatal(err)
				}
				return
			}
			if pin != nil || !errors.Is(err, errIO) {
				t.Fatalf("I/O pin=%T error=%v", pin, err)
			}
		})
	}
}

func pinEvidence(pin resumeauthority.PinnedPublication) resumeauthority.Evidence {
	if pin == nil {
		return 0
	}
	return pin.Observation().FinalEvidence()
}

func TestPublicationAuthorityClosureRetainsRestartWitnessTransitions(t *testing.T) {
	t.Run("published object size is part of initial evidence", func(t *testing.T) {
		platform := newPublicationFaultPlatform()
		final := platform.addFile(platform.rootNode(), "final.bin", publicationFixtureSize+1)
		observer, err := NewPublicationObserver(platform)
		if err != nil {
			t.Fatal(err)
		}
		pin, err := observer.PinPublication(context.Background(), publicationCheckpoint{
			record: publicationRecord(t, "final.bin", 0xa2),
			owned:  final,
		})
		if err != nil {
			t.Fatal(err)
		}
		if pin.Observation().FinalEvidence() != resumeauthority.EvidenceReplaced {
			t.Fatalf("size-mismatched evidence = %v", pin.Observation().FinalEvidence())
		}
		if err := pin.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("exact final deletion remains absent rather than replaced", func(t *testing.T) {
		platform := newPublicationFaultPlatform()
		final := platform.addFile(platform.rootNode(), "final.bin", publicationFixtureSize)
		observer, err := NewPublicationObserver(platform)
		if err != nil {
			t.Fatal(err)
		}
		pin, err := observer.PinPublication(context.Background(), publicationCheckpoint{
			record: publicationRecord(t, "final.bin", 0xa3),
			owned:  final,
		})
		if err != nil {
			t.Fatal(err)
		}
		platform.mu.Lock()
		delete(platform.root.entries, "final.bin")
		platform.mu.Unlock()
		evidence, err := pin.Revalidate(context.Background())
		if err != nil || evidence != resumeauthority.EvidenceAbsent {
			t.Fatalf("deleted final evidence=%v error=%v", evidence, err)
		}
		if err := pin.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("alias-equivalent replacement is ambiguous", func(t *testing.T) {
		platform := newPublicationFaultPlatform()
		final := platform.addFile(platform.rootNode(), "final.bin", publicationFixtureSize)
		observer, err := NewPublicationObserver(platform)
		if err != nil {
			t.Fatal(err)
		}
		pin, err := observer.PinPublication(context.Background(), publicationCheckpoint{
			record: publicationRecord(t, "final.bin", 0xa4),
			owned:  final,
		})
		if err != nil {
			t.Fatal(err)
		}
		platform.mu.Lock()
		delete(platform.root.entries, "final.bin")
		platform.mu.Unlock()
		platform.addFile(platform.rootNode(), "FINAL.BIN", publicationFixtureSize)
		evidence, err := pin.Revalidate(context.Background())
		if err != nil || evidence != resumeauthority.EvidenceAmbiguous {
			t.Fatalf("alias replacement evidence=%v error=%v", evidence, err)
		}
		if err := pin.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("same retained object with changed size is replaced evidence", func(t *testing.T) {
		platform := newPublicationFaultPlatform()
		final := platform.addFile(platform.rootNode(), "final.bin", publicationFixtureSize)
		observer, err := NewPublicationObserver(platform)
		if err != nil {
			t.Fatal(err)
		}
		pin, err := observer.PinPublication(context.Background(), publicationCheckpoint{
			record: publicationRecord(t, "final.bin", 0xa5),
			owned:  final,
		})
		if err != nil {
			t.Fatal(err)
		}
		platform.mu.Lock()
		final.size++
		platform.mu.Unlock()
		evidence, err := pin.Revalidate(context.Background())
		if err != nil || evidence != resumeauthority.EvidenceReplaced {
			t.Fatalf("changed-size evidence=%v error=%v", evidence, err)
		}
		if err := pin.Close(); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("final can disappear after reference revalidation", func(t *testing.T) {
		platform := newPublicationFaultPlatform()
		final := platform.addFile(platform.rootNode(), "final.bin", publicationFixtureSize)
		observer, err := NewPublicationObserver(platform)
		if err != nil {
			t.Fatal(err)
		}
		pin, err := observer.PinPublication(context.Background(), publicationCheckpoint{
			record: publicationRecord(t, "final.bin", 0xa6),
			owned:  final,
		})
		if err != nil {
			t.Fatal(err)
		}
		platform.plan.mu.Lock()
		platform.plan.entryMatchesHookAt = platform.plan.entryMatchesCalls + 1
		platform.plan.entryMatchesHook = func() {
			platform.mu.Lock()
			delete(platform.root.entries, "final.bin")
			platform.mu.Unlock()
		}
		platform.plan.mu.Unlock()
		evidence, err := pin.Revalidate(context.Background())
		if err != nil || evidence != resumeauthority.EvidenceAbsent {
			t.Fatalf("post-reference deletion evidence=%v error=%v", evidence, err)
		}
		if err := pin.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestPublicationAuthorityClosureClosesGuardsAndSurfacesWitnessFaults(t *testing.T) {
	if _, err := NewPublicationObserver(nil); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("nil observer platform error = %v", err)
	}
	badCanonical := &publicationCanonicalFaultPlatform{
		publicationFaultPlatform: newPublicationFaultPlatform(),
		componentErr:             errors.New("component key unavailable"),
		componentErrName:         reservedControlComponent,
	}
	if _, err := NewPublicationObserver(badCanonical); !errors.Is(err, badCanonical.componentErr) {
		t.Fatalf("reserved component key error = %v", err)
	}
	canonical := &publicationCanonicalFaultPlatform{
		publicationFaultPlatform: newPublicationFaultPlatform(),
	}
	canonicalObserver, err := NewPublicationObserver(canonical)
	if err != nil {
		t.Fatal(err)
	}
	canonical.locatorErr = errors.New("locator key unavailable")
	if _, err := canonicalObserver.PinPublication(context.Background(), publicationCheckpoint{
		record: publicationRecord(t, "folder/final.bin", 0xb0),
	}); !errors.Is(err, canonical.locatorErr) || !errors.Is(err, ErrInvalidLocator) {
		t.Fatalf("canonical locator error = %v", err)
	}
	canonical.locatorErr = nil
	canonical.componentErr = errors.New("path component key unavailable")
	canonical.componentErrName = "folder"
	if _, err := canonicalObserver.PinPublication(context.Background(), publicationCheckpoint{
		record: publicationRecord(t, "folder/final.bin", 0xb0),
	}); !errors.Is(err, canonical.componentErr) || !errors.Is(err, ErrInvalidLocator) {
		t.Fatalf("canonical component error = %v", err)
	}

	platform := newPublicationFaultPlatform()
	observer, err := NewPublicationObserver(platform)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := (*PublicationObserver)(nil).PinPublication(
		context.Background(),
		publicationCheckpoint{record: publicationRecord(t, "final.bin", 0xb1)},
	); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("nil observer pin error = %v", err)
	}
	if _, err := observer.PinPublication(context.Background(), nil); !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("nil checkpoint pin error = %v", err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := observer.PinPublication(canceled, publicationCheckpoint{
		record: publicationRecord(t, "final.bin", 0xb3),
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled pin error = %v", err)
	}
	if _, err := observer.PinPublication(context.Background(), publicationCheckpoint{}); !errors.Is(err, ErrInvalidLocator) {
		t.Fatalf("invalid record pin error = %v", err)
	}

	errGuard := errors.New("restart guard unavailable")
	platform.guardErr = errGuard
	if _, err := observer.PinPublication(context.Background(), publicationCheckpoint{
		record: publicationRecord(t, "final.bin", 0xb4),
	}); !errors.Is(err, errGuard) {
		t.Fatalf("guard acquisition error = %v", err)
	}
	platform.guardErr = nil
	platform.rootNil = true
	if _, err := observer.PinPublication(context.Background(), publicationCheckpoint{
		record: publicationRecord(t, "final.bin", 0xb5),
	}); !errors.Is(err, ErrRetainedAuthorityChanged) {
		t.Fatalf("missing guarded root error = %v", err)
	}

	afterGuard := newPublicationFaultPlatform()
	afterObserver, err := NewPublicationObserver(afterGuard)
	if err != nil {
		t.Fatal(err)
	}
	afterContext, afterCancel := context.WithCancel(context.Background())
	afterGuard.afterGuard = afterCancel
	if _, err := afterObserver.PinPublication(afterContext, publicationCheckpoint{
		record: publicationRecord(t, "final.bin", 0xb6),
	}); !errors.Is(err, context.Canceled) {
		t.Fatalf("post-guard cancellation error = %v", err)
	}

	faultPlatform := newPublicationFaultPlatform()
	faultFinal := faultPlatform.addFile(faultPlatform.rootNode(), "final.bin", publicationFixtureSize)
	faultObserver, err := NewPublicationObserver(faultPlatform)
	if err != nil {
		t.Fatal(err)
	}
	errRelease := errors.New("publication guard close failed")
	faultPlatform.guardCloseErr = errRelease
	faultPlatform.plan.wrapFile = true
	pin, err := faultObserver.PinPublication(context.Background(), publicationClosureCheckpoint{
		publicationCheckpoint: publicationCheckpoint{
			record: publicationRecord(t, "final.bin", 0xb7),
			owned:  faultFinal,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	errSame := errors.New("same-file witness failed")
	errClose := errors.New("current final close failed")
	errSize := errors.New("retained final size failed")
	faultPlatform.plan.file.mu.Lock()
	faultPlatform.plan.file.sameErr = errSame
	faultPlatform.plan.file.closeErr = errClose
	faultPlatform.plan.file.sizeErr = errSize
	faultPlatform.plan.file.mu.Unlock()
	evidence, err := pin.Revalidate(context.Background())
	if evidence != resumeauthority.EvidenceAmbiguous || !errors.Is(err, errSame) ||
		!errors.Is(err, errClose) || !errors.Is(err, errSize) {
		t.Fatalf("witness fault evidence=%v error=%v", evidence, err)
	}
	faultPlatform.plan.file.mu.Lock()
	faultPlatform.plan.file.sameErr = nil
	faultPlatform.plan.file.closeErr = nil
	faultPlatform.plan.file.sizeErr = nil
	faultPlatform.plan.file.mu.Unlock()
	if err := pin.Close(); !errors.Is(err, errRelease) {
		t.Fatalf("pin close error = %v", err)
	}
	if err := pin.Close(); !errors.Is(err, errRelease) {
		t.Fatalf("idempotent pin close error = %v", err)
	}
	if _, err := pin.Revalidate(context.Background()); !errors.Is(err, ErrAuthorityClosed) {
		t.Fatalf("closed pin revalidation error = %v", err)
	}

	var nilPin *publicationPin
	if nilPin.Observation() != (resumeauthority.PublicationObservation{}) || nilPin.Close() != nil {
		t.Fatal("nil publication pin did not remain inert")
	}
	if _, err := nilPin.Revalidate(context.Background()); !errors.Is(err, ErrInvalidClaim) {
		t.Fatalf("nil publication pin error = %v", err)
	}
	canceledRevalidation, cancelRevalidation := context.WithCancel(context.Background())
	cancelRevalidation()
	if _, err := pin.Revalidate(canceledRevalidation); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled revalidation error = %v", err)
	}
}

func TestPublicationAuthorityClosureReturnsOperationalObservationErrors(t *testing.T) {
	errClassify := errors.New("classify final failed")
	platform := newPublicationFaultPlatform()
	platform.plan.classifyErr = errClassify
	observer, err := NewPublicationObserver(platform)
	if err != nil {
		t.Fatal(err)
	}
	if pin, err := observer.PinPublication(context.Background(), publicationCheckpoint{
		record: publicationRecord(t, "final.bin", 0xc1),
	}); pin != nil || !errors.Is(err, errClassify) {
		t.Fatalf("classify pin=%T error=%v", pin, err)
	}

	openPlatform := newPublicationFaultPlatform()
	openPlatform.addFile(openPlatform.rootNode(), "final.bin", publicationFixtureSize)
	errOpen := errors.New("open final failed")
	openPlatform.plan.openFileErr = errOpen
	openObserver, err := NewPublicationObserver(openPlatform)
	if err != nil {
		t.Fatal(err)
	}
	if pin, err := openObserver.PinPublication(context.Background(), publicationCheckpoint{
		record: publicationRecord(t, "final.bin", 0xc2),
	}); pin != nil || !errors.Is(err, errOpen) {
		t.Fatalf("open pin=%T error=%v", pin, err)
	}

	comparePlatform := newPublicationFaultPlatform()
	compareFinal := comparePlatform.addFile(comparePlatform.rootNode(), "final.bin", publicationFixtureSize)
	compareObserver, err := NewPublicationObserver(comparePlatform)
	if err != nil {
		t.Fatal(err)
	}
	errCompare := errors.New("owned witness comparison failed")
	if pin, err := compareObserver.PinPublication(context.Background(), publicationCheckpoint{
		record:   publicationRecord(t, "final.bin", 0xc3),
		owned:    compareFinal,
		evidence: resumeauthority.EvidenceAmbiguous,
	}); err != nil || pin == nil {
		t.Fatalf("explicit ambiguous pin=%T error=%v", pin, err)
	} else if closeErr := pin.Close(); closeErr != nil {
		t.Fatal(closeErr)
	}
	comparePlatform.plan.file.sizeErr = errCompare
	comparePlatform.plan.wrapFile = true
	if pin, err := compareObserver.PinPublication(context.Background(), publicationClosureCheckpoint{
		publicationCheckpoint: publicationCheckpoint{
			record: publicationRecord(t, "final.bin", 0xc4),
			owned:  compareFinal,
		},
	}); pin != nil || !errors.Is(err, errCompare) {
		t.Fatalf("size observation pin=%T error=%v", pin, err)
	}

	missingPlatform := newPublicationFaultPlatform()
	missingObserver, err := NewPublicationObserver(missingPlatform)
	if err != nil {
		t.Fatal(err)
	}
	missingPin, err := missingObserver.PinPublication(context.Background(), publicationCheckpoint{
		record: publicationRecord(t, "missing/final.bin", 0xc5),
	})
	if err != nil {
		t.Fatal(err)
	}
	missingPlatform.plan.classifyErr = fs.ErrPermission
	if evidence, err := missingPin.Revalidate(context.Background()); evidence != resumeauthority.EvidenceAmbiguous || !errors.Is(err, fs.ErrPermission) {
		t.Fatalf("missing-lineage evidence=%v error=%v", evidence, err)
	}
	if err := missingPin.Close(); err != nil {
		t.Fatal(err)
	}
}
