package artifactpublish

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"path"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"unicode/utf8"

	"golang.org/x/text/cases"
	"golang.org/x/text/language"
	"golang.org/x/text/unicode/norm"
)

const (
	existingDirectoryStagingPrefix                    = ".browser-evidence-upload-"
	maximumExistingDirectoryFiles                     = 10_000
	maximumExistingDirectoryDirectories               = 20_000
	maximumExistingDirectoryFileBytes          uint64 = 512 << 20
	maximumExistingDirectoryTotalBytes         uint64 = 2 << 30
	maximumExistingDirectorySnapshotBytes      uint64 = 16 << 20
	maximumExistingDirectorySnapshotTotalBytes uint64 = 32 << 20
	maximumExistingDirectorySnapshots                 = 64
	maximumExistingDirectoryPathBytes                 = 4_096
	maximumExistingDirectoryDepth                     = 64
	maximumExistingDirectoryManifestBytes      uint64 = 8 << 20
	existingDirectoryManifestPath                     = "manifest.json"
)

type normalizedExistingDirectory struct {
	parentPath             string
	outputName             string
	stagingName            string
	inventory              ExistingDirectoryInventory
	manifestPath           string
	expectedManifestSHA256 string
	snapshotPaths          []string
	tree                   *existingDirectoryNode
}

type existingDirectoryNode struct {
	relativePath string
	directories  map[string]*existingDirectoryNode
	files        map[string]ExistingDirectoryFile
}

type existingDirectoryInventoryIndex struct {
	directories  []string
	files        []ExistingDirectoryFile
	directorySet map[string]struct{}
	fileSet      map[string]ExistingDirectoryFile
}

func normalizeExistingDirectoryRequest(request ExistingDirectoryRequest) (normalizedExistingDirectory, error) {
	if !validExistingStagingName(request.StagingName) {
		return normalizedExistingDirectory{}, fmt.Errorf("%w: existing staging name is not invocation-owned", ErrUnsafe)
	}
	if request.Receipt.IsZero() {
		return normalizedExistingDirectory{}, fmt.Errorf("%w: existing staging receipt is required", ErrUnsafe)
	}
	return normalizeExistingDirectory(
		request.ParentPath,
		request.OutputName,
		request.StagingName,
		request.Inventory,
		request.ManifestPath,
		request.ExpectedManifestSHA256,
		request.SnapshotPaths,
	)
}

func normalizeExistingDirectoryVerificationRequest(
	request ExistingDirectoryVerificationRequest,
) (normalizedExistingDirectory, error) {
	return normalizeExistingDirectory(
		request.ParentPath,
		request.OutputName,
		"",
		request.Inventory,
		request.ManifestPath,
		request.ExpectedManifestSHA256,
		request.SnapshotPaths,
	)
}

func normalizeExistingDirectory(
	parentPath string,
	outputName string,
	stagingName string,
	inventory ExistingDirectoryInventory,
	manifestPath string,
	expectedManifestSHA256 string,
	snapshotPaths []string,
) (normalizedExistingDirectory, error) {
	if err := validateExistingDirectoryAuthority(
		parentPath,
		outputName,
		inventory,
		manifestPath,
		expectedManifestSHA256,
	); err != nil {
		return normalizedExistingDirectory{}, err
	}
	indexed, err := indexExistingDirectoryInventory(inventory)
	if err != nil {
		return normalizedExistingDirectory{}, err
	}
	if err := requireExistingManifestAuthority(
		indexed.fileSet,
		manifestPath,
		expectedManifestSHA256,
	); err != nil {
		return normalizedExistingDirectory{}, err
	}
	if err := requireExistingParentClosure(indexed); err != nil {
		return normalizedExistingDirectory{}, err
	}
	normalizedSnapshots, err := normalizeExistingSnapshots(snapshotPaths, indexed.fileSet)
	if err != nil {
		return normalizedExistingDirectory{}, err
	}
	tree := buildExistingDirectoryTree(indexed.directories, indexed.files)
	return normalizedExistingDirectory{
		parentPath:  parentPath,
		outputName:  outputName,
		stagingName: stagingName,
		inventory: ExistingDirectoryInventory{
			Directories: indexed.directories,
			Files:       indexed.files,
		},
		manifestPath:           manifestPath,
		expectedManifestSHA256: expectedManifestSHA256,
		snapshotPaths:          normalizedSnapshots,
		tree:                   tree,
	}, nil
}

func validateExistingDirectoryAuthority(
	parentPath string,
	outputName string,
	inventory ExistingDirectoryInventory,
	manifestPath string,
	expectedManifestSHA256 string,
) error {
	if !filepath.IsAbs(parentPath) || filepath.Clean(parentPath) != parentPath {
		return fmt.Errorf("%w: sealed artifact parent must be clean and absolute", ErrUnsafe)
	}
	if outputName != ExistingDirectoryOutputName {
		return fmt.Errorf("%w: sealed artifact output name is not deterministic", ErrUnsafe)
	}
	if manifestPath != existingDirectoryManifestPath {
		return fmt.Errorf("%w: sealed artifact manifest path is not canonical", ErrUnsafe)
	}
	if !isSHA256(expectedManifestSHA256) {
		return fmt.Errorf("%w: expected sealed artifact manifest digest is invalid", ErrUnsafe)
	}
	if len(inventory.Files) < 1 || len(inventory.Files) > maximumExistingDirectoryFiles ||
		len(inventory.Directories) > maximumExistingDirectoryDirectories {
		return fmt.Errorf("%w: sealed artifact inventory exceeds its entry authority", ErrUnsafe)
	}
	return nil
}

func indexExistingDirectoryInventory(
	inventory ExistingDirectoryInventory,
) (existingDirectoryInventoryIndex, error) {
	directories := slices.Clone(inventory.Directories)
	files := slices.Clone(inventory.Files)
	if !sort.StringsAreSorted(directories) || !slices.IsSortedFunc(files, compareExistingFiles) {
		return existingDirectoryInventoryIndex{}, fmt.Errorf("%w: sealed artifact inventory is not canonically ordered", ErrUnsafe)
	}
	portableSet := make(map[string]struct{}, len(directories)+len(files))
	directorySet, err := indexExistingDirectories(directories, portableSet)
	if err != nil {
		return existingDirectoryInventoryIndex{}, err
	}
	fileSet, err := indexExistingFiles(files, portableSet)
	if err != nil {
		return existingDirectoryInventoryIndex{}, err
	}
	return existingDirectoryInventoryIndex{
		directories:  directories,
		files:        files,
		directorySet: directorySet,
		fileSet:      fileSet,
	}, nil
}

func indexExistingDirectories(
	directories []string,
	portableSet map[string]struct{},
) (map[string]struct{}, error) {
	directorySet := make(map[string]struct{}, len(directories))
	for _, directory := range directories {
		if err := requirePortableExistingPath(directory); err != nil {
			return nil, err
		}
		if _, repeated := directorySet[directory]; repeated {
			return nil, fmt.Errorf("%w: sealed artifact directory repeats", ErrUnsafe)
		}
		key := portableExistingPathKey(directory)
		if _, collided := portableSet[key]; collided {
			return nil, fmt.Errorf("%w: sealed artifact paths collide portably", ErrUnsafe)
		}
		directorySet[directory] = struct{}{}
		portableSet[key] = struct{}{}
	}
	return directorySet, nil
}

func indexExistingFiles(
	files []ExistingDirectoryFile,
	portableSet map[string]struct{},
) (map[string]ExistingDirectoryFile, error) {
	fileSet := make(map[string]ExistingDirectoryFile, len(files))
	var totalBytes uint64
	for _, file := range files {
		if err := requirePortableExistingPath(file.RelativePath); err != nil {
			return nil, err
		}
		if file.ByteLength > maximumExistingDirectoryFileBytes || !isSHA256(file.SHA256) {
			return nil, fmt.Errorf("%w: sealed artifact file metadata is outside its authority", ErrUnsafe)
		}
		if totalBytes > maximumExistingDirectoryTotalBytes-file.ByteLength {
			return nil, fmt.Errorf("%w: sealed artifact bytes exceed their total authority", ErrUnsafe)
		}
		totalBytes += file.ByteLength
		if _, repeated := fileSet[file.RelativePath]; repeated {
			return nil, fmt.Errorf("%w: sealed artifact file repeats", ErrUnsafe)
		}
		key := portableExistingPathKey(file.RelativePath)
		if _, collided := portableSet[key]; collided {
			return nil, fmt.Errorf("%w: sealed artifact paths collide portably", ErrUnsafe)
		}
		fileSet[file.RelativePath] = file
		portableSet[key] = struct{}{}
	}
	return fileSet, nil
}

func requireExistingManifestAuthority(
	fileSet map[string]ExistingDirectoryFile,
	manifestPath string,
	expectedManifestSHA256 string,
) error {
	manifestFile, ok := fileSet[manifestPath]
	if !ok {
		return fmt.Errorf("%w: sealed artifact manifest is absent from inventory", ErrUnsafe)
	}
	if manifestFile.ByteLength < 1 || manifestFile.ByteLength > maximumExistingDirectoryManifestBytes ||
		manifestFile.SHA256 != expectedManifestSHA256 {
		return fmt.Errorf("%w: sealed artifact manifest does not match external authority", ErrUnsafe)
	}
	return nil
}

func requireExistingParentClosure(index existingDirectoryInventoryIndex) error {
	for _, directory := range index.directories {
		parent := path.Dir(directory)
		if parent != "." {
			if _, ok := index.directorySet[parent]; !ok {
				return fmt.Errorf("%w: sealed artifact directory parent is absent", ErrUnsafe)
			}
		}
	}
	for _, file := range index.files {
		parent := path.Dir(file.RelativePath)
		if parent != "." {
			if _, ok := index.directorySet[parent]; !ok {
				return fmt.Errorf("%w: sealed artifact file parent is absent", ErrUnsafe)
			}
		}
	}
	return nil
}

func normalizeExistingSnapshots(
	snapshotPaths []string,
	files map[string]ExistingDirectoryFile,
) ([]string, error) {
	if len(snapshotPaths) > maximumExistingDirectorySnapshots || !sort.StringsAreSorted(snapshotPaths) {
		return nil, fmt.Errorf("%w: sealed artifact snapshots are not canonically bounded", ErrUnsafe)
	}
	normalized := slices.Clone(snapshotPaths)
	var total uint64
	for index, snapshotPath := range normalized {
		if index > 0 && normalized[index-1] == snapshotPath {
			return nil, fmt.Errorf("%w: sealed artifact snapshot repeats", ErrUnsafe)
		}
		file, ok := files[snapshotPath]
		if !ok || file.ByteLength > maximumExistingDirectorySnapshotBytes ||
			total > maximumExistingDirectorySnapshotTotalBytes-file.ByteLength {
			return nil, fmt.Errorf("%w: sealed artifact snapshots exceed their byte authority", ErrUnsafe)
		}
		total += file.ByteLength
	}
	return normalized, nil
}

func buildExistingDirectoryTree(
	directories []string,
	files []ExistingDirectoryFile,
) *existingDirectoryNode {
	root := &existingDirectoryNode{directories: map[string]*existingDirectoryNode{}, files: map[string]ExistingDirectoryFile{}}
	nodes := map[string]*existingDirectoryNode{"": root}
	for _, relative := range directories {
		parentPath := path.Dir(relative)
		if parentPath == "." {
			parentPath = ""
		}
		parent := nodes[parentPath]
		node := &existingDirectoryNode{
			relativePath: relative,
			directories:  map[string]*existingDirectoryNode{},
			files:        map[string]ExistingDirectoryFile{},
		}
		parent.directories[path.Base(relative)] = node
		nodes[relative] = node
	}
	for _, file := range files {
		parentPath := path.Dir(file.RelativePath)
		if parentPath == "." {
			parentPath = ""
		}
		nodes[parentPath].files[path.Base(file.RelativePath)] = file
	}
	return root
}

func requirePortableExistingPath(value string) error {
	if !utf8.ValidString(value) || !norm.NFC.IsNormalString(value) || len(value) < 1 ||
		len(value) > maximumExistingDirectoryPathBytes || strings.HasPrefix(value, "/") ||
		strings.ContainsAny(value, "\\:<>\"|?*\x00") {
		return fmt.Errorf("%w: sealed artifact path is not portable NFC", ErrUnsafe)
	}
	segments := strings.Split(value, "/")
	if len(segments) > maximumExistingDirectoryDepth {
		return fmt.Errorf("%w: sealed artifact path exceeds its depth authority", ErrUnsafe)
	}
	for _, segment := range segments {
		if len(segment) < 1 || len(segment) > maximumNameBytes || segment == "." || segment == ".." ||
			strings.HasSuffix(segment, ".") || strings.HasSuffix(segment, " ") ||
			containsPortableControl(segment) || containsNonASCIIExistingCase(segment) || isDOSDeviceName(segment) {
			return fmt.Errorf("%w: sealed artifact path contains a non-portable component", ErrUnsafe)
		}
	}
	return nil
}

func containsPortableControl(value string) bool {
	for _, current := range value {
		if current < 0x20 || current == 0x7f {
			return true
		}
	}
	return false
}

func isDOSDeviceName(segment string) bool {
	base := strings.ToUpper(strings.SplitN(segment, ".", 2)[0])
	if base == "CON" || base == "PRN" || base == "AUX" || base == "NUL" || base == "CLOCK$" ||
		base == "CONIN$" || base == "CONOUT$" {
		return true
	}
	if len(base) == 4 && (strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT")) {
		return base[3] >= '1' && base[3] <= '9'
	}
	if strings.HasPrefix(base, "COM") || strings.HasPrefix(base, "LPT") {
		suffix := strings.TrimPrefix(strings.TrimPrefix(base, "COM"), "LPT")
		return suffix == "¹" || suffix == "²" || suffix == "³"
	}
	return false
}

func portableExistingPathKey(value string) string {
	return strings.Map(func(current rune) rune {
		if current >= 'A' && current <= 'Z' {
			return current + ('a' - 'A')
		}
		return current
	}, value)
}

func containsNonASCIIExistingCase(value string) bool {
	for _, current := range value {
		if current <= 0x7f {
			continue
		}
		scalar := string(current)
		if cases.Upper(language.Und).String(scalar) != cases.Lower(language.Und).String(scalar) {
			return true
		}
	}
	return false
}

func validExistingStagingName(value string) bool {
	if !strings.HasPrefix(value, existingDirectoryStagingPrefix) ||
		len(value) != len(existingDirectoryStagingPrefix)+32 {
		return false
	}
	for _, current := range value[len(existingDirectoryStagingPrefix):] {
		if (current < '0' || current > '9') && (current < 'a' || current > 'f') {
			return false
		}
	}
	return true
}

func isSHA256(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == sha256.Size && strings.ToLower(value) == value
}

func compareExistingFiles(left, right ExistingDirectoryFile) int {
	return strings.Compare(left.RelativePath, right.RelativePath)
}

func findExistingFile(files []ExistingDirectoryFile, relativePath string) *ExistingDirectoryFile {
	index, found := slices.BinarySearchFunc(files, ExistingDirectoryFile{RelativePath: relativePath}, compareExistingFiles)
	if !found {
		return nil
	}
	return &files[index]
}
