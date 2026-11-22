package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"strings"

	"golang.org/x/mod/modfile"
)

const (
	metadataFileMaxBytes = 4 << 20
	vectorDirectorySlash = "core/testvectors/"
)

var metadataPaths = map[string]struct{}{
	"go.mod":                         {},
	"LICENSE":                        {},
	"NOTICE":                         {},
	"core/testvectors/inventory.txt": {},
}

func isMetadataPath(relativePath string) bool {
	_, ok := metadataPaths[relativePath]
	return ok
}

func auditCommittedProjection(files []committedFile) error {
	seenFiles := make(map[string]struct{}, len(files))
	for _, file := range files {
		seenFiles[file.relativePath] = struct{}{}
	}
	required := make(map[string]struct{}, len(requiredFiles))
	for _, filePath := range requiredFiles {
		required[filePath] = struct{}{}
	}
	if missing := setDifference(required, seenFiles); len(missing) != 0 {
		return fmt.Errorf("required release files are missing: %s", strings.Join(missing, ", "))
	}
	return nil
}

func requireEmptyDirectory(directory string) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("read target directory: %w", err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("target directory is not empty: %s", directory)
	}
	return nil
}

func validateReleaseMetadata(metadata map[string][]byte, files []committedFile) error {
	goMod, ok := metadata["go.mod"]
	if !ok || len(goMod) == 0 {
		return errors.New("required release file is missing or empty: go.mod")
	}
	if actual := modfile.ModulePath(goMod); actual != modulePath {
		return fmt.Errorf("go.mod module path is %q, want %q", actual, modulePath)
	}
	if err := requireText("LICENSE", metadata["LICENSE"], "GNU AFFERO GENERAL PUBLIC LICENSE", "Version 3, 19 November 2007"); err != nil {
		return err
	}
	if err := requireText("NOTICE", metadata["NOTICE"], "WindShare", "SPDX-License-Identifier: AGPL-3.0-or-later"); err != nil {
		return err
	}
	inventoryBytes, ok := metadata["core/testvectors/inventory.txt"]
	if !ok || len(inventoryBytes) == 0 {
		return errors.New("required release file is missing or empty: core/testvectors/inventory.txt")
	}
	return validateVectorInventory(inventoryBytes, files)
}

func requireText(filePath string, content []byte, required ...string) error {
	if len(content) == 0 {
		return fmt.Errorf("required release file is missing or empty: %s", filePath)
	}
	for _, text := range required {
		if !bytes.Contains(content, []byte(text)) {
			return fmt.Errorf("%s does not contain required text %q", filePath, text)
		}
	}
	return nil
}

func validateVectorInventory(inventoryBytes []byte, files []committedFile) error {
	expected := make(map[string]struct{})
	scanner := bufio.NewScanner(bytes.NewReader(inventoryBytes))
	for scanner.Scan() {
		entry := strings.TrimSpace(scanner.Text())
		if entry == "" || strings.HasPrefix(entry, "#") {
			continue
		}
		if path.Base(entry) != entry || path.Ext(entry) != ".json" || !isSafeModulePath(entry) {
			return fmt.Errorf("invalid testvector inventory entry: %q", entry)
		}
		if _, duplicate := expected[entry]; duplicate {
			return fmt.Errorf("duplicate testvector inventory entry: %s", entry)
		}
		expected[entry] = struct{}{}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read testvector inventory: %w", err)
	}
	if len(expected) == 0 {
		return errors.New("testvector inventory is empty")
	}

	actual := make(map[string]struct{})
	for _, file := range files {
		if !strings.HasPrefix(file.relativePath, vectorDirectorySlash) {
			continue
		}
		relativeVector := strings.TrimPrefix(file.relativePath, vectorDirectorySlash)
		if path.Ext(relativeVector) != ".json" {
			continue
		}
		actual[relativeVector] = struct{}{}
	}
	if difference := setDifference(expected, actual); len(difference) != 0 {
		return fmt.Errorf("testvector inventory names missing files: %s", strings.Join(difference, ", "))
	}
	if difference := setDifference(actual, expected); len(difference) != 0 {
		return fmt.Errorf("testvector JSON files missing inventory entries: %s", strings.Join(difference, ", "))
	}
	return nil
}
