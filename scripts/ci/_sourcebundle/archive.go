package main

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/mod/module"
)

func setDifference(left, right map[string]struct{}) []string {
	difference := make([]string, 0)
	for item := range left {
		if _, found := right[item]; !found {
			difference = append(difference, item)
		}
	}
	sort.Strings(difference)
	return difference
}

func sourcePrefix(version module.Version) string { return "windshare-" + version.Version + "/" }

// No module-aware archive filter may discard a local replacement or its license.
// Every committed regular file streams directly from Git into the archive so
// Windows does not lose POSIX execute bits or pay for a redundant staging tree.
func buildSourceBundle(repositoryRoot, zipPath string, version module.Version, committed []committedFile) (builtBundle, error) {
	if err := os.MkdirAll(filepath.Dir(zipPath), 0o755); err != nil {
		return builtBundle{}, fmt.Errorf("create source bundle parent: %w", err)
	}
	output, err := os.OpenFile(zipPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return builtBundle{}, fmt.Errorf("create source bundle: %w", err)
	}

	requiredSet := make(map[string]struct{}, len(requiredFiles))
	for _, filePath := range requiredFiles {
		requiredSet[filePath] = struct{}{}
	}
	metadataContents := make(map[string][]byte, len(metadataPaths))
	fileDigests := make(map[string][sha256.Size]byte, len(committed))
	zipHash := sha256.New()
	writer := zip.NewWriter(io.MultiWriter(output, zipHash))
	prefix := sourcePrefix(version)

	createErr := streamCommittedBlobs(repositoryRoot, committed, func(file committedFile, size int64, body io.Reader) error {
		if _, required := requiredSet[file.relativePath]; required && size == 0 {
			return fmt.Errorf("required release file is empty: %s", file.relativePath)
		}
		header := &zip.FileHeader{Name: prefix + file.relativePath, Method: zip.Deflate}
		header.SetMode(0o644)
		if file.executable {
			header.SetMode(0o755)
		}
		entry, err := writer.CreateHeader(header)
		if err != nil {
			return err
		}

		fileHash := sha256.New()
		var metadataBuffer bytes.Buffer
		destination := io.MultiWriter(entry, fileHash)
		captureMetadata := isMetadataPath(file.relativePath)
		if captureMetadata {
			if size > metadataFileMaxBytes {
				return fmt.Errorf("metadata file exceeds limit (%d bytes): %s", metadataFileMaxBytes, file.relativePath)
			}
			destination = io.MultiWriter(entry, fileHash, &metadataBuffer)
		}

		written, err := io.Copy(destination, body)
		if err != nil {
			return err
		}
		if written != size {
			return fmt.Errorf("short blob write for %s: wrote %d of %d bytes", file.relativePath, written, size)
		}
		var digest [sha256.Size]byte
		copy(digest[:], fileHash.Sum(nil))
		fileDigests[file.relativePath] = digest
		if captureMetadata {
			metadataContents[file.relativePath] = metadataBuffer.Bytes()
		}
		return nil
	})
	createErr = errors.Join(createErr, writer.Close())
	closeErr := output.Close()
	if createErr != nil {
		return builtBundle{}, removeFailedArchive(zipPath, fmt.Errorf("construct source bundle: %w", createErr))
	}
	if closeErr != nil {
		return builtBundle{}, removeFailedArchive(zipPath, fmt.Errorf("close source bundle: %w", closeErr))
	}
	if err := validateReleaseMetadata(metadataContents, committed); err != nil {
		return builtBundle{}, removeFailedArchive(zipPath, err)
	}

	var result builtBundle
	copy(result.zipDigest[:], zipHash.Sum(nil))
	result.fileDigests = fileDigests
	return result, nil
}

func extractSourceBundle(
	zipPath, extractPath string,
	version module.Version,
	expected []string,
	expectedDigests map[string][sha256.Size]byte,
) error {
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer reader.Close()
	prefix := sourcePrefix(version)
	actual := make([]string, 0, len(reader.File))
	for _, file := range reader.File {
		if !strings.HasPrefix(file.Name, prefix) || !file.Mode().IsRegular() {
			return fmt.Errorf("invalid source entry: %s", file.Name)
		}
		actual = append(actual, strings.TrimPrefix(file.Name, prefix))
	}
	if err := validateExactProjection(actual, expected, "source bundle"); err != nil {
		return err
	}
	if err := os.MkdirAll(extractPath, 0o755); err != nil {
		return err
	}
	if err := requireEmptyDirectory(extractPath); err != nil {
		return err
	}
	createdDirs := make(map[string]struct{})
	for index, file := range reader.File {
		relativePath := actual[index]
		destination := filepath.Join(extractPath, filepath.FromSlash(relativePath))
		parent := filepath.Dir(destination)
		if _, exists := createdDirs[parent]; !exists {
			if err := os.MkdirAll(parent, 0o755); err != nil {
				return err
			}
			createdDirs[parent] = struct{}{}
		}
		input, err := file.Open()
		if err != nil {
			return err
		}
		output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, file.Mode().Perm())
		if err != nil {
			_ = input.Close()
			return err
		}
		extractedHash := sha256.New()
		_, copyErr := io.Copy(io.MultiWriter(output, extractedHash), input)
		if err := errors.Join(copyErr, input.Close(), output.Close()); err != nil {
			return err
		}
		if expectedDigests != nil {
			expectedDigest, found := expectedDigests[relativePath]
			if !found {
				return fmt.Errorf("missing expected digest for extracted source: %s", relativePath)
			}
			var actualDigest [sha256.Size]byte
			copy(actualDigest[:], extractedHash.Sum(nil))
			if actualDigest != expectedDigest {
				return fmt.Errorf("extracted source differs: %s", relativePath)
			}
		}
	}
	return nil
}

func validateExactProjection(actualFiles, committedFiles []string, actualName string) error {
	actual, err := modulePathSet(actualFiles, actualName)
	if err != nil {
		return err
	}
	committed, err := modulePathSet(committedFiles, "committed projection")
	if err != nil {
		return err
	}
	if difference := setDifference(actual, committed); len(difference) != 0 {
		return fmt.Errorf("%s has files outside the committed projection: %s", actualName, strings.Join(difference, ", "))
	}
	if difference := setDifference(committed, actual); len(difference) != 0 {
		return fmt.Errorf("committed projection has files absent from the %s: %s", actualName, strings.Join(difference, ", "))
	}
	return nil
}

func modulePathSet(files []string, owner string) (map[string]struct{}, error) {
	result := make(map[string]struct{}, len(files))
	for _, filePath := range files {
		if !isSafeModulePath(filePath) {
			return nil, fmt.Errorf("%s contains invalid module path: %q", owner, filePath)
		}
		if _, duplicate := result[filePath]; duplicate {
			return nil, fmt.Errorf("%s contains duplicate module path: %s", owner, filePath)
		}
		result[filePath] = struct{}{}
	}
	return result, nil
}

func removeFailedArchive(zipPath string, failure error) error {
	if err := os.Remove(zipPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Join(failure, fmt.Errorf("remove failed source bundle: %w", err))
	}
	return failure
}
