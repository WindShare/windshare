package main

import (
	"archive/zip"
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

// No module-aware archive filter may discard a local replacement or its license.
func validateSourceInput(stageDirectory string) ([]string, error) {
	var accepted []string
	err := filepath.WalkDir(stageDirectory, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		relativePath, err := filepath.Rel(stageDirectory, filePath)
		if err != nil {
			return err
		}
		normalized := filepath.ToSlash(relativePath)
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || !isSafeModulePath(normalized) {
			return fmt.Errorf("irregular source path: %s", normalized)
		}
		accepted = append(accepted, normalized)
		return nil
	})
	sort.Strings(accepted)
	return accepted, err
}

func sourcePrefix(version module.Version) string { return "windshare-" + version.Version + "/" }

func extractSourceBundle(zipPath, extractPath string, version module.Version, expected []string) error {
	reader, err := zip.OpenReader(zipPath)
	if err != nil {
		return err
	}
	defer reader.Close()
	prefix := sourcePrefix(version)
	var actual []string
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
	for index, file := range reader.File {
		destination := filepath.Join(extractPath, filepath.FromSlash(actual[index]))
		if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
			return err
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
		_, copyErr := io.Copy(output, input)
		if err := errors.Join(copyErr, input.Close(), output.Close()); err != nil {
			return err
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

func createSourceBundle(zipPath string, version module.Version, stageDirectory string, committed []committedFile) error {
	executable := make(map[string]bool, len(committed))
	for _, file := range committed {
		executable[file.relativePath] = file.executable
	}
	if err := os.MkdirAll(filepath.Dir(zipPath), 0o755); err != nil {
		return fmt.Errorf("create source bundle parent: %w", err)
	}
	output, err := os.OpenFile(zipPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create source bundle: %w", err)
	}
	writer := zip.NewWriter(output)
	paths, createErr := validateSourceInput(stageDirectory)
	if createErr == nil {
		for _, relativePath := range paths {
			filePath := filepath.Join(stageDirectory, filepath.FromSlash(relativePath))
			content, err := os.ReadFile(filePath)
			if err != nil {
				createErr = err
				break
			}
			header := &zip.FileHeader{Name: sourcePrefix(version) + relativePath, Method: zip.Deflate}
			header.SetMode(0o644)
			// Windows does not preserve POSIX execute bits on staged files;
			// the commit's mode is the portable source of archive permissions.
			if executable[relativePath] {
				header.SetMode(0o755)
			}
			entry, err := writer.CreateHeader(header)
			if err != nil {
				createErr = err
				break
			}
			if _, err := entry.Write(content); err != nil {
				createErr = err
				break
			}
		}
	}
	createErr = errors.Join(createErr, writer.Close())
	closeErr := output.Close()
	if createErr != nil {
		return removeFailedArchive(zipPath, fmt.Errorf("construct source bundle: %w", createErr))
	}
	if closeErr != nil {
		return removeFailedArchive(zipPath, fmt.Errorf("close source bundle: %w", closeErr))
	}
	return nil
}

func removeFailedArchive(zipPath string, failure error) error {
	if err := os.Remove(zipPath); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Join(failure, fmt.Errorf("remove failed source bundle: %w", err))
	}
	return failure
}

func fileDigest(filePath string) ([sha256.Size]byte, error) {
	file, err := os.Open(filePath)
	if err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("open archive for hashing: %w", err)
	}
	defer file.Close()

	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return [sha256.Size]byte{}, fmt.Errorf("hash archive: %w", err)
	}
	var result [sha256.Size]byte
	copy(result[:], digest.Sum(nil))
	return result, nil
}
