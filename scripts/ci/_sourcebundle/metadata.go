package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"

	"golang.org/x/mod/modfile"
)

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
		return fmt.Errorf("read staging directory: %w", err)
	}
	if len(entries) != 0 {
		return fmt.Errorf("staging directory is not empty: %s", directory)
	}
	return nil
}

func validateReleaseMetadata(stageDirectory string) error {
	for _, relativePath := range requiredFiles {
		info, err := os.Stat(filepath.Join(stageDirectory, filepath.FromSlash(relativePath)))
		if err != nil {
			return fmt.Errorf("required release file %s: %w", relativePath, err)
		}
		if !info.Mode().IsRegular() || info.Size() == 0 {
			return fmt.Errorf("required release file is empty or irregular: %s", relativePath)
		}
	}

	goMod, err := os.ReadFile(filepath.Join(stageDirectory, "go.mod"))
	if err != nil {
		return fmt.Errorf("read go.mod: %w", err)
	}
	if actual := modfile.ModulePath(goMod); actual != modulePath {
		return fmt.Errorf("go.mod module path is %q, want %q", actual, modulePath)
	}
	if err := requireText(filepath.Join(stageDirectory, "LICENSE"), "Apache License", "Version 2.0"); err != nil {
		return err
	}
	if err := requireText(filepath.Join(stageDirectory, "NOTICE"), "WindShare"); err != nil {
		return err
	}
	if err := validateVectorInventory(stageDirectory); err != nil {
		return err
	}
	return nil
}

func requireText(filePath string, required ...string) error {
	content, err := os.ReadFile(filePath)
	if err != nil {
		return fmt.Errorf("read %s: %w", filePath, err)
	}
	for _, text := range required {
		if !bytes.Contains(content, []byte(text)) {
			return fmt.Errorf("%s does not contain required text %q", filePath, text)
		}
	}
	return nil
}

func validateVectorInventory(stageDirectory string) error {
	vectorDirectory := filepath.Join(stageDirectory, "core", "testvectors")
	inventoryPath := filepath.Join(vectorDirectory, "inventory.txt")
	inventory, err := os.Open(inventoryPath)
	if err != nil {
		return fmt.Errorf("open testvector inventory: %w", err)
	}
	defer inventory.Close()

	expected := make(map[string]struct{})
	scanner := bufio.NewScanner(inventory)
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
	err = filepath.WalkDir(vectorDirectory, func(filePath string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return fmt.Errorf("inspect testvector path %s: %w", filePath, walkErr)
		}
		if filePath == vectorDirectory {
			return nil
		}
		relativePath, err := filepath.Rel(vectorDirectory, filePath)
		if err != nil {
			return fmt.Errorf("relativize testvector path %s: %w", filePath, err)
		}
		normalized := filepath.ToSlash(relativePath)
		if path.Ext(normalized) != ".json" {
			return nil
		}
		if entry.IsDir() {
			return fmt.Errorf("testvector JSON path is irregular: %s", normalized)
		}
		info, err := entry.Info()
		if err != nil {
			return fmt.Errorf("inspect testvector %s: %w", normalized, err)
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("testvector JSON path is irregular: %s", normalized)
		}
		actual[normalized] = struct{}{}
		return nil
	})
	if err != nil {
		return fmt.Errorf("read testvector directory: %w", err)
	}
	if difference := setDifference(expected, actual); len(difference) != 0 {
		return fmt.Errorf("testvector inventory names missing files: %s", strings.Join(difference, ", "))
	}
	if difference := setDifference(actual, expected); len(difference) != 0 {
		return fmt.Errorf("testvector JSON files missing inventory entries: %s", strings.Join(difference, ", "))
	}
	return nil
}
