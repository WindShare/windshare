//go:build linux

package outputlinux

import (
	"errors"
	"fmt"
	"math"
	"sort"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/osfs/internal/outputcap"
	"github.com/windshare/windshare/core/transfer"
	"golang.org/x/sys/unix"
)

func (platform *linuxV3Platform) ValidateSelectionMetadata(selection transfer.OutputSelection) error {
	if platform == nil || platform.root == nil || platform.root.native == nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux output platform is closed"))
	}
	if platform.root.metadataPolicy != nil {
		return errors.Join(outputcap.ErrUnsafeNamespace, errors.New("osfs: Linux selection metadata policy is already bound"))
	}
	if err := platform.root.native.validateSelectionMetadata(selection); err != nil {
		return linuxV3Error(err)
	}
	// The policy becomes immutable before materialization. This lets the existing
	// post-probe authority boundary prove the layout of each exact directory inode
	// without widening the transport-neutral output interface.
	platform.root.metadataPolicy = linuxSelectionMetadataPolicyFor(selection)
	return nil
}

type linuxSelectionMetadataPolicy struct {
	extendedTimestampDirectories []string
}

func linuxSelectionMetadataPolicyFor(selection transfer.OutputSelection) *linuxSelectionMetadataPolicy {
	paths := make([]string, 0, len(selection.Directories()))
	for _, directory := range selection.Directories() {
		if linuxModifiedTimeRequiresExtendedInodeFields(directory.ModifiedTime) {
			paths = append(paths, directory.Path)
		}
	}
	sort.Strings(paths)
	return &linuxSelectionMetadataPolicy{extendedTimestampDirectories: paths}
}

func (policy *linuxSelectionMetadataPolicy) requiresExtendedTimestamp(path string) bool {
	if policy == nil {
		return false
	}
	index := sort.SearchStrings(policy.extendedTimestampDirectories, path)
	return index < len(policy.extendedTimestampDirectories) &&
		policy.extendedTimestampDirectories[index] == path
}

type linuxSelectionMetadataRequirements struct {
	maximumSize               uint64
	hasSize                   bool
	timeBounds                map[catalog.TimePrecision]linuxModifiedTimeBounds
	requiresExtendedTimestamp bool
}

func linuxSelectionMetadataRequirementsFor(
	selection transfer.OutputSelection,
) (linuxSelectionMetadataRequirements, error) {
	const operation = "validate ext4 selection metadata"
	requirements := linuxSelectionMetadataRequirements{
		timeBounds: make(map[catalog.TimePrecision]linuxModifiedTimeBounds, 3),
	}
	for _, directory := range selection.Directories() {
		if err := requirements.observeModifiedTime(directory.ModifiedTime); err != nil {
			return linuxSelectionMetadataRequirements{}, err
		}
	}
	for _, file := range selection.Files() {
		if file.ExpectedSize > math.MaxInt64 {
			return linuxSelectionMetadataRequirements{}, linuxUnsupported(
				operation, "selected file size exceeds the native truncate ABI", nil,
			)
		}
		if !requirements.hasSize || file.ExpectedSize > requirements.maximumSize {
			requirements.maximumSize = file.ExpectedSize
			requirements.hasSize = true
		}
		if err := requirements.observeModifiedTime(file.ModifiedTime); err != nil {
			return linuxSelectionMetadataRequirements{}, err
		}
	}
	return requirements, nil
}

func (requirements *linuxSelectionMetadataRequirements) observeModifiedTime(
	modified catalog.ModifiedTime,
) error {
	if err := linuxValidateModifiedTime(modified); err != nil {
		return err
	}
	if !modified.Present() {
		return nil
	}
	requirements.requiresExtendedTimestamp = requirements.requiresExtendedTimestamp ||
		linuxModifiedTimeRequiresExtendedInodeFields(modified)
	bounds := requirements.timeBounds[modified.Precision()]
	bounds.observe(modified)
	requirements.timeBounds[modified.Precision()] = bounds
	return nil
}

func (root *linuxOutputDirectory) validateSelectionMetadata(selection transfer.OutputSelection) (resultErr error) {
	if err := root.verifyHandle(); err != nil {
		return err
	}
	requirements, err := linuxSelectionMetadataRequirementsFor(selection)
	if err != nil {
		return err
	}
	modifiedTimes := linuxModifiedTimeWitnesses(requirements.timeBounds)
	if !requirements.hasSize && len(modifiedTimes) == 0 {
		return nil
	}
	if err := root.validateMetadataProbeProviders(); err != nil {
		return err
	}

	fd, err := root.openAnonymousMetadataProbe()
	if err != nil {
		return err
	}
	defer func() { resultErr = errors.Join(resultErr, root.system.close(fd)) }()
	probeIdentity, err := root.validateAnonymousMetadataProbe(
		fd, requirements.requiresExtendedTimestamp,
	)
	if err != nil {
		return err
	}
	if requirements.hasSize {
		if err := root.roundTripSelectedFileSize(fd, requirements.maximumSize); err != nil {
			return err
		}
	}
	if err := root.roundTripSelectedModifiedTimes(fd, modifiedTimes); err != nil {
		return err
	}
	currentIdentity, err := linuxVerifyOpenObject(root.system, fd, root.certificate)
	if err != nil {
		return err
	}
	if !probeIdentity.identity.sameObject(currentIdentity.identity) {
		return linuxUnsafe("validate ext4 selection metadata",
			"anonymous metadata probe identity changed", nil)
	}
	return root.verifyHandle()
}

func (root *linuxOutputDirectory) validateMetadataProbeProviders() error {
	const operation = "validate ext4 selection metadata"
	if root.system == nil || root.system.openat2 == nil || root.system.close == nil ||
		root.system.ftruncate == nil || root.system.utimensat == nil || root.system.statx == nil ||
		root.system.fsync == nil {
		return linuxUnsupported(operation, "native anonymous metadata probe provider is incomplete", nil)
	}
	return nil
}

func (root *linuxOutputDirectory) openAnonymousMetadataProbe() (int, error) {
	const operation = "validate ext4 selection metadata"
	how := unix.OpenHow{
		Flags: uint64(unix.O_TMPFILE | unix.O_RDWR | unix.O_CLOEXEC | unix.O_LARGEFILE),
		Mode:  linuxOutputStateFileMode,
		Resolve: uint64(
			unix.RESOLVE_BENEATH | unix.RESOLVE_NO_MAGICLINKS | unix.RESOLVE_NO_SYMLINKS,
		),
	}
	fd, err := root.system.openat2(root.fd, ".", &how)
	if err != nil {
		return -1, linuxUnsupported(
			operation, "ext4 anonymous inode metadata probing is unavailable", err,
		)
	}
	return fd, nil
}

func (root *linuxOutputDirectory) validateAnonymousMetadataProbe(
	fd int,
	requiresExtendedTimestamp bool,
) (linuxOpenHandleFacts, error) {
	const operation = "validate ext4 selection metadata"
	identity, err := linuxVerifyOpenObject(root.system, fd, root.certificate)
	if err != nil {
		return linuxOpenHandleFacts{}, err
	}
	if linuxFileType(identity.mode) != unix.S_IFREG {
		return linuxOpenHandleFacts{}, linuxUnsafe(
			operation, "anonymous metadata probe is not a regular file", nil,
		)
	}
	if requiresExtendedTimestamp {
		if err := linuxRequireExtendedTimestampLayout(
			root.system, fd, root.certificate, unix.S_IFREG, operation,
		); err != nil {
			return linuxOpenHandleFacts{}, err
		}
	}
	return identity, nil
}

func (root *linuxOutputDirectory) roundTripSelectedFileSize(fd int, maximumSize uint64) error {
	const operation = "validate ext4 selection metadata"
	if err := root.system.ftruncate(fd, int64(maximumSize)); err != nil {
		return linuxUnsupported(
			operation, fmt.Sprintf("selected file size %d is not representable", maximumSize), err,
		)
	}
	if err := linuxSyncOutputHandle(root.system, fd, operation); err != nil {
		return err
	}
	metadata, err := linuxReadHandleMetadata(root.system, fd, root.certificate, unix.S_IFREG)
	if err != nil {
		return err
	}
	if metadata.size != maximumSize {
		return linuxUnsupported(
			operation,
			fmt.Sprintf("selected file size %d did not round-trip exactly", maximumSize),
			nil,
		)
	}
	return nil
}

func (root *linuxOutputDirectory) roundTripSelectedModifiedTimes(
	fd int,
	modifiedTimes []catalog.ModifiedTime,
) error {
	const operation = "validate ext4 selection metadata"
	for _, modified := range modifiedTimes {
		if err := linuxSetHandleModifiedTime(root.system, fd, modified, operation); err != nil {
			return err
		}
		// BTIME proves that this ext4 inode has room for mtime_extra. Fsync then
		// forces the exact value through ext4's raw-inode encoder before accepting
		// the same handle's observation as restart-safe.
		if err := linuxSyncOutputHandle(root.system, fd, operation); err != nil {
			return err
		}
		metadata, err := linuxReadHandleMetadata(root.system, fd, root.certificate, unix.S_IFREG)
		if err != nil {
			return err
		}
		if !linuxModifiedTimeMatches(metadata, modified) {
			return linuxUnsupported(
				operation,
				"selected modified time did not round-trip at its declared precision",
				nil,
			)
		}
	}
	return nil
}

type linuxModifiedTimeBounds struct {
	minimum catalog.ModifiedTime
	maximum catalog.ModifiedTime
	present bool
}

func (bounds *linuxModifiedTimeBounds) observe(modified catalog.ModifiedTime) {
	if !bounds.present {
		bounds.minimum = modified
		bounds.maximum = modified
		bounds.present = true
		return
	}
	if linuxModifiedTimeBefore(modified, bounds.minimum) {
		bounds.minimum = modified
	}
	if linuxModifiedTimeBefore(bounds.maximum, modified) {
		bounds.maximum = modified
	}
}

func linuxModifiedTimeBefore(left, right catalog.ModifiedTime) bool {
	if left.Seconds() != right.Seconds() {
		return left.Seconds() < right.Seconds()
	}
	return left.Nanoseconds() < right.Nanoseconds()
}

func linuxModifiedTimeWitnesses(
	bounds map[catalog.TimePrecision]linuxModifiedTimeBounds,
) []catalog.ModifiedTime {
	witnesses := make([]catalog.ModifiedTime, 0, 6)
	for precision := catalog.TimePrecisionSeconds; precision <= catalog.TimePrecisionNanoseconds; precision++ {
		bounded := bounds[precision]
		if !bounded.present {
			continue
		}
		witnesses = append(witnesses, bounded.minimum)
		if bounded.maximum != bounded.minimum {
			witnesses = append(witnesses, bounded.maximum)
		}
	}
	return witnesses
}
