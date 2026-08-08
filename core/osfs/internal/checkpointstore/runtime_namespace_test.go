package checkpointstore

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"io/fs"
	"slices"
	"testing"

	"github.com/windshare/windshare/core/osfs/internal/checkpointmodel"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	transferfault "github.com/windshare/windshare/core/transfer/fault"
)

func TestRuntimeClosureResourceReleaseOrderKeepsLeaseAuthorityLast(t *testing.T) {
	failure := errors.New("close failure")
	var order []string
	closeDirectory := func(name string) outputcap.Directory {
		return &runtimeClosureCloseDirectory{
			Directory: newMemoryDirectory(),
			close: func() error {
				order = append(order, name)
				return failure
			},
		}
	}
	closeLock := func(name string) outputcap.Lock {
		return &runtimeClosureCloseLock{
			Lock: &memoryLock{directory: newMemoryDirectory(), name: name, file: &memoryFile{data: &memoryFileData{}}},
			close: func() error {
				order = append(order, name)
				return failure
			},
		}
	}

	namespace := Namespace{
		intents: closeDirectory("intents"), leases: closeDirectory("leases"), checkpointRoot: closeDirectory("checkpoint-root"),
	}
	if err := namespace.Close(); errorCode(err) != ErrorStateIO || !slices.Equal(order, []string{"intents", "leases", "checkpoint-root"}) {
		t.Fatalf("namespace close order = %v, %v", order, err)
	}
	if err := namespace.Close(); err != nil {
		t.Fatalf("second namespace close = %v", err)
	}

	order = nil
	repository := Repository{
		records: closeDirectory("records"), anchors: closeDirectory("anchors"),
		stages: closeDirectory("stages"), intent: closeDirectory("intent"),
	}
	if err := repository.Close(); errorCode(err) != ErrorStateIO ||
		!slices.Equal(order, []string{"records", "anchors", "stages", "intent"}) {
		t.Fatalf("repository close order = %v, %v", order, err)
	}

	order = nil
	lease := IntentLease{intents: closeDirectory("retained-intents"), lock: closeLock("intent-lock")}
	if err := lease.Close(); errorCode(err) != ErrorStateIO || !slices.Equal(order, []string{"retained-intents", "intent-lock"}) {
		t.Fatalf("intent lease close order = %v, %v", order, err)
	}
}

func TestRuntimeClosureOpenNamespaceClosesPartiallyOpenedCapabilitiesInOrder(t *testing.T) {
	failure := errors.New("namespace open failure")

	t.Run("control close failure closes checkpoint root", func(t *testing.T) {
		root, config := runtimeClosureInitializedRoot(t, 0xa1)
		control, _ := root.OpenDirectory(ControlDirectory, true)
		checkpointRoot, _ := control.OpenDirectory(CheckpointDirectory, true)
		var order []string
		checkpointTracked := runtimeClosureTrackedDirectory(checkpointRoot, "checkpoint-root", &order, nil)
		controlTracked := runtimeClosureTrackedDirectory(control, "control", &order, failure)
		controlWrapped := &runtimeClosureOpenDirectory{
			Directory: controlTracked,
			open:      func(string, bool) (outputcap.Directory, error) { return checkpointTracked, nil },
		}
		rootWrapped := &runtimeClosureOpenDirectory{
			Directory: root,
			open:      func(string, bool) (outputcap.Directory, error) { return controlWrapped, nil },
		}
		config.Root = rootWrapped
		if _, err := OpenNamespace(config); errorCode(err) != ErrorStateIO || !errors.Is(err, failure) ||
			!slices.Equal(order, []string{"control", "checkpoint-root"}) {
			t.Fatalf("control close cut = order:%v error:%v", order, err)
		}
	})

	t.Run("intent open failure closes returned handle then prior authorities", func(t *testing.T) {
		root, config := runtimeClosureInitializedRoot(t, 0xb1)
		control, _ := root.OpenDirectory(ControlDirectory, true)
		checkpointRoot, _ := control.OpenDirectory(CheckpointDirectory, true)
		leases, _ := checkpointRoot.OpenDirectory(LeasesDirectory, true)
		intents, _ := checkpointRoot.OpenDirectory(IntentsDirectory, true)
		var order []string
		controlTracked := runtimeClosureTrackedDirectory(control, "control", &order, nil)
		checkpointTracked := runtimeClosureTrackedDirectory(checkpointRoot, "checkpoint-root", &order, nil)
		leasesTracked := runtimeClosureTrackedDirectory(leases, "leases", &order, nil)
		intentsTracked := runtimeClosureTrackedDirectory(intents, "returned-intents", &order, nil)
		checkpointWrapped := &runtimeClosureOpenDirectory{
			Directory: checkpointTracked,
			open: func(name string, _ bool) (outputcap.Directory, error) {
				switch name {
				case LeasesDirectory:
					return leasesTracked, nil
				case IntentsDirectory:
					return intentsTracked, failure
				default:
					return nil, fs.ErrNotExist
				}
			},
		}
		controlWrapped := &runtimeClosureOpenDirectory{
			Directory: controlTracked,
			open:      func(string, bool) (outputcap.Directory, error) { return checkpointWrapped, nil },
		}
		config.Root = &runtimeClosureOpenDirectory{
			Directory: root,
			open:      func(string, bool) (outputcap.Directory, error) { return controlWrapped, nil },
		}
		if _, err := OpenNamespace(config); errorCode(err) != ErrorStateIO || !errors.Is(err, failure) ||
			!slices.Equal(order, []string{"control", "returned-intents", "leases", "checkpoint-root"}) {
			t.Fatalf("intent open cut = order:%v error:%v", order, err)
		}
	})
}

func TestRuntimeClosureIntentAcquisitionReleasesTheLockOnEverySetupFailure(t *testing.T) {
	failure := errors.New("intent setup failure")
	for _, test := range []struct {
		name      string
		configure func(*Namespace)
		want      error
	}{
		{
			name: "lease sync",
			configure: func(namespace *Namespace) {
				namespace.leases = &runtimeClosureSyncDirectory{Directory: namespace.leases, sync: func() error { return failure }}
			},
			want: failure,
		},
		{
			name: "retained intents duplicate",
			configure: func(namespace *Namespace) {
				namespace.intents = &runtimeClosureDuplicateDirectory{
					Directory: namespace.intents,
					duplicate: func() (outputcap.Directory, error) { return nil, failure },
				}
			},
			want: failure,
		},
		{
			name: "nil retained intents capability",
			configure: func(namespace *Namespace) {
				namespace.intents = &runtimeClosureDuplicateDirectory{
					Directory: namespace.intents,
					duplicate: func() (outputcap.Directory, error) { return nil, nil },
				}
			},
			want: outputcap.ErrUnsafeNamespace,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := newMemoryDirectory()
			config, intent := certifiedFixture(t, root, checkpointmodel.CallerProvidedContainer, 0xc1)
			namespace, err := Initialize(config)
			if err != nil {
				t.Fatal(err)
			}
			baseLeases, baseIntents := namespace.leases, namespace.intents
			test.configure(&namespace)
			if _, err := namespace.AcquireIntent(intent); !errors.Is(err, test.want) {
				t.Fatalf("acquire setup cut = %v", err)
			}
			namespace.leases, namespace.intents = baseLeases, baseIntents
			lease, err := namespace.AcquireIntent(intent)
			if err != nil {
				t.Fatalf("lock remained held after setup failure: %v", err)
			}
			_ = lease.Close()
			_ = namespace.Close()
		})
	}
}

func TestRuntimeClosureFaultNormalizationPreservesCancellationAndNativeContext(t *testing.T) {
	for _, canceled := range []error{context.Canceled, context.DeadlineExceeded} {
		if got := repositoryError("repository", canceled); got != canceled {
			t.Fatalf("repository cancellation = %v", got)
		}
		if got := codedError(ErrorStateIO, "coded", canceled); got != canceled {
			t.Fatalf("coded cancellation = %v", got)
		}
		if got := fileOutputBoundaryError(context.Background(), transferfault.ScopeFileLocal, canceled); got != canceled {
			t.Fatalf("file cancellation = %v", got)
		}
	}

	native := &Error{code: ErrorBusy, operation: "retain native operation", cause: outputcap.ErrNamespaceLockBusy}
	var normalized *Error
	if err := repositoryError("outer operation", native); !errors.As(err, &normalized) ||
		normalized.Code() != ErrorBusy || normalized.Operation() != native.operation {
		t.Fatalf("native checkpoint context = %#v", err)
	}

	for _, test := range []struct {
		name  string
		cause error
		kind  transferfault.OutputCode
	}{
		{"unsupported", outputcap.ErrRecoverableOutputUnsupported, transferfault.OutputUnsupportedFilesystem},
		{"lock ownership", outputcap.ErrNamespaceLockBusy, transferfault.OutputOwnership},
		{"state I/O", errors.New("file state failure"), transferfault.OutputStateIO},
	} {
		t.Run(test.name, func(t *testing.T) {
			var boundary *transferfault.BoundaryError
			err := fileOutputBoundaryError(context.Background(), transferfault.ScopeFileLocal, test.cause)
			if !errors.As(err, &boundary) {
				t.Fatalf("file output normalization = %v", err)
			}
			code, ok := boundaryOutputCode(boundary)
			if !ok || code != test.kind || !errors.Is(err, test.cause) {
				t.Fatalf("file output normalization = %v", err)
			}
		})
	}
}

func boundaryOutputCode(boundary *transferfault.BoundaryError) (transferfault.OutputCode, bool) {
	if boundary == nil {
		return 0, false
	}
	return boundary.Fault().OutputCode()
}

func TestRuntimeClosureNamespaceConstructionReleasesPartialAuthority(t *testing.T) {
	failure := errors.New("namespace construction failure")

	t.Run("invalid values fail before capability access", func(t *testing.T) {
		if _, err := Initialize(CertifiedConfig{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
			t.Fatalf("invalid initialize = %v", err)
		}
		if _, err := OpenNamespace(CertifiedConfig{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
			t.Fatalf("invalid open = %v", err)
		}
		var namespace *Namespace
		if _, err := namespace.AcquireIntent(transfer.TransferIntentDigest{}); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
			t.Fatalf("invalid acquire = %v", err)
		}
		var lease *IntentLease
		if _, err := lease.OpenExistingRepository(); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
			t.Fatalf("invalid leased repository = %v", err)
		}
	})

	t.Run("checkpoint creation error closes returned checkpoint then control", func(t *testing.T) {
		root := newMemoryDirectory()
		config, _ := certifiedFixture(t, root, checkpointmodel.CallerProvidedContainer, 0x41)
		control, err := root.CreateDirectory(ControlDirectory, true)
		if err != nil {
			t.Fatal(err)
		}
		var order []string
		controlTracked := runtimeClosureTrackedDirectory(control, "control", &order, nil)
		controlWrapped := &runtimeClosureCreateDirectory{
			Directory: controlTracked,
			create: func(string, bool) (outputcap.Directory, error) {
				return runtimeClosureTrackedDirectory(newMemoryDirectory(), "checkpoint", &order, nil), failure
			},
		}
		config.Root = &runtimeClosureOpenDirectory{
			Directory: root,
			open:      func(string, bool) (outputcap.Directory, error) { return controlWrapped, nil },
		}
		if _, err := Initialize(config); errorCode(err) != ErrorStateIO || !errors.Is(err, failure) ||
			!slices.Equal(order, []string{"checkpoint", "control"}) {
			t.Fatalf("checkpoint create cut = order:%v error:%v", order, err)
		}
	})

	t.Run("control close error closes opened checkpoint", func(t *testing.T) {
		root := newMemoryDirectory()
		config, _ := certifiedFixture(t, root, checkpointmodel.CallerProvidedContainer, 0x43)
		control, _ := root.CreateDirectory(ControlDirectory, true)
		checkpointRoot, _ := control.CreateDirectory(CheckpointDirectory, true)
		var order []string
		checkpointTracked := runtimeClosureTrackedDirectory(checkpointRoot, "checkpoint", &order, nil)
		controlTracked := runtimeClosureTrackedDirectory(control, "control", &order, failure)
		controlWrapped := &runtimeClosureOpenDirectory{
			Directory: controlTracked,
			open:      func(string, bool) (outputcap.Directory, error) { return checkpointTracked, nil },
		}
		config.Root = &runtimeClosureOpenDirectory{
			Directory: root,
			open:      func(string, bool) (outputcap.Directory, error) { return controlWrapped, nil },
		}
		if _, err := Initialize(config); errorCode(err) != ErrorStateIO || !errors.Is(err, failure) ||
			!slices.Equal(order, []string{"control", "checkpoint"}) {
			t.Fatalf("control close cut = order:%v error:%v", order, err)
		}
	})

	t.Run("nil lock is never treated as acquired authority", func(t *testing.T) {
		root := newMemoryDirectory()
		config, intent := certifiedFixture(t, root, checkpointmodel.CallerProvidedContainer, 0x45)
		namespace, err := Initialize(config)
		if err != nil {
			t.Fatal(err)
		}
		defer namespace.Close()
		namespace.leases = &runtimeClosureAcquireDirectory{
			Directory: namespace.leases,
			acquire:   func(string, bool) (outputcap.Lock, bool, error) { return nil, false, nil },
		}
		if _, err := namespace.AcquireIntent(intent); errorCode(err) != ErrorUnsafeInstall {
			t.Fatalf("nil acquired lock = %v", err)
		}
	})
}

func TestRuntimeClosureCertifiedNamespaceFailureCutsReleaseEarlierCapabilities(t *testing.T) {
	failure := errors.New("certified namespace failure")

	for _, test := range []struct {
		name string
		wrap func(outputcap.Directory) outputcap.Directory
	}{
		{
			name: "ownership observation",
			wrap: func(checkpointRoot outputcap.Directory) outputcap.Directory {
				return &runtimeClosureObserveDirectory{
					Directory: checkpointRoot,
					observe:   func(string) (outputcap.EntryKind, error) { return 0, failure },
				}
			},
		},
		{
			name: "leases open",
			wrap: func(checkpointRoot outputcap.Directory) outputcap.Directory {
				return &runtimeClosureOpenDirectory{
					Directory: checkpointRoot,
					open: func(name string, private bool) (outputcap.Directory, error) {
						opened, err := checkpointRoot.OpenDirectory(name, private)
						if name == LeasesDirectory {
							return opened, failure
						}
						return opened, err
					},
				}
			},
		},
		{
			name: "intents open",
			wrap: func(checkpointRoot outputcap.Directory) outputcap.Directory {
				return &runtimeClosureOpenDirectory{
					Directory: checkpointRoot,
					open: func(name string, private bool) (outputcap.Directory, error) {
						opened, err := checkpointRoot.OpenDirectory(name, private)
						if name == IntentsDirectory {
							return opened, failure
						}
						return opened, err
					},
				}
			},
		},
	} {
		t.Run("initialize "+test.name, func(t *testing.T) {
			root, config := runtimeClosureInitializedRoot(t, 0x91)
			config = runtimeClosureConfigWithCheckpoint(t, root, config, test.wrap)
			if _, err := Initialize(config); errorCode(err) == "" || !errors.Is(err, failure) {
				t.Fatalf("initialize %s cut = %v", test.name, err)
			}
		})
	}

	t.Run("open rejects every missing certified parent", func(t *testing.T) {
		root := newMemoryDirectory()
		config, _ := certifiedFixture(t, root, checkpointmodel.CallerProvidedContainer, 0x93)
		if _, err := OpenNamespace(config); errorCode(err) != ErrorStateIO || !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("missing control = %v", err)
		}
		control, err := root.CreateDirectory(ControlDirectory, true)
		if err != nil {
			t.Fatal(err)
		}
		_ = control.Close()
		if _, err := OpenNamespace(config); errorCode(err) != ErrorStateIO || !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("missing checkpoint root = %v", err)
		}
	})

	t.Run("open distinguishes ownership I/O from absent leases", func(t *testing.T) {
		root, config := runtimeClosureInitializedRoot(t, 0x95)
		configWithFailure := runtimeClosureConfigWithCheckpoint(t, root, config, func(checkpointRoot outputcap.Directory) outputcap.Directory {
			return &runtimeClosureObserveDirectory{
				Directory: checkpointRoot,
				observe:   func(string) (outputcap.EntryKind, error) { return 0, failure },
			}
		})
		if _, err := OpenNamespace(configWithFailure); errorCode(err) != ErrorStateIO || !errors.Is(err, failure) {
			t.Fatalf("ownership I/O = %v", err)
		}

		control := root.dirs[ControlDirectory]
		checkpointRoot := control.dirs[CheckpointDirectory]
		checkpointRoot.mu.Lock()
		delete(checkpointRoot.dirs, LeasesDirectory)
		checkpointRoot.mu.Unlock()
		if _, err := OpenNamespace(config); errorCode(err) != ErrorStateIO || !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("missing leases = %v", err)
		}
	})

	t.Run("intent binding and nil release fail closed", func(t *testing.T) {
		invalid := &Namespace{
			ownership: checkpointmodel.Ownership{},
			leases:    newMemoryDirectory(), intents: newMemoryDirectory(),
		}
		intent, err := transfer.TransferIntentDigestFromBytes(bytes.Repeat([]byte{0x97}, sha256.Size))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := invalid.AcquireIntent(intent); !errors.Is(err, transfer.ErrInvalidOutputBinding) {
			t.Fatalf("invalid namespace ownership = %v", err)
		}
		if err := (*IntentLease)(nil).Close(); err != nil {
			t.Fatalf("nil intent lease close = %v", err)
		}
	})

	for _, blocked := range []string{RecordsDirectory, AnchorsDirectory, StagesDirectory} {
		t.Run("repository child "+blocked, func(t *testing.T) {
			_, namespace, lease, repository, _, _ := openRepositoryFixture(t, 0x99)
			if err := repository.Close(); err != nil {
				t.Fatal(err)
			}
			defer func() {
				_ = lease.Close()
				_ = namespace.Close()
			}()
			baseIntents := lease.intents
			lease.intents = &runtimeClosureOpenDirectory{
				Directory: baseIntents,
				open: func(name string, private bool) (outputcap.Directory, error) {
					intentDirectory, err := baseIntents.OpenDirectory(name, private)
					if err != nil {
						return intentDirectory, err
					}
					return &runtimeClosureOpenDirectory{
						Directory: intentDirectory,
						open: func(child string, childPrivate bool) (outputcap.Directory, error) {
							opened, openErr := intentDirectory.OpenDirectory(child, childPrivate)
							if child == blocked {
								return opened, failure
							}
							return opened, openErr
						},
					}, nil
				},
			}
			if _, err := lease.OpenExistingRepository(); errorCode(err) != ErrorStateIO || !errors.Is(err, failure) {
				t.Fatalf("open child %s = %v", blocked, err)
			}
		})
	}
}
