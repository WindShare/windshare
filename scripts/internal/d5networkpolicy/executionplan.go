package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
)

const (
	executionPlanSchemaVersion = 1
	executionPlanOperation     = "go-test-execution"
	executionPlanDigestDomain  = "d5-go-test-execution-plan-v1"
	networkAccessNone          = "none"
	networkAccessParentPipe    = "parent-owned-one-use-pipe"
)

var (
	sha256TextPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	requestIDPattern  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)
)

type executionSourceIdentity struct {
	IdentityKind  string `json:"IdentityKind"`
	Commit        string `json:"Commit"`
	WorktreeClean bool   `json:"WorktreeClean"`
	SourceDigest  string `json:"SourceDigest"`
}

type executionProgram struct {
	Path   string `json:"Path"`
	Bytes  int64  `json:"Bytes"`
	SHA256 string `json:"SHA256"`
}

type executionPlanRequest struct {
	SchemaVersion int                         `json:"SchemaVersion"`
	RunID         string                      `json:"RunID"`
	Source        executionSourceIdentity     `json:"Source"`
	Operations    []executionOperationRequest `json:"Operations"`
}

type executionOperationRequest struct {
	RequestID        string           `json:"RequestID"`
	PackagePath      string           `json:"PackagePath"`
	Executable       executionProgram `json:"Executable"`
	WorkingDirectory string           `json:"WorkingDirectory"`
	Arguments        []string         `json:"Arguments"`
}

type plannedSemanticEntry struct {
	Kind            string `json:"Kind"`
	Name            string `json:"Name"`
	RequiresNetwork bool   `json:"RequiresOSNetwork"`
}

type testExecutionPlan struct {
	SchemaVersion            int                     `json:"SchemaVersion"`
	Operation                string                  `json:"Operation"`
	PlanSHA256               string                  `json:"PlanSHA256"`
	RequestID                string                  `json:"RequestID"`
	RunID                    string                  `json:"RunID"`
	PackagePath              string                  `json:"PackagePath"`
	NetworkAccess            string                  `json:"NetworkAccess"`
	SelectionClass           string                  `json:"SelectionClass"`
	LifecycleRequiresNetwork bool                    `json:"LifecycleRequiresOSNetwork"`
	Executable               executionProgram        `json:"Executable"`
	WorkingDirectory         string                  `json:"WorkingDirectory"`
	Arguments                []string                `json:"Arguments"`
	Source                   executionSourceIdentity `json:"Source"`
	Entries                  []plannedSemanticEntry  `json:"Entries"`
}

type executionPlanDocument struct {
	SchemaVersion int                     `json:"SchemaVersion"`
	RunID         string                  `json:"RunID"`
	Source        executionSourceIdentity `json:"Source"`
	Plans         []testExecutionPlan     `json:"Plans"`
}

func loadExecutionPlanRequest(path string) (executionPlanRequest, error) {
	file, err := os.Open(path)
	if err != nil {
		return executionPlanRequest{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var request executionPlanRequest
	if err := decoder.Decode(&request); err != nil {
		return executionPlanRequest{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return executionPlanRequest{}, errors.New("execution-plan request contains trailing JSON")
	}
	return request, nil
}

func buildExecutionPlanDocument(
	root string,
	expectedPackages map[string]bool,
	catalog semanticCatalog,
	request executionPlanRequest,
) (executionPlanDocument, error) {
	if request.SchemaVersion != executionPlanSchemaVersion ||
		strings.TrimSpace(request.RunID) == "" || len(request.Operations) == 0 {
		return executionPlanDocument{}, errors.New("execution-plan request identity is invalid")
	}
	source, err := normalizeExecutionSource(request.Source)
	if err != nil {
		return executionPlanDocument{}, err
	}
	entriesByPackage := make(map[string][]semanticEntry)
	for _, entry := range catalog.entries {
		entriesByPackage[entry.PackagePath] = append(entriesByPackage[entry.PackagePath], entry)
	}
	document := executionPlanDocument{
		SchemaVersion: executionPlanSchemaVersion,
		RunID:         request.RunID,
		Source:        source,
	}
	seen := make(map[string]bool)
	for _, operation := range request.Operations {
		if !requestIDPattern.MatchString(operation.RequestID) || seen[operation.RequestID] {
			return executionPlanDocument{}, fmt.Errorf("invalid or duplicate execution-plan request ID %q", operation.RequestID)
		}
		seen[operation.RequestID] = true
		packagePath := normalizePackagePath(operation.PackagePath)
		if !expectedPackages[packagePath] {
			return executionPlanDocument{}, fmt.Errorf("execution-plan package is absent from the fixed manifest: %s", packagePath)
		}
		program, err := validateExecutionProgram(operation.Executable)
		if err != nil {
			return executionPlanDocument{}, fmt.Errorf("execution-plan %s: %w", operation.RequestID, err)
		}
		workingDirectory, err := validateExecutionWorkingDirectory(root, packagePath, operation.WorkingDirectory)
		if err != nil {
			return executionPlanDocument{}, fmt.Errorf("execution-plan %s: %w", operation.RequestID, err)
		}
		selected, err := selectSemanticEntries(entriesByPackage[packagePath], operation.Arguments)
		if err != nil {
			return executionPlanDocument{}, fmt.Errorf("execution-plan %s: %w", operation.RequestID, err)
		}
		plan := testExecutionPlan{
			SchemaVersion:            executionPlanSchemaVersion,
			Operation:                executionPlanOperation,
			RequestID:                operation.RequestID,
			RunID:                    request.RunID,
			PackagePath:              packagePath,
			LifecycleRequiresNetwork: catalog.lifecycle[packagePath],
			Executable:               program,
			WorkingDirectory:         workingDirectory,
			Arguments:                append([]string(nil), operation.Arguments...),
			Source:                   source,
			Entries:                  selected,
		}
		plan.NetworkAccess, plan.SelectionClass = classifySelection(
			plan.Entries,
			plan.LifecycleRequiresNetwork,
		)
		plan.PlanSHA256 = executionPlanSHA256(plan)
		document.Plans = append(document.Plans, plan)
	}
	return document, nil
}

func normalizeExecutionSource(source executionSourceIdentity) (executionSourceIdentity, error) {
	source.SourceDigest = strings.ToLower(source.SourceDigest)
	if (source.IdentityKind != "git-commit" && source.IdentityKind != "workspace-manifest") ||
		strings.TrimSpace(source.Commit) == "" || !sha256TextPattern.MatchString(source.SourceDigest) {
		return executionSourceIdentity{}, errors.New("execution-plan source identity is invalid")
	}
	return source, nil
}

func validateExecutionProgram(program executionProgram) (executionProgram, error) {
	if !filepath.IsAbs(program.Path) || program.Bytes <= 0 {
		return executionProgram{}, errors.New("executable evidence is invalid")
	}
	path, err := filepath.Abs(program.Path)
	if err != nil {
		return executionProgram{}, fmt.Errorf("resolve executable: %w", err)
	}
	program.SHA256 = strings.ToLower(program.SHA256)
	if !sha256TextPattern.MatchString(program.SHA256) {
		return executionProgram{}, errors.New("executable digest is invalid")
	}
	file, err := os.Open(path)
	if err != nil {
		return executionProgram{}, fmt.Errorf("open executable: %w", err)
	}
	defer file.Close()
	stat, err := file.Stat()
	if err != nil {
		return executionProgram{}, fmt.Errorf("stat executable: %w", err)
	}
	digest := sha256.New()
	if _, err := io.Copy(digest, file); err != nil {
		return executionProgram{}, fmt.Errorf("hash executable: %w", err)
	}
	actualSHA256 := hex.EncodeToString(digest.Sum(nil))
	if stat.Size() != program.Bytes || actualSHA256 != program.SHA256 {
		return executionProgram{}, errors.New("executable differs from its parent-owned evidence")
	}
	program.Path = path
	return program, nil
}

func validateExecutionWorkingDirectory(root, packagePath, requested string) (string, error) {
	if !filepath.IsAbs(requested) {
		return "", errors.New("working directory is not absolute")
	}
	actual, err := filepath.Abs(requested)
	if err != nil {
		return "", fmt.Errorf("resolve working directory: %w", err)
	}
	expected, err := filepath.Abs(filepath.Join(root, filepath.FromSlash(packagePath)))
	if err != nil {
		return "", fmt.Errorf("resolve package working directory: %w", err)
	}
	if !sameExecutionPath(actual, expected) {
		return "", fmt.Errorf("working directory %s does not match package %s", actual, packagePath)
	}
	stat, err := os.Stat(actual)
	if err != nil || !stat.IsDir() {
		return "", fmt.Errorf("working directory is unavailable: %s", actual)
	}
	return actual, nil
}

func sameExecutionPath(left, right string) bool {
	left, right = filepath.Clean(left), filepath.Clean(right)
	if runtime.GOOS == "windows" {
		return strings.EqualFold(left, right)
	}
	return left == right
}

func normalizePackagePath(path string) string {
	return strings.TrimPrefix(filepath.ToSlash(filepath.Clean(path)), "./")
}

func classifySelection(entries []plannedSemanticEntry, lifecycleNetwork bool) (string, string) {
	hasNetwork := lifecycleNetwork
	hasPure := false
	for _, entry := range entries {
		hasNetwork = hasNetwork || entry.RequiresNetwork
		hasPure = hasPure || !entry.RequiresNetwork
	}
	if hasNetwork {
		switch {
		case lifecycleNetwork && !anyNetworkEntry(entries):
			return networkAccessParentPipe, "network-lifecycle"
		case hasPure:
			return networkAccessParentPipe, "mixed-network"
		default:
			return networkAccessParentPipe, "network"
		}
	}
	if len(entries) == 0 {
		return networkAccessNone, "empty"
	}
	return networkAccessNone, "non-network"
}

func anyNetworkEntry(entries []plannedSemanticEntry) bool {
	for _, entry := range entries {
		if entry.RequiresNetwork {
			return true
		}
	}
	return false
}

func executionPlanSHA256(plan testExecutionPlan) string {
	fields := []string{
		executionPlanDigestDomain,
		strconv.Itoa(plan.SchemaVersion),
		plan.Operation,
		plan.RequestID,
		plan.RunID,
		plan.PackagePath,
		plan.NetworkAccess,
		plan.SelectionClass,
		plan.Source.IdentityKind,
		plan.Source.Commit,
		strconv.FormatBool(plan.Source.WorktreeClean),
		plan.Source.SourceDigest,
		plan.Executable.Path,
		strconv.FormatInt(plan.Executable.Bytes, 10),
		plan.Executable.SHA256,
		plan.WorkingDirectory,
		strconv.Itoa(len(plan.Arguments)),
	}
	fields = append(fields, plan.Arguments...)
	fields = append(fields,
		strconv.FormatBool(plan.LifecycleRequiresNetwork),
		strconv.Itoa(len(plan.Entries)),
	)
	for _, entry := range plan.Entries {
		fields = append(fields, entry.Kind, entry.Name, strconv.FormatBool(entry.RequiresNetwork))
	}
	var payload bytes.Buffer
	for _, field := range fields {
		fmt.Fprintf(&payload, "%d:", len([]byte(field)))
		payload.WriteString(field)
		payload.WriteByte('\n')
	}
	digest := sha256.Sum256(payload.Bytes())
	return hex.EncodeToString(digest[:])
}

func writeExecutionPlanDocument(path string, document executionPlanDocument) error {
	raw, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(path, raw, 0o600)
}
