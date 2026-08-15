//go:build windows

package outputwindows

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

const (
	windowsV3OutputProbePrefix             = ".windshare-output.probe-"
	windowsV3OutputProbeRandomBytes        = 16
	windowsV3OutputProbeAllocationAttempts = 16
)

func (root *windowsV3Directory) probeRecoverableFeatures() error {
	return root.probeRecoverableFeaturesWithRandom(rand.Reader)
}

func (root *windowsV3Directory) destinationCapabilities() (outputcap.DestinationCapabilities, error) {
	results, err := root.probeDestinationCapabilitiesWithRandom(rand.Reader)
	if err != nil {
		return outputcap.DestinationCapabilities{}, err
	}
	return windowsV3DestinationCapabilitiesFromResults(results)
}

type windowsV3CapabilityProbeResults struct {
	safePublish       error
	operationRecovery error
	rangeRecovery     error
	crashCleanup      error
}

func windowsV3DestinationCapabilitiesFromResults(
	results windowsV3CapabilityProbeResults,
) (outputcap.DestinationCapabilities, error) {
	facts := []struct {
		err    error
		reason outputcap.CapabilityReason
	}{
		{results.safePublish, outputcap.CapabilityReasonUnsafePublication},
		{results.operationRecovery, outputcap.CapabilityReasonUnverifiableOperationRecovery},
		{results.rangeRecovery, outputcap.CapabilityReasonUnverifiableRangeRecovery},
		{results.crashCleanup, outputcap.CapabilityReasonUnverifiableCrashCleanup},
	}
	evidence := make([]outputcap.CapabilityEvidence, len(facts))
	for index, fact := range facts {
		if fact.err == nil {
			evidence[index] = outputcap.SupportedCapability()
			continue
		}
		if errors.Is(fact.err, errWindowsV3OutputUnsafe) {
			return outputcap.DestinationCapabilities{}, windowsOutputV3Error(fact.err)
		}
		evidence[index], _ = outputcap.UnsupportedCapability(fact.reason)
	}
	return outputcap.NewDestinationCapabilities(evidence[0], evidence[1], evidence[2], evidence[3])
}

func (root *windowsV3Directory) probeRecoverableFeaturesWithRandom(random io.Reader) error {
	results, err := root.probeDestinationCapabilitiesWithRandom(random)
	if err != nil {
		return err
	}
	return errors.Join(
		results.safePublish,
		results.operationRecovery,
		results.rangeRecovery,
		results.crashCleanup,
	)
}

func (root *windowsV3Directory) probeDestinationCapabilitiesWithRandom(
	random io.Reader,
) (results windowsV3CapabilityProbeResults, resultErr error) {
	const operation = "probe Windows output filesystem"
	if err := root.usable(); err != nil {
		return results, err
	}
	lock, err := root.acquireOutputProbeLock()
	if err != nil {
		return results, err
	}
	defer func() { resultErr = errors.Join(resultErr, root.releaseOutputProbeLock(lock)) }()
	if err := root.recoverOutputProbeLeftovers(); err != nil {
		return results, err
	}
	if random == nil {
		return results, windowsV3Failure(operation, "", errWindowsV3OutputUnsafe, errors.New("random source is absent"))
	}
	for range windowsV3OutputProbeAllocationAttempts {
		name, err := newWindowsV3OutputProbeName(random)
		if err != nil {
			return results, windowsV3Failure(operation, "", errWindowsV3OutputUnsafe, err)
		}
		directory, err := root.CreatePrivateDirectory(name)
		if errors.Is(err, errWindowsV3OutputCollision) {
			continue
		}
		if err != nil {
			return results, err
		}
		probe := windowsV3OutputProbe{root: root, rootName: name, directory: directory, rootPresent: true}
		if err := errors.Join(directory.Sync(), root.Sync()); err != nil {
			results.safePublish = err
			results.operationRecovery = windowsV3ProbeDependencyFailure("private probe namespace", err)
			results.rangeRecovery = windowsV3ProbeDependencyFailure("private probe namespace", err)
			results.crashCleanup = windowsV3ProbeDependencyFailure("private probe namespace", err)
		} else {
			results = probe.runCapabilityFacts()
		}
		cleanupErr := probe.cleanup()
		if cleanupErr != nil {
			return results, windowsV3Failure(operation, name, errWindowsV3OutputUnsafe,
				errors.Join(errors.New("probe fixed namespace could not be removed"), cleanupErr))
		}
		if results.operationRecovery == nil {
			results.operationRecovery = root.preparePersistentRootIdentity()
			if results.operationRecovery != nil && results.rangeRecovery == nil {
				results.rangeRecovery = windowsV3ProbeDependencyFailure(
					"persistent root identity", results.operationRecovery,
				)
			}
		}
		return results, nil
	}
	return results, windowsV3Failure(operation, "", errWindowsV3OutputUnsafe,
		errors.New("could not allocate a unique fixed probe namespace"))
}

func newWindowsV3OutputProbeName(random io.Reader) (string, error) {
	var nonce [windowsV3OutputProbeRandomBytes]byte
	if _, err := io.ReadFull(random, nonce[:]); err != nil {
		return "", err
	}
	return windowsV3OutputProbePrefix + hex.EncodeToString(nonce[:]), nil
}

type windowsV3OutputProbe struct {
	root      *windowsV3Directory
	rootName  string
	directory *windowsV3Directory

	stage       *windowsV3File
	anchor      *windowsV3File
	publication *windowsV3File
	oldRecord   *windowsV3File
	newRecord   *windowsV3File
	installed   *windowsV3Directory
	collision   *windowsV3Directory

	rootPresent         bool
	stageMayExist       bool
	anchorMayExist      bool
	publicationMayExist bool
	recordMayExist      bool
	temporaryMayExist   bool
	installedMayExist   bool
	candidateMayExist   bool
	collisionMayExist   bool
}

func (probe *windowsV3OutputProbe) runCapabilityFacts() (results windowsV3CapabilityProbeResults) {
	results.safePublish = probe.probeSafePublish()
	if results.safePublish != nil {
		results.operationRecovery = windowsV3ProbeDependencyFailure("safe publication", results.safePublish)
		results.rangeRecovery = windowsV3ProbeDependencyFailure("safe publication", results.safePublish)
		results.crashCleanup = windowsV3ProbeDependencyFailure("private stage creation", results.safePublish)
		return results
	}
	results.rangeRecovery = probe.probeRangeRecovery()
	if results.rangeRecovery != nil {
		results.operationRecovery = windowsV3ProbeDependencyFailure("private record replacement", results.rangeRecovery)
		results.crashCleanup = windowsV3ProbeDependencyFailure("fixed private records", results.rangeRecovery)
		return results
	}
	results.operationRecovery = probe.probeOperationRecovery()
	if results.operationRecovery != nil {
		results.crashCleanup = windowsV3ProbeDependencyFailure("fixed operation state", results.operationRecovery)
		return results
	}
	results.crashCleanup = probe.probeCrashCleanup()
	return results
}

func windowsV3ProbeDependencyFailure(dependency string, cause error) error {
	return windowsV3Failure(
		"probe Windows destination capability", "", errWindowsV3OutputUnsupported,
		errors.Join(errors.New("required dependency could not be proven: "+dependency), cause),
	)
}

func (probe *windowsV3OutputProbe) probeSafePublish() error {
	const operation = "probe Windows safe publication"
	stage, err := probe.directory.CreatePrivateFile("stage")
	if err != nil {
		return err
	}
	probe.stage, probe.stageMayExist = stage, true
	if err := stage.Truncate(1); err != nil {
		return err
	}
	if err := errors.Join(stage.Sync(), probe.directory.Sync()); err != nil {
		return err
	}

	probe.anchorMayExist = true
	anchor, err := probe.directory.LinkRegularFileNoReplace(stage, "anchor")
	if err != nil {
		return err
	}
	probe.anchor = anchor
	probe.publicationMayExist = true
	publication, err := probe.directory.LinkRegularFileNoReplace(anchor, "publication")
	if err != nil {
		return err
	}
	probe.publication = publication
	if err := probe.directory.Sync(); err != nil {
		return err
	}
	if unexpected, err := probe.directory.LinkRegularFileNoReplace(anchor, "publication"); !errors.Is(err, errWindowsV3OutputCollision) {
		if unexpected != nil {
			_ = unexpected.Close()
		}
		return windowsV3Failure(operation, "publication", errWindowsV3OutputUnsupported,
			errors.Join(errors.New("hard-link publication is not atomic no-replace"), err))
	}
	return nil
}

func (probe *windowsV3OutputProbe) probeRangeRecovery() error {
	oldRecord, err := probe.directory.CreatePrivateFile("record")
	if err != nil {
		return err
	}
	probe.oldRecord, probe.recordMayExist = oldRecord, true
	if err := oldRecord.Sync(); err != nil {
		return err
	}
	// NTFS atomic replacement requires all readers of the target name to be
	// closed even when they shared delete. The state engine follows the same
	// write-temp, close-old, rename, reopen-verify sequence.
	if err := oldRecord.Close(); err != nil {
		return err
	}
	probe.oldRecord = nil
	newRecord, err := probe.directory.CreatePrivateFile("record.tmp")
	if err != nil {
		return err
	}
	probe.newRecord, probe.temporaryMayExist = newRecord, true
	if err := newRecord.Truncate(1); err != nil {
		return err
	}
	if err := newRecord.Sync(); err != nil {
		return err
	}
	if err := probe.directory.AtomicReplacePrivateFile(newRecord, "record"); err != nil {
		return err
	}
	probe.temporaryMayExist = false
	probe.recordMayExist = true
	if err := probe.directory.Sync(); err != nil {
		return err
	}
	return nil
}

func (probe *windowsV3OutputProbe) probeOperationRecovery() error {
	const operation = "probe Windows operation recovery"
	installed, err := probe.directory.CreatePrivateDirectory("candidate")
	if err != nil {
		return err
	}
	probe.installed, probe.candidateMayExist = installed, true
	if err := errors.Join(installed.Sync(), probe.directory.Sync()); err != nil {
		return err
	}
	probe.installedMayExist = true
	installedHandle, err := probe.directory.InstallPrivateDirectoryNoReplace(installed, "installed")
	if err != nil {
		return err
	}
	probe.candidateMayExist = false
	if err := installedHandle.Close(); err != nil {
		return err
	}
	if err := probe.directory.Sync(); err != nil {
		return err
	}

	collision, err := probe.directory.CreatePrivateDirectory("candidate")
	if err != nil {
		return err
	}
	probe.collision, probe.collisionMayExist = collision, true
	if unexpected, err := probe.directory.InstallPrivateDirectoryNoReplace(collision, "installed"); !errors.Is(err, errWindowsV3OutputCollision) {
		if unexpected != nil {
			_ = unexpected.Close()
		}
		return windowsV3Failure(operation, "installed", errWindowsV3OutputUnsupported,
			errors.Join(errors.New("directory installation is not atomic no-replace"), err))
	}
	return nil
}

func (probe *windowsV3OutputProbe) probeCrashCleanup() error {
	const operation = "probe Windows crash cleanup"
	if err := probe.directory.verify(true); err != nil {
		return err
	}
	for name, file := range map[string]*windowsV3File{
		"stage": probe.stage, "anchor": probe.anchor, "publication": probe.publication,
		"record": probe.newRecord,
	} {
		current, err := probe.directory.OpenPrivateFile(name)
		if err != nil {
			return err
		}
		same, compareErr := sameWindowsV3OpenedObject(current, file)
		closeErr := current.Close()
		if compareErr != nil || closeErr != nil || !same {
			return errors.Join(
				windowsV3Failure(operation, name, errWindowsV3OutputUnsafe,
					errors.New("private cleanup name does not identify its fixed owned object")),
				compareErr, closeErr,
			)
		}
	}
	return nil
}

func (probe *windowsV3OutputProbe) cleanup() error {
	if probe == nil {
		return nil
	}
	if probe.directory == nil {
		return probe.closeChildHandles()
	}
	if err := probe.cleanupEntries(); err != nil {
		return errors.Join(err, probe.closeChildHandles(), closeWindowsV3ProbeDirectory(probe.directory))
	}
	if err := probe.closeChildHandles(); err != nil {
		return errors.Join(err, closeWindowsV3ProbeDirectory(probe.directory))
	}
	if probe.rootPresent && probe.root != nil {
		if err := probe.root.RemoveDirectory(probe.rootName, probe.directory); err != nil {
			return errors.Join(err, closeWindowsV3ProbeDirectory(probe.directory))
		}
		if err := probe.root.Sync(); err != nil {
			return errors.Join(err, closeWindowsV3ProbeDirectory(probe.directory))
		}
		probe.rootPresent = false
	}
	return closeWindowsV3ProbeDirectory(probe.directory)
}

func (probe *windowsV3OutputProbe) cleanupEntries() error {
	removeRegular := func(present *bool, name string, candidates ...*windowsV3File) error {
		if !*present {
			return nil
		}
		if err := probe.removeKnownRegular(name, candidates...); err != nil {
			return err
		}
		*present = false
		return probe.directory.Sync()
	}
	removeDirectory := func(present *bool, name string, candidates ...*windowsV3Directory) error {
		if !*present {
			return nil
		}
		if err := probe.removeKnownDirectory(name, candidates...); err != nil {
			return err
		}
		*present = false
		return probe.directory.Sync()
	}
	if err := removeRegular(&probe.stageMayExist, "stage", probe.stage); err != nil {
		return err
	}
	if err := removeRegular(
		&probe.publicationMayExist, "publication", probe.publication, probe.anchor, probe.stage,
	); err != nil {
		return err
	}
	if err := removeRegular(&probe.anchorMayExist, "anchor", probe.anchor, probe.stage); err != nil {
		return err
	}
	if err := removeRegular(&probe.temporaryMayExist, "record.tmp", probe.newRecord); err != nil {
		return err
	}
	if err := removeRegular(&probe.recordMayExist, "record", probe.newRecord, probe.oldRecord); err != nil {
		return err
	}
	if err := removeDirectory(&probe.collisionMayExist, "candidate", probe.collision); err != nil {
		return err
	}
	if err := removeDirectory(&probe.candidateMayExist, "candidate", probe.installed); err != nil {
		return err
	}
	return removeDirectory(&probe.installedMayExist, "installed", probe.installed, probe.collision)
}

func (probe *windowsV3OutputProbe) removeKnownRegular(
	name string,
	candidates ...*windowsV3File,
) (resultErr error) {
	current, err := probe.directory.OpenRegularFile(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, current.Close()) }()
	for _, candidate := range candidates {
		if candidate == nil || candidate.file == nil {
			continue
		}
		same, compareErr := sameWindowsV3OpenedObject(current, candidate)
		if compareErr != nil {
			return compareErr
		}
		if same {
			return probe.directory.RemoveRegularLink(name, candidate)
		}
	}
	return windowsV3Failure("clean Windows output probe", name, errWindowsV3OutputUnsafe,
		errors.New("entry does not match any probe-created regular file"))
}

func (probe *windowsV3OutputProbe) removeKnownDirectory(
	name string,
	candidates ...*windowsV3Directory,
) (resultErr error) {
	current, err := probe.directory.OpenDirectory(name)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, current.Close()) }()
	for _, candidate := range candidates {
		if candidate == nil || candidate.file == nil {
			continue
		}
		same, compareErr := sameWindowsV3OpenedDirectory(current, candidate)
		if compareErr != nil {
			return compareErr
		}
		if same {
			return probe.directory.RemoveDirectory(name, candidate)
		}
	}
	return windowsV3Failure("clean Windows output probe", name, errWindowsV3OutputUnsafe,
		errors.New("entry does not match any probe-created directory"))
}

func (probe *windowsV3OutputProbe) closeChildHandles() error {
	return errors.Join(
		closeWindowsV3ProbeFile(probe.stage),
		closeWindowsV3ProbeFile(probe.anchor),
		closeWindowsV3ProbeFile(probe.publication),
		closeWindowsV3ProbeFile(probe.oldRecord),
		closeWindowsV3ProbeFile(probe.newRecord),
		closeWindowsV3ProbeDirectory(probe.installed),
		closeWindowsV3ProbeDirectory(probe.collision),
	)
}

func closeWindowsV3ProbeFile(file *windowsV3File) error {
	if file == nil {
		return nil
	}
	return file.Close()
}

func closeWindowsV3ProbeDirectory(directory *windowsV3Directory) error {
	if directory == nil {
		return nil
	}
	return directory.Close()
}
