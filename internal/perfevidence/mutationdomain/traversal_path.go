package mutationdomain

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func treeSHA256(root string) (string, error) {
	budget := productionMutationTraversalBudget()
	if err := budget.admitCandidate("source"); err != nil {
		return "", err
	}
	return treeSHA256WithBudget(root, budget)
}

func treeSHA256WithBudget(root string, budget *mutationTraversalBudget) (string, error) {
	return walkPathTree(root, "", budget)
}

func copyTreeWithBudget(source, destination string, budget *mutationTraversalBudget) (string, error) {
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return "", err
	}
	return walkPathTree(source, destination, budget)
}

func walkPathTree(source, destination string, budget *mutationTraversalBudget) (string, error) {
	root, err := os.Open(source)
	if err != nil {
		return "", err
	}
	rootInfo, statErr := root.Stat()
	if statErr != nil || rootInfo == nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return "", errors.Join(errors.New("private mutation input root is not a no-follow directory"), statErr, root.Close())
	}
	records, walkErr := walkPathDirectory(root, source, destination, "", 0, budget)
	if err := errors.Join(walkErr, root.Close()); err != nil {
		return "", err
	}
	sort.Strings(records)
	return hashBytes([]byte(strings.Join(records, "\n"))), nil
}

func walkPathDirectory(
	directory *os.File,
	sourcePath string,
	destinationPath string,
	prefix string,
	depth int,
	budget *mutationTraversalBudget,
) ([]string, error) {
	seen := make(map[string]struct{})
	var records []string
	for {
		entries, readErr := directory.ReadDir(mutationTreeBatchSize)
		sort.Slice(entries, func(left, right int) bool { return entries[left].Name() < entries[right].Name() })
		for _, entry := range entries {
			name := entry.Name()
			if name == "" || name == "." || name == ".." || filepath.Base(name) != name {
				return nil, fmt.Errorf("private mutation input contains invalid name %q", name)
			}
			if _, duplicate := seen[name]; duplicate {
				return nil, fmt.Errorf("private mutation input enumeration repeated name %q", name)
			}
			seen[name] = struct{}{}
			relative := name
			if prefix != "" {
				relative = filepath.Join(prefix, name)
			}
			path := filepath.Join(sourcePath, name)
			entryInfo, err := entry.Info()
			if err != nil {
				return nil, err
			}
			if entryInfo.Mode()&os.ModeSymlink != 0 {
				return nil, fmt.Errorf("private mutation input contains symlink %s", path)
			}
			if entryInfo.IsDir() {
				if err := budget.admitObject(relative, depth+1, 0); err != nil {
					return nil, err
				}
				child, err := os.Open(path)
				if err != nil {
					return nil, err
				}
				stable, statErr := child.Stat()
				if statErr != nil || stable == nil || !stable.IsDir() || !os.SameFile(entryInfo, stable) {
					return nil, errors.Join(errors.New("private mutation input directory changed during retained open"), statErr, child.Close())
				}
				childDestination := ""
				if destinationPath != "" {
					childDestination = filepath.Join(destinationPath, name)
					if err := os.Mkdir(childDestination, 0o700); err != nil {
						return nil, errors.Join(err, child.Close())
					}
					if err := os.Chmod(childDestination, stable.Mode().Perm()); err != nil {
						return nil, errors.Join(err, child.Close())
					}
				}
				childRecords, walkErr := walkPathDirectory(
					child, path, childDestination, relative, depth+1, budget,
				)
				if err := errors.Join(walkErr, child.Close()); err != nil {
					return nil, err
				}
				records = append(records, fmt.Sprintf(
					"D\x00%s\x00%o", filepath.ToSlash(relative), stable.Mode().Perm(),
				))
				records = append(records, childRecords...)
				continue
			}
			if !entryInfo.Mode().IsRegular() {
				return nil, fmt.Errorf("private mutation input contains unsupported object %s", path)
			}
			if err := budget.admitObject(relative, depth+1, entryInfo.Size()); err != nil {
				return nil, err
			}
			record, err := copyPathTreeFile(path, destinationPath, name, relative, entryInfo)
			if err != nil {
				return nil, err
			}
			records = append(records, record)
		}
		if errors.Is(readErr, io.EOF) {
			return records, nil
		}
		if readErr != nil {
			return nil, readErr
		}
	}
}

func copyPathTreeFile(path, destinationPath, name, relative string, admitted os.FileInfo) (string, error) {
	input, err := os.Open(path)
	if err != nil {
		return "", err
	}
	stable, statErr := input.Stat()
	if statErr != nil || stable == nil || !stable.Mode().IsRegular() || !os.SameFile(admitted, stable) ||
		stable.Size() != admitted.Size() {
		return "", errors.Join(errors.New("private mutation input file changed during retained open"), statErr, input.Close())
	}
	hasher := sha256.New()
	writer := io.Writer(hasher)
	var output *os.File
	if destinationPath != "" {
		output, err = os.OpenFile(
			filepath.Join(destinationPath, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600,
		)
		if err != nil {
			return "", errors.Join(err, input.Close())
		}
		writer = io.MultiWriter(hasher, output)
	}
	written, copyErr := io.CopyN(writer, input, stable.Size())
	var extra [1]byte
	extraBytes, extraErr := input.Read(extra[:])
	if errors.Is(extraErr, io.EOF) {
		extraErr = nil
	}
	var outputErr error
	if output != nil {
		modeErr := output.Chmod(stable.Mode().Perm())
		copied, copiedStatErr := output.Stat()
		if copiedStatErr == nil && (copied.Size() != stable.Size() || copied.Mode().Perm() != stable.Mode().Perm()) {
			copiedStatErr = fmt.Errorf("copied file %s did not retain its admitted identity", relative)
		}
		outputErr = errors.Join(modeErr, copiedStatErr, output.Sync(), output.Close())
	}
	if err := errors.Join(copyErr, extraErr, outputErr, input.Close()); err != nil || written != stable.Size() || extraBytes != 0 {
		return "", errors.Join(fmt.Errorf("private mutation input %s changed while copying", relative), err)
	}
	return fmt.Sprintf(
		"F\x00%s\x00%d\x00%o\x00%s", filepath.ToSlash(relative), stable.Size(), stable.Mode().Perm(),
		hex.EncodeToString(hasher.Sum(nil)),
	), nil
}
