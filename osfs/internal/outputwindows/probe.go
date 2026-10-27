//go:build windows

package outputwindows

import (
	"crypto/rand"
	"errors"
	"io"

	"github.com/windshare/windshare/osfs/internal/outputcap"
	"github.com/windshare/windshare/osfs/internal/outputprobe"
)

func (root *windowsDirectory) probeRecoverableFeatures() error {
	return root.probeRecoverableFeaturesWithRandom(rand.Reader)
}

func (root *windowsDirectory) destinationCapabilities() (outputcap.DestinationCapabilities, error) {
	results, err := root.probeDestinationCapabilitiesWithRandom(rand.Reader)
	if err != nil {
		return outputcap.DestinationCapabilities{}, windowsOutputError(err)
	}
	return results.Capabilities()
}

func (root *windowsDirectory) probeRecoverableFeaturesWithRandom(random io.Reader) error {
	results, err := root.probeDestinationCapabilitiesWithRandom(random)
	return windowsProbeError(errors.Join(err, results.Err()))
}

func (root *windowsDirectory) probeDestinationCapabilitiesWithRandom(
	random io.Reader,
) (results outputprobe.Results, resultErr error) {
	if err := root.usable(); err != nil {
		return results, err
	}
	lock, err := root.acquireOutputProbeLock()
	if err != nil {
		return results, err
	}
	defer func() {
		resultErr = windowsProbeError(errors.Join(resultErr, root.releaseOutputProbeLock(lock)))
	}()
	if err := root.recoverOutputProbeLeftovers(); err != nil {
		return results, err
	}
	workspace, err := outputprobe.Allocate(&windowsOutputDirectory{native: root}, random)
	if err != nil {
		return results, err
	}
	if err := errors.Join(workspace.Directory().Sync(), root.Sync()); err != nil {
		results.SafePublish = windowsOutputError(err)
		results.OperationRecovery = windowsProbeDependencyFailure("private probe namespace", err)
		results.RangeRecovery = windowsProbeDependencyFailure("private probe namespace", err)
		results.CrashCleanup = windowsProbeDependencyFailure("private probe namespace", err)
	} else {
		results = runWindowsCapabilityFacts(workspace)
	}
	if err := workspace.Cleanup(); err != nil {
		return results, errors.Join(outputcap.ErrUnsafeNamespace, err)
	}
	if results.OperationRecovery == nil {
		results.OperationRecovery = windowsOutputError(root.preparePersistentRootIdentity())
		if results.OperationRecovery != nil && results.RangeRecovery == nil {
			results.RangeRecovery = windowsProbeDependencyFailure("persistent root identity", results.OperationRecovery)
		}
	}
	return results, nil
}

func runWindowsCapabilityFacts(workspace *outputprobe.Workspace) (results outputprobe.Results) {
	// NTFS's cleanup proof retains the linked files and installed record from
	// these steps. The shared algorithms own all mutation and cleanup ordering.
	results.SafePublish = workspace.SafePublish(outputprobe.SizedDataLinks)
	if results.SafePublish != nil {
		results.OperationRecovery = windowsProbeDependencyFailure("safe publication", results.SafePublish)
		results.RangeRecovery = windowsProbeDependencyFailure("safe publication", results.SafePublish)
		results.CrashCleanup = windowsProbeDependencyFailure("private stage creation", results.SafePublish)
		return results
	}
	results.RangeRecovery = workspace.RangeRecovery()
	if results.RangeRecovery != nil {
		results.OperationRecovery = windowsProbeDependencyFailure("private record replacement", results.RangeRecovery)
		results.CrashCleanup = windowsProbeDependencyFailure("fixed private records", results.RangeRecovery)
		return results
	}
	results.OperationRecovery = workspace.OperationRecovery()
	if results.OperationRecovery != nil {
		results.CrashCleanup = windowsProbeDependencyFailure("fixed operation state", results.OperationRecovery)
		return results
	}
	results.CrashCleanup = verifyWindowsProbeCleanup(workspace)
	return results
}

func verifyWindowsProbeCleanup(workspace *outputprobe.Workspace) error {
	directory, ok := workspace.Directory().(*windowsOutputDirectory)
	if !ok || directory == nil || directory.native == nil {
		return outputcap.ErrUnsafeNamespace
	}
	// Child identity does not prove that the parent still has the protected
	// NTFS ACL and attributes required to own restart-cleanup names.
	if err := directory.native.verify(true); err != nil {
		return windowsOutputError(err)
	}
	return workspace.VerifyFiles()
}

func windowsProbeDependencyFailure(dependency string, cause error) error {
	return windowsOutputError(windowsFailure(
		"probe Windows destination capability", "", errWindowsOutputUnsupported,
		errors.Join(errors.New("required dependency could not be proven: "+dependency), cause),
	))
}

func windowsProbeError(err error) error {
	switch {
	case errors.Is(err, outputcap.ErrUnsafeNamespace):
		return errors.Join(errWindowsOutputUnsafe, err)
	case errors.Is(err, outputcap.ErrRecoverableOutputUnsupported):
		return errors.Join(errWindowsOutputUnsupported, err)
	default:
		return err
	}
}
