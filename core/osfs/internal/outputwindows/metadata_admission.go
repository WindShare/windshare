//go:build windows

package outputwindows

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"runtime"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	"golang.org/x/sys/windows"
)

const (
	windowsV3MetadataProbePrefix             = ".windshare-output.metadata-probe-"
	windowsV3MetadataProbeRandomBytes        = 16
	windowsV3MetadataProbeAllocationAttempts = 16
)

type windowsV3MetadataTimeWitness struct {
	modified catalog.ModifiedTime
	ticks    uint64
}

type windowsV3MetadataTimeBounds struct {
	minimum windowsV3MetadataTimeWitness
	maximum windowsV3MetadataTimeWitness
	present bool
}

type windowsV3MetadataProbePlan struct {
	maximumSize uint64
	hasSize     bool
	times       []windowsV3MetadataTimeWitness
}

func (plan windowsV3MetadataProbePlan) empty() bool {
	return !plan.hasSize && len(plan.times) == 0
}

// The executor boundary keeps selection cardinality out of native I/O. The
// planner may inspect every authenticated claim, while this interface can be
// invoked at most once for size and twice for each supported time precision.
type windowsV3MetadataProbeExecutor interface {
	ProbeSize(*windowsV3File, uint64) error
	ProbeModifiedTime(*windowsV3File, uint64, windowsV3MetadataTimeWitness) error
}

type windowsV3NativeMetadataProbeExecutor struct{}

func (root *windowsV3Directory) validateSelectionMetadata(
	selection transfer.OutputSelection,
) error {
	return root.validateSelectionMetadataWithExecutor(selection, windowsV3NativeMetadataProbeExecutor{})
}

func (root *windowsV3Directory) validateSelectionMetadataWithExecutor(
	selection transfer.OutputSelection,
	executor windowsV3MetadataProbeExecutor,
) (resultErr error) {
	plan, err := windowsV3PlanSelectionMetadata(selection)
	if err != nil || plan.empty() {
		return err
	}
	if executor == nil {
		return windowsV3Failure("validate Windows output selection metadata", "", errWindowsV3OutputUnsafe,
			errors.New("metadata probe executor is absent"))
	}
	// The probe mutex is derived from the persistent root identity. Preparing it
	// on this exact guarded handle keeps File-ID reuse from turning an earlier
	// authority into a lookup cache while leaving metadata-free selections inert.
	if _, err := root.prepareIdentityClaim(); err != nil {
		return err
	}

	lock, err := root.acquireOutputProbeLock()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, root.releaseOutputProbeLock(lock)) }()
	probe, probeName, err := root.createMetadataProbe(rand.Reader)
	if err != nil {
		return err
	}
	defer func() {
		resultErr = errors.Join(resultErr, root.closeMetadataProbeAndVerifyAbsent(
			windows.InvalidHandle, probe, probeName,
		))
	}()

	return executeWindowsV3MetadataProbe(probe, plan, executor)
}

func windowsV3PlanSelectionMetadata(selection transfer.OutputSelection) (windowsV3MetadataProbePlan, error) {
	const operation = "validate Windows output selection metadata"
	var bounds [3]windowsV3MetadataTimeBounds
	addTime := func(path string, modified catalog.ModifiedTime) error {
		ticks, present, err := windowsV3ModifiedTimeTicks(modified)
		if err != nil {
			return windowsV3Failure(operation, path, errWindowsV3OutputUnsupported, err)
		}
		if !present {
			return nil
		}
		index, err := windowsV3MetadataPrecisionIndex(modified.Precision())
		if err != nil {
			return windowsV3Failure(operation, path, errWindowsV3OutputUnsupported, err)
		}
		witness := windowsV3MetadataTimeWitness{modified: modified, ticks: ticks}
		if !bounds[index].present {
			bounds[index] = windowsV3MetadataTimeBounds{minimum: witness, maximum: witness, present: true}
			return nil
		}
		if ticks < bounds[index].minimum.ticks {
			bounds[index].minimum = witness
		}
		if ticks > bounds[index].maximum.ticks {
			bounds[index].maximum = witness
		}
		return nil
	}

	plan := windowsV3MetadataProbePlan{times: make([]windowsV3MetadataTimeWitness, 0, len(bounds)*2)}
	if err := selection.VisitDirectories(func(directory transfer.OutputSelectionDirectory) error {
		return addTime(directory.Path, directory.ModifiedTime)
	}); err != nil {
		return windowsV3MetadataProbePlan{}, err
	}
	if err := selection.VisitFiles(func(file transfer.OutputSelectionFile) error {
		if file.ExpectedSize > math.MaxInt64 {
			return windowsV3Failure(operation, file.Path, errWindowsV3OutputUnsupported,
				errors.New("logical size exceeds the native signed file-offset range"))
		}
		plan.hasSize = true
		if file.ExpectedSize > plan.maximumSize {
			plan.maximumSize = file.ExpectedSize
		}
		if err := addTime(file.Path, file.ModifiedTime); err != nil {
			return err
		}
		return nil
	}); err != nil {
		return windowsV3MetadataProbePlan{}, err
	}
	for _, current := range bounds {
		if !current.present {
			continue
		}
		plan.times = append(plan.times, current.minimum)
		if current.maximum.ticks != current.minimum.ticks {
			plan.times = append(plan.times, current.maximum)
		}
	}
	return plan, nil
}

func windowsV3MetadataPrecisionIndex(precision catalog.TimePrecision) (int, error) {
	switch precision {
	case catalog.TimePrecisionSeconds:
		return 0, nil
	case catalog.TimePrecisionMilliseconds:
		return 1, nil
	case catalog.TimePrecisionNanoseconds:
		return 2, nil
	default:
		return 0, errors.New("catalog modified-time precision has no native witness class")
	}
}

func executeWindowsV3MetadataProbe(
	probe *windowsV3File,
	plan windowsV3MetadataProbePlan,
	executor windowsV3MetadataProbeExecutor,
) error {
	if executor == nil {
		return windowsV3Failure("execute Windows metadata admission probe", "", errWindowsV3OutputUnsafe,
			errors.New("metadata probe executor is absent"))
	}
	if plan.hasSize {
		if err := executor.ProbeSize(probe, plan.maximumSize); err != nil {
			return err
		}
	}
	for _, witness := range plan.times {
		if err := executor.ProbeModifiedTime(probe, plan.maximumSize, witness); err != nil {
			return err
		}
	}
	return nil
}

func (root *windowsV3Directory) createMetadataProbe(
	random io.Reader,
) (*windowsV3File, string, error) {
	const operation = "create Windows metadata admission probe"
	if random == nil {
		return nil, "", windowsV3Failure(operation, "", errWindowsV3OutputUnsafe,
			errors.New("random source is absent"))
	}
	descriptor, err := root.policy.descriptor(false)
	if err != nil {
		return nil, "", windowsV3Failure(operation, "", errWindowsV3OutputUnsafe, err)
	}
	for range windowsV3MetadataProbeAllocationAttempts {
		var nonce [windowsV3MetadataProbeRandomBytes]byte
		if _, err := io.ReadFull(random, nonce[:]); err != nil {
			return nil, "", windowsV3Failure(operation, "", errWindowsV3OutputUnsafe, err)
		}
		name := windowsV3MetadataProbePrefix + hex.EncodeToString(nonce[:])
		handle, status, err := windowsV3OpenNativeWithOptions(
			root.handle(), name, windowsV3PrivateFileAccess(), windows.FILE_CREATE,
			windows.FILE_NON_DIRECTORY_FILE|windows.FILE_DELETE_ON_CLOSE,
			windows.FILE_ATTRIBUTE_HIDDEN, descriptor,
			windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
			windows.OBJ_CASE_INSENSITIVE|windows.OBJ_DONT_REPARSE,
		)
		runtime.KeepAlive(descriptor)
		if windowsV3IsCollision(err) {
			continue
		}
		if err != nil {
			return nil, "", windowsV3NativeOperationFailure(operation, name, err)
		}
		created, statusErr := windowsV3CreationStatus(windows.FILE_CREATE, status)
		if statusErr != nil || !created {
			return nil, "", errors.Join(
				windowsV3Failure(operation, name, errWindowsV3OutputUnsafe, statusErr),
				root.closeMetadataProbeAndVerifyAbsent(handle, nil, name),
			)
		}
		wrapped := os.NewFile(uintptr(handle), name)
		if wrapped == nil {
			return nil, "", errors.Join(
				windowsV3Failure(operation, name, errWindowsV3OutputUnsafe, errors.New("wrap probe handle")),
				root.closeMetadataProbeAndVerifyAbsent(handle, nil, name),
			)
		}
		probe := &windowsV3File{
			file: wrapped, path: filepath.Join(root.path, name), volume: root.volume,
			inspector: root.inspector, policy: root.policy,
		}
		if err := probe.verify(true); err != nil {
			return nil, "", errors.Join(err,
				root.closeMetadataProbeAndVerifyAbsent(windows.InvalidHandle, probe, name))
		}
		if err := windowsV3VerifyOpenedLeafAuthority(probe.handle(), name, true); err != nil {
			return nil, "", errors.Join(err,
				root.closeMetadataProbeAndVerifyAbsent(windows.InvalidHandle, probe, name))
		}
		var returned uint32
		if err := windows.DeviceIoControl(
			probe.handle(), windows.FSCTL_SET_SPARSE, nil, 0, nil, 0, &returned, nil,
		); err != nil {
			return nil, "", errors.Join(
				windowsV3NativeOperationFailure(operation, name,
					errors.Join(errors.New("NTFS sparse metadata probe is unavailable"), err)),
				root.closeMetadataProbeAndVerifyAbsent(windows.InvalidHandle, probe, name),
			)
		}
		return probe, name, nil
	}
	return nil, "", windowsV3Failure(operation, "", errWindowsV3OutputUnsafe,
		errors.New("could not allocate a unique metadata probe name"))
}

func (windowsV3NativeMetadataProbeExecutor) ProbeSize(probe *windowsV3File, size uint64) error {
	const operation = "exercise Windows metadata size representation"
	if probe == nil || probe.file == nil {
		return windowsV3Failure(operation, "", errWindowsV3OutputUnsafe, errors.New("metadata probe is absent"))
	}
	if err := probe.Truncate(int64(size)); err != nil {
		return windowsV3NativeOperationFailure(operation, probe.path, err)
	}
	if err := probe.Sync(); err != nil {
		return err
	}
	metadata, err := readWindowsV3MetadataProbe(probe)
	if err != nil || metadata.size != size {
		return windowsV3Failure(operation, probe.path, errWindowsV3OutputUnsupported,
			errors.Join(fmt.Errorf("NTFS size readback differs: size=%d expected=%d", metadata.size, size), err))
	}
	return nil
}

func (windowsV3NativeMetadataProbeExecutor) ProbeModifiedTime(
	probe *windowsV3File,
	expectedSize uint64,
	witness windowsV3MetadataTimeWitness,
) error {
	const operation = "exercise Windows metadata time representation"
	if probe == nil || probe.file == nil {
		return windowsV3Failure(operation, "", errWindowsV3OutputUnsafe, errors.New("metadata probe is absent"))
	}
	if err := probe.setModifiedTime(witness.modified); err != nil {
		return err
	}
	if err := probe.Sync(); err != nil {
		return err
	}
	metadata, err := readWindowsV3MetadataProbe(probe)
	if err != nil || metadata.size != expectedSize || metadata.modifiedTicks != witness.ticks {
		return windowsV3Failure(operation, probe.path, errWindowsV3OutputUnsupported,
			errors.Join(
				fmt.Errorf("NTFS metadata readback differs: size=%d expected=%d time=%d expected=%d",
					metadata.size, expectedSize, metadata.modifiedTicks, witness.ticks),
				err,
			))
	}
	return nil
}

func readWindowsV3MetadataProbe(probe *windowsV3File) (windowsV3OutputMetadata, error) {
	reopened, err := duplicateWindowsV3MetadataProbe(probe)
	if err != nil {
		return windowsV3OutputMetadata{}, err
	}
	metadata, metadataErr := windowsV3ReadHandleMetadata(reopened.handle())
	closeErr := reopened.Close()
	return metadata, errors.Join(metadataErr, closeErr)
}

func (root *windowsV3Directory) closeMetadataProbeAndVerifyAbsent(
	rawHandle windows.Handle,
	probe *windowsV3File,
	name string,
) error {
	const operation = "retire Windows metadata admission probe"
	var closeErr error
	switch {
	case probe != nil:
		closeErr = probe.Close()
	case rawHandle != windows.InvalidHandle && rawHandle != 0:
		closeErr = windows.CloseHandle(rawHandle)
	default:
		closeErr = errors.New("metadata probe close authority is absent")
	}
	syncErr := root.Sync()
	kind, exact, observeErr := root.classifyExactEntry(name)
	if observeErr == nil && (kind != outputcap.EntryAbsent || !exact) {
		observeErr = windowsV3Failure(operation, name, errWindowsV3OutputUnsafe,
			fmt.Errorf("delete-on-close metadata probe remained visible as kind=%d exact=%t", kind, exact))
	}
	return errors.Join(closeErr, syncErr, observeErr)
}

func duplicateWindowsV3MetadataProbe(probe *windowsV3File) (*windowsV3File, error) {
	const operation = "reopen Windows metadata admission probe"
	if probe == nil || probe.file == nil {
		return nil, windowsV3Failure(operation, "", errWindowsV3OutputUnsafe,
			errors.New("probe handle is absent"))
	}
	var handle windows.Handle
	process := windows.CurrentProcess()
	if err := windows.DuplicateHandle(
		process, probe.handle(), process, &handle, 0, false, windows.DUPLICATE_SAME_ACCESS,
	); err != nil {
		return nil, windowsV3NativeOperationFailure(operation, probe.path, err)
	}
	wrapped := os.NewFile(uintptr(handle), probe.path)
	if wrapped == nil {
		return nil, errors.Join(
			windowsV3Failure(operation, probe.path, errWindowsV3OutputUnsafe, errors.New("wrap reopened probe handle")),
			windows.CloseHandle(handle),
		)
	}
	reopened := &windowsV3File{
		file: wrapped, path: probe.path, volume: probe.volume, inspector: probe.inspector, policy: probe.policy,
	}
	if err := reopened.verify(true); err != nil {
		return nil, errors.Join(err, reopened.Close())
	}
	return reopened, nil
}
