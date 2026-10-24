//go:build windows

package outputwindows

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"

	"github.com/windshare/windshare/osfs/internal/outputcap"
)

const (
	windowsOutputProbePrefix             = ".windshare-output.probe-"
	windowsOutputProbeRandomBytes        = 16
	windowsOutputProbeAllocationAttempts = 16
)

func (root *windowsDirectory) probeRecoverableFeatures() error {
	return root.probeRecoverableFeaturesWithRandom(rand.Reader)
}

func (root *windowsDirectory) destinationCapabilities() (outputcap.DestinationCapabilities, error) {
	results, err := root.probeDestinationCapabilitiesWithRandom(rand.Reader)
	if err != nil {
		return outputcap.DestinationCapabilities{}, err
	}
	return windowsDestinationCapabilitiesFromResults(results)
}

type windowsCapabilityProbeResults struct {
	safePublish       error
	operationRecovery error
	rangeRecovery     error
	crashCleanup      error
}

func windowsDestinationCapabilitiesFromResults(
	results windowsCapabilityProbeResults,
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
		if errors.Is(fact.err, errWindowsOutputUnsafe) {
			return outputcap.DestinationCapabilities{}, windowsOutputError(fact.err)
		}
		evidence[index], _ = outputcap.UnsupportedCapability(fact.reason)
	}
	return outputcap.NewDestinationCapabilities(evidence[0], evidence[1], evidence[2], evidence[3])
}

func (root *windowsDirectory) probeRecoverableFeaturesWithRandom(random io.Reader) error {
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

func (root *windowsDirectory) probeDestinationCapabilitiesWithRandom(
	random io.Reader,
) (results windowsCapabilityProbeResults, resultErr error) {
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
		return results, windowsFailure(operation, "", errWindowsOutputUnsafe, errors.New("random source is absent"))
	}
	for range windowsOutputProbeAllocationAttempts {
		name, err := newWindowsOutputProbeName(random)
		if err != nil {
			return results, windowsFailure(operation, "", errWindowsOutputUnsafe, err)
		}
		directory, err := root.CreatePrivateDirectory(name)
		if errors.Is(err, errWindowsOutputCollision) {
			continue
		}
		if err != nil {
			return results, err
		}
		probe := windowsOutputProbe{root: root, rootName: name, directory: directory, rootPresent: true}
		if err := errors.Join(directory.Sync(), root.Sync()); err != nil {
			results.safePublish = err
			results.operationRecovery = windowsProbeDependencyFailure("private probe namespace", err)
			results.rangeRecovery = windowsProbeDependencyFailure("private probe namespace", err)
			results.crashCleanup = windowsProbeDependencyFailure("private probe namespace", err)
		} else {
			results = probe.runCapabilityFacts()
		}
		cleanupErr := probe.cleanup()
		if cleanupErr != nil {
			return results, windowsFailure(operation, name, errWindowsOutputUnsafe,
				errors.Join(errors.New("probe fixed namespace could not be removed"), cleanupErr))
		}
		if results.operationRecovery == nil {
			results.operationRecovery = root.preparePersistentRootIdentity()
			if results.operationRecovery != nil && results.rangeRecovery == nil {
				results.rangeRecovery = windowsProbeDependencyFailure(
					"persistent root identity", results.operationRecovery,
				)
			}
		}
		return results, nil
	}
	return results, windowsFailure(operation, "", errWindowsOutputUnsafe,
		errors.New("could not allocate a unique fixed probe namespace"))
}

func newWindowsOutputProbeName(random io.Reader) (string, error) {
	var nonce [windowsOutputProbeRandomBytes]byte
	if _, err := io.ReadFull(random, nonce[:]); err != nil {
		return "", err
	}
	return windowsOutputProbePrefix + hex.EncodeToString(nonce[:]), nil
}

type windowsOutputProbe struct {
	root      *windowsDirectory
	rootName  string
	directory *windowsDirectory

	stage       *windowsFile
	anchor      *windowsFile
	publication *windowsFile
	oldRecord   *windowsFile
	newRecord   *windowsFile
	installed   *windowsDirectory
	collision   *windowsDirectory

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

func (probe *windowsOutputProbe) runCapabilityFacts() (results windowsCapabilityProbeResults) {
	results.safePublish = probe.probeSafePublish()
	if results.safePublish != nil {
		results.operationRecovery = windowsProbeDependencyFailure("safe publication", results.safePublish)
		results.rangeRecovery = windowsProbeDependencyFailure("safe publication", results.safePublish)
		results.crashCleanup = windowsProbeDependencyFailure("private stage creation", results.safePublish)
		return results
	}
	results.rangeRecovery = probe.probeRangeRecovery()
	if results.rangeRecovery != nil {
		results.operationRecovery = windowsProbeDependencyFailure("private record replacement", results.rangeRecovery)
		results.crashCleanup = windowsProbeDependencyFailure("fixed private records", results.rangeRecovery)
		return results
	}
	results.operationRecovery = probe.probeOperationRecovery()
	if results.operationRecovery != nil {
		results.crashCleanup = windowsProbeDependencyFailure("fixed operation state", results.operationRecovery)
		return results
	}
	results.crashCleanup = probe.probeCrashCleanup()
	return results
}

func windowsProbeDependencyFailure(dependency string, cause error) error {
	return windowsFailure(
		"probe Windows destination capability", "", errWindowsOutputUnsupported,
		errors.Join(errors.New("required dependency could not be proven: "+dependency), cause),
	)
}

func (probe *windowsOutputProbe) probeSafePublish() error {
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
	if unexpected, err := probe.directory.LinkRegularFileNoReplace(anchor, "publication"); !errors.Is(err, errWindowsOutputCollision) {
		if unexpected != nil {
			_ = unexpected.Close()
		}
		return windowsFailure(operation, "publication", errWindowsOutputUnsupported,
			errors.Join(errors.New("hard-link publication is not atomic no-replace"), err))
	}
	return nil
}

func (probe *windowsOutputProbe) probeRangeRecovery() error {
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

func (probe *windowsOutputProbe) probeOperationRecovery() error {
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
	if unexpected, err := probe.directory.InstallPrivateDirectoryNoReplace(collision, "installed"); !errors.Is(err, errWindowsOutputCollision) {
		if unexpected != nil {
			_ = unexpected.Close()
		}
		return windowsFailure(operation, "installed", errWindowsOutputUnsupported,
			errors.Join(errors.New("directory installation is not atomic no-replace"), err))
	}
	return nil
}

func (probe *windowsOutputProbe) probeCrashCleanup() error {
	const operation = "probe Windows crash cleanup"
	if err := probe.directory.verify(true); err != nil {
		return err
	}
	for name, file := range map[string]*windowsFile{
		"stage": probe.stage, "anchor": probe.anchor, "publication": probe.publication,
		"record": probe.newRecord,
	} {
		current, err := probe.directory.OpenPrivateFile(name)
		if err != nil {
			return err
		}
		same, compareErr := sameWindowsOpenedObject(current, file)
		closeErr := current.Close()
		if compareErr != nil || closeErr != nil || !same {
			return errors.Join(
				windowsFailure(operation, name, errWindowsOutputUnsafe,
					errors.New("private cleanup name does not identify its fixed owned object")),
				compareErr, closeErr,
			)
		}
	}
	return nil
}

func (probe *windowsOutputProbe) cleanup() error {
	if probe == nil {
		return nil
	}
	if probe.directory == nil {
		return probe.closeChildHandles()
	}
	if err := probe.cleanupEntries(); err != nil {
		return errors.Join(err, probe.closeChildHandles(), closeWindowsProbeDirectory(probe.directory))
	}
	if err := probe.closeChildHandles(); err != nil {
		return errors.Join(err, closeWindowsProbeDirectory(probe.directory))
	}
	if probe.rootPresent && probe.root != nil {
		if err := probe.root.RemoveDirectory(probe.rootName, probe.directory); err != nil {
			return errors.Join(err, closeWindowsProbeDirectory(probe.directory))
		}
		if err := probe.root.Sync(); err != nil {
			return errors.Join(err, closeWindowsProbeDirectory(probe.directory))
		}
		probe.rootPresent = false
	}
	return closeWindowsProbeDirectory(probe.directory)
}

func (probe *windowsOutputProbe) cleanupEntries() error {
	removeRegular := func(present *bool, name string, candidates ...*windowsFile) error {
		if !*present {
			return nil
		}
		if err := probe.removeKnownRegular(name, candidates...); err != nil {
			return err
		}
		*present = false
		return probe.directory.Sync()
	}
	removeDirectory := func(present *bool, name string, candidates ...*windowsDirectory) error {
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

func (probe *windowsOutputProbe) removeKnownRegular(
	name string,
	candidates ...*windowsFile,
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
		same, compareErr := sameWindowsOpenedObject(current, candidate)
		if compareErr != nil {
			return compareErr
		}
		if same {
			return probe.directory.RemoveRegularLink(name, candidate)
		}
	}
	return windowsFailure("clean Windows output probe", name, errWindowsOutputUnsafe,
		errors.New("entry does not match any probe-created regular file"))
}

func (probe *windowsOutputProbe) removeKnownDirectory(
	name string,
	candidates ...*windowsDirectory,
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
		same, compareErr := sameWindowsOpenedDirectory(current, candidate)
		if compareErr != nil {
			return compareErr
		}
		if same {
			return probe.directory.RemoveDirectory(name, candidate)
		}
	}
	return windowsFailure("clean Windows output probe", name, errWindowsOutputUnsafe,
		errors.New("entry does not match any probe-created directory"))
}

func (probe *windowsOutputProbe) closeChildHandles() error {
	return errors.Join(
		closeWindowsProbeFile(probe.stage),
		closeWindowsProbeFile(probe.anchor),
		closeWindowsProbeFile(probe.publication),
		closeWindowsProbeFile(probe.oldRecord),
		closeWindowsProbeFile(probe.newRecord),
		closeWindowsProbeDirectory(probe.installed),
		closeWindowsProbeDirectory(probe.collision),
	)
}

func closeWindowsProbeFile(file *windowsFile) error {
	if file == nil {
		return nil
	}
	return file.Close()
}

func closeWindowsProbeDirectory(directory *windowsDirectory) error {
	if directory == nil {
		return nil
	}
	return directory.Close()
}
