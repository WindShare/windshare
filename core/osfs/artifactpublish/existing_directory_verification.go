package artifactpublish

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"slices"
	"sort"

	"github.com/windshare/windshare/core/osfs/internal/outputcap"
)

const verificationBufferBytes = 64 << 10

func verifyExistingDirectoryTree(
	root outputcap.Directory,
	normalized normalizedExistingDirectory,
	syncFiles bool,
	snapshotPaths []string,
) (ExistingDirectoryResult, error) {
	snapshotSet := make(map[string]struct{}, len(snapshotPaths))
	for _, snapshotPath := range snapshotPaths {
		snapshotSet[snapshotPath] = struct{}{}
	}
	snapshots := make(map[string]ExistingDirectorySnapshot, len(snapshotPaths))
	if err := verifyExistingNode(root, normalized.tree, syncFiles, snapshotSet, snapshots); err != nil {
		return ExistingDirectoryResult{}, err
	}
	manifest := findExistingFile(normalized.inventory.Files, normalized.manifestPath)
	if manifest == nil {
		return ExistingDirectoryResult{}, unsafeError("locate sealed artifact manifest authority", nil)
	}
	orderedSnapshots := make([]ExistingDirectorySnapshot, 0, len(snapshotPaths))
	for _, snapshotPath := range snapshotPaths {
		snapshot, ok := snapshots[snapshotPath]
		if !ok {
			return ExistingDirectoryResult{}, unsafeError("return every requested sealed artifact snapshot", nil)
		}
		orderedSnapshots = append(orderedSnapshots, snapshot)
	}
	return ExistingDirectoryResult{ManifestSHA256: manifest.SHA256, Snapshots: orderedSnapshots}, nil
}

func verifyExistingNode(
	directory outputcap.Directory,
	node *existingDirectoryNode,
	syncFiles bool,
	snapshotPaths map[string]struct{},
	snapshots map[string]ExistingDirectorySnapshot,
) (resultErr error) {
	expectedNames := make([]string, 0, len(node.directories)+len(node.files))
	for name := range node.directories {
		expectedNames = append(expectedNames, name)
	}
	for name := range node.files {
		expectedNames = append(expectedNames, name)
	}
	sort.Strings(expectedNames)
	actualNames, err := directory.Names(len(expectedNames) + 1)
	if err != nil {
		return unsafeError("enumerate exact sealed artifact directory", err)
	}
	sort.Strings(actualNames)
	if !slices.Equal(actualNames, expectedNames) {
		return unsafeError("verify exact sealed artifact directory entries", nil)
	}
	for _, name := range expectedNames {
		if child, ok := node.directories[name]; ok {
			opened, err := directory.OpenDirectory(name, true)
			if err != nil {
				return unsafeError("open sealed artifact subdirectory", err)
			}
			childErr := verifyExistingNode(opened, child, syncFiles, snapshotPaths, snapshots)
			if childErr == nil && syncFiles {
				childErr = opened.Sync()
			}
			closeErr := opened.Close()
			if childErr != nil || closeErr != nil {
				return errors.Join(childErr, unsafeError("close verified sealed artifact subdirectory", closeErr))
			}
			continue
		}
		expected := node.files[name]
		opened, err := directory.OpenFile(name, true, syncFiles)
		if err != nil {
			return unsafeError("open sealed artifact file", err)
		}
		_, snapshotRequested := snapshotPaths[expected.RelativePath]
		snapshot, readErr := verifyExistingFile(opened, expected, snapshotRequested)
		if readErr == nil && syncFiles {
			readErr = opened.Sync()
		}
		closeErr := opened.Close()
		if readErr != nil || closeErr != nil {
			return errors.Join(readErr, unsafeError("close verified sealed artifact file", closeErr))
		}
		if snapshotRequested {
			snapshots[expected.RelativePath] = snapshot
		}
	}
	return nil
}

func verifyExistingFile(
	file outputcap.File,
	expected ExistingDirectoryFile,
	snapshotRequested bool,
) (ExistingDirectorySnapshot, error) {
	before, err := file.Size()
	if err != nil || before != expected.ByteLength {
		return ExistingDirectorySnapshot{}, unsafeError("verify sealed artifact file size", err)
	}
	digest := sha256.New()
	var snapshot []byte
	if snapshotRequested {
		snapshot = make([]byte, int(expected.ByteLength))
	}
	buffer := make([]byte, verificationBufferBytes)
	var offset uint64
	for offset < expected.ByteLength {
		remaining := expected.ByteLength - offset
		chunk := buffer
		if remaining < uint64(len(chunk)) {
			chunk = chunk[:int(remaining)]
		}
		read, readErr := file.ReadAt(chunk, int64(offset))
		if read > 0 {
			_, _ = digest.Write(chunk[:read])
			if snapshotRequested {
				copy(snapshot[int(offset):], chunk[:read])
			}
			offset += uint64(read)
		}
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return ExistingDirectorySnapshot{}, unsafeError("stream sealed artifact file", readErr)
		}
		if read == 0 {
			return ExistingDirectorySnapshot{}, unsafeError("stream exact sealed artifact bytes", io.ErrUnexpectedEOF)
		}
	}
	after, err := file.Size()
	if err != nil || after != before {
		return ExistingDirectorySnapshot{}, unsafeError("revalidate sealed artifact file size", err)
	}
	encodedDigest := hex.EncodeToString(digest.Sum(nil))
	if encodedDigest != expected.SHA256 {
		return ExistingDirectorySnapshot{}, unsafeError("verify sealed artifact file digest", nil)
	}
	return ExistingDirectorySnapshot{
		RelativePath: expected.RelativePath,
		Bytes:        snapshot,
		SHA256:       encodedDigest,
	}, nil
}
