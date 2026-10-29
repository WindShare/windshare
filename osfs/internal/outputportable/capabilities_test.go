package outputportable

import (
	"errors"
	"io/fs"
	"os"
	"testing"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

func TestDestinationCapabilitiesUseActualPrimitivesWithoutHardLinks(t *testing.T) {
	ioFailure := errors.New("filesystem temporarily unavailable")
	for _, test := range []struct {
		name         string
		publishErr   error
		installErr   error
		wantSafe     bool
		wantRecovery bool
		wantIO       bool
	}{
		{name: "supported without hard links", wantSafe: true, wantRecovery: true},
		{name: "unsupported publication", publishErr: outputcap.ErrOrdinaryOutputUnsupported},
		{name: "unsupported directory installation", installErr: outputcap.ErrOrdinaryOutputUnsupported, wantSafe: true},
		{name: "transient publication failure", publishErr: ioFailure, wantIO: true},
		{name: "transient installation failure", installErr: ioFailure, wantIO: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := t.TempDir()
			platform, err := Open(path, false)
			if err != nil {
				t.Fatal(err)
			}
			defer platform.Close()
			root := &capabilityProbeRoot{
				Directory:  platform.Root(),
				publishErr: test.publishErr, installErr: test.installErr,
			}
			capabilities, err := probeDestinationCapabilities(root)
			if test.wantIO {
				if !errors.Is(err, ioFailure) || capabilities.Valid() {
					t.Fatalf("I/O failure became capabilities: %+v, %v", capabilities, err)
				}
			} else {
				if err != nil || capabilities.SafePublish().Supported() != test.wantSafe ||
					(capabilities.OperationRecovery().Supported() && capabilities.RangeRecovery().Supported()) != test.wantRecovery ||
					capabilities.CrashCleanup().Supported() {
					t.Fatalf("capabilities = %+v, %v", capabilities, err)
				}
				mode, modeErr := outputcap.SelectExecutionMode(capabilities)
				if !test.wantSafe && !errors.Is(modeErr, outputcap.ErrOrdinaryOutputUnsupported) {
					t.Fatalf("unsupported publication was admitted: %s, %v", mode, modeErr)
				}
				if test.wantSafe && !test.wantRecovery && (modeErr != nil || mode != outputcap.ExecutionLiveOnly) {
					t.Fatalf("independent publication was revoked: %s, %v", mode, modeErr)
				}
			}
			entries, err := os.ReadDir(path)
			if err != nil || len(entries) != 0 {
				t.Fatalf("capability probe left artifacts: %v, %v", entries, err)
			}
		})
	}
}

func TestDestinationCapabilitiesCacheFactsButRetryIO(t *testing.T) {
	opened, err := Open(t.TempDir(), false)
	if err != nil {
		t.Fatal(err)
	}
	platform := opened.(*Platform)
	defer platform.Close()
	calls := 0
	injected := errors.New("probe I/O failed")
	supported := outputcap.SupportedCapability()
	unsupported, _ := outputcap.UnsupportedCapability(outputcap.CapabilityReasonUnverifiableCrashCleanup)
	expected, _ := outputcap.NewDestinationCapabilities(supported, supported, supported, unsupported)
	probe := func(outputcap.Directory) (outputcap.DestinationCapabilities, error) {
		calls++
		if calls == 1 {
			return outputcap.DestinationCapabilities{}, injected
		}
		return expected, nil
	}
	if _, err := platform.destinationCapabilities(probe); !errors.Is(err, injected) {
		t.Fatalf("initial failure = %v", err)
	}
	for range 2 {
		if actual, err := platform.destinationCapabilities(probe); err != nil || actual != expected {
			t.Fatalf("capability retry = %+v, %v", actual, err)
		}
	}
	if calls != 2 {
		t.Fatalf("capabilities were probed %d times, want one retry then cached facts", calls)
	}
	if err := platform.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := platform.destinationCapabilities(probe); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("closed destination retained capabilities: %v", err)
	}
}

type capabilityProbeRoot struct {
	outputcap.Directory
	publishErr error
	installErr error
}

func (root *capabilityProbeRoot) CreateDirectory(name string, private bool) (outputcap.Directory, error) {
	directory, err := root.Directory.CreateDirectory(name, private)
	if err != nil {
		return nil, err
	}
	return &capabilityProbeDirectory{
		Directory: directory, publishErr: root.publishErr, installErr: root.installErr,
	}, nil
}
func (root *capabilityProbeRoot) RemoveDirectory(name string, expected outputcap.Directory) error {
	return root.Directory.RemoveDirectory(name, expected.(*capabilityProbeDirectory).Directory)
}

type capabilityProbeDirectory struct {
	outputcap.Directory
	publishErr error
	installErr error
}

func (*capabilityProbeDirectory) LinkFileNoReplace(outputcap.FileIdentity, string) (outputcap.ObservedFile, error) {
	panic("portable capability probing must not require hard links")
}
func (directory *capabilityProbeDirectory) PublishFileNoReplace(source outputcap.FileIdentity, name string) (outputcap.PublishNoReplaceOutcome, error) {
	if directory.publishErr != nil {
		return publicationResult(directory.publishErr)
	}
	return directory.Directory.(interface {
		PublishFileNoReplace(outputcap.FileIdentity, string) (outputcap.PublishNoReplaceOutcome, error)
	}).PublishFileNoReplace(source, name)
}
func (directory *capabilityProbeDirectory) InstallDirectoryNoReplace(source outputcap.Directory, name string) (outputcap.Directory, error) {
	if directory.installErr != nil {
		return nil, directory.installErr
	}
	return directory.Directory.InstallDirectoryNoReplace(source, name)
}
