package perfevidence

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

func CaptureSource(ctx context.Context, runner CommandRunner, repositoryRoot string) (SourceIdentity, error) {
	root, err := filepath.Abs(repositoryRoot)
	if err != nil {
		return SourceIdentity{}, fmt.Errorf("resolve repository root: %w", err)
	}
	commit, err := runGit(ctx, runner, root, "rev-parse", "HEAD")
	if err != nil {
		return SourceIdentity{}, err
	}
	status, err := runGit(ctx, runner, root, "status", "--porcelain=v1", "-z", "--untracked-files=all")
	if err != nil {
		return SourceIdentity{}, err
	}
	listed, err := runGit(ctx, runner, root, "ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return SourceIdentity{}, err
	}
	paths, err := nulPaths(listed)
	if err != nil {
		return SourceIdentity{}, err
	}
	files := make([]SourceFile, 0, len(paths))
	for _, relative := range paths {
		record, recordErr := snapshotSourceFile(root, relative)
		if recordErr != nil {
			return SourceIdentity{}, recordErr
		}
		files = append(files, record)
	}
	sort.Slice(files, func(left, right int) bool { return files[left].Path < files[right].Path })
	identity := SourceIdentity{
		Commit:        strings.TrimSpace(string(commit)),
		WorktreeDirty: len(status) != 0,
		StatusSHA256:  hashBytes(status),
		Files:         files,
	}
	digestInput, err := json.Marshal(struct {
		Commit       string       `json:"commit"`
		StatusSHA256 string       `json:"statusSha256"`
		Files        []SourceFile `json:"files"`
	}{identity.Commit, identity.StatusSHA256, identity.Files})
	if err != nil {
		return SourceIdentity{}, fmt.Errorf("encode source identity: %w", err)
	}
	identity.SourceSHA256 = hashBytes(digestInput)
	return identity, nil
}

func SameSource(left, right SourceIdentity) bool {
	return left.Commit == right.Commit &&
		left.StatusSHA256 == right.StatusSHA256 &&
		left.SourceSHA256 == right.SourceSHA256
}

func requireStableSourceObservation(
	ctx context.Context,
	runner CommandRunner,
	repositoryRoot string,
	expected SourceIdentity,
	boundary string,
) error {
	observed, err := CaptureSource(ctx, runner, repositoryRoot)
	if err != nil {
		return fmt.Errorf("repeat source observation after %s: %w", boundary, err)
	}
	if !SameSource(expected, observed) {
		return fmt.Errorf(
			"source identity changed during %s (commit %s -> %s, status %s -> %s)",
			boundary, expected.Commit, observed.Commit, expected.StatusSHA256, observed.StatusSHA256,
		)
	}
	return nil
}

func runGit(ctx context.Context, runner CommandRunner, root string, arguments ...string) ([]byte, error) {
	result, err := runner.Run(ctx, Command{Executable: "git", Arguments: append([]string{"-C", root}, arguments...)})
	if err != nil {
		return nil, fmt.Errorf("git %s: %w: %s", strings.Join(arguments, " "), err, strings.TrimSpace(string(result.Output)))
	}
	if result.ExitCode != 0 {
		return nil, fmt.Errorf("git %s exited with %d: %s", strings.Join(arguments, " "), result.ExitCode, strings.TrimSpace(string(result.Output)))
	}
	return result.Output, nil
}

func nulPaths(encoded []byte) ([]string, error) {
	if len(encoded) == 0 {
		return nil, nil
	}
	if encoded[len(encoded)-1] != 0 {
		return nil, errors.New("git path list was not NUL terminated")
	}
	parts := strings.Split(string(encoded[:len(encoded)-1]), "\x00")
	seen := make(map[string]struct{}, len(parts))
	for _, path := range parts {
		if path == "" {
			return nil, errors.New("git path list contained an empty path")
		}
		if _, duplicate := seen[path]; duplicate {
			return nil, fmt.Errorf("git path list repeated %q", path)
		}
		seen[path] = struct{}{}
	}
	return parts, nil
}

func snapshotSourceFile(root, relative string) (SourceFile, error) {
	canonical := filepath.ToSlash(relative)
	clean := filepath.Clean(filepath.FromSlash(canonical))
	if clean == "." || filepath.IsAbs(clean) || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return SourceFile{}, fmt.Errorf("git returned unsafe source path %q", relative)
	}
	path := filepath.Join(root, clean)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return SourceFile{Path: canonical, Kind: "missing", Missing: true}, nil
	}
	if err != nil {
		return SourceFile{}, fmt.Errorf("inspect source file %s: %w", canonical, err)
	}
	record := SourceFile{Path: canonical, Mode: uint32(info.Mode()), Bytes: info.Size()}
	switch {
	case isReparsePointInfo(info):
		if info.Mode()&os.ModeSymlink == 0 {
			return SourceFile{}, fmt.Errorf("source path %s is an unsupported reparse point", canonical)
		}
		record.Kind = "symlink"
		var target string
		target, err = os.Readlink(path)
		record.Bytes = int64(len([]byte(target)))
		record.SHA256 = hashBytes([]byte(target))
	case info.Mode().IsRegular():
		record.Kind = "file"
		record.SHA256, err = hashFileExact(path, info.Size())
	case info.IsDir():
		record.Kind = "directory"
		record.Bytes = 0
		record.SHA256 = hashBytes(nil)
	default:
		return SourceFile{}, fmt.Errorf("source path %s has unsupported mode %s", canonical, info.Mode())
	}
	if err != nil {
		return SourceFile{}, fmt.Errorf("hash source file %s: %w", canonical, err)
	}
	return record, nil
}

func hashFile(path string) (digestValue string, resultErr error) {
	digestValue, _, resultErr = hashFileAtMost(path, maximumSnapshotSingleFileBytes)
	return digestValue, resultErr
}

func hashFileExact(path string, expectedBytes int64) (digestValue string, resultErr error) {
	if expectedBytes < 0 || expectedBytes > maximumSnapshotSingleFileBytes {
		return "", fmt.Errorf(
			"file %s byte count %d exceeds maximum %d",
			path, expectedBytes, maximumSnapshotSingleFileBytes,
		)
	}
	digestValue, observedBytes, err := hashFileAtMost(path, expectedBytes)
	if err != nil {
		return "", err
	}
	if observedBytes != expectedBytes {
		return "", fmt.Errorf("file %s has %d bytes, expected %d", path, observedBytes, expectedBytes)
	}
	return digestValue, nil
}

func hashFileAtMost(path string, maximumBytes int64) (digestValue string, observedBytes int64, resultErr error) {
	if maximumBytes < 0 || maximumBytes >= int64(^uint64(0)>>1) {
		return "", 0, fmt.Errorf("file %s has an invalid hash byte limit %d", path, maximumBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer func() { resultErr = errors.Join(resultErr, file.Close()) }()
	info, err := file.Stat()
	if err != nil {
		return "", 0, err
	}
	if !info.Mode().IsRegular() || isReparsePointInfo(info) {
		return "", 0, fmt.Errorf("file %s is not a real regular file", path)
	}
	if info.Size() > maximumBytes {
		return "", 0, fmt.Errorf("file %s exceeds maximum byte count %d", path, maximumBytes)
	}
	digest := sha256.New()
	// FileInfo is only a preflight observation. Limiting the reader makes the
	// declared byte budget remain authoritative if a concurrent writer grows
	// the file before or during hashing.
	observedBytes, err = io.Copy(digest, io.LimitReader(file, maximumBytes+1))
	if err != nil {
		return "", observedBytes, err
	}
	if observedBytes > maximumBytes {
		return "", observedBytes, fmt.Errorf("file %s exceeded maximum byte count %d while hashing", path, maximumBytes)
	}
	return hex.EncodeToString(digest.Sum(nil)), observedBytes, nil
}

func hashBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}
