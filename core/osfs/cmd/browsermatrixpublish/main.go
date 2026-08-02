package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/windshare/windshare/core/osfs/artifactpublish"
)

const (
	protocolVersion   = "windshare.artifact-publisher/v2"
	maximumInputSize  = 48 << 20
	selfCheckArgument = "self-check"
)

type artifactRequest struct {
	Name        string `json:"name"`
	BytesBase64 string `json:"bytesBase64"`
	SHA256      string `json:"sha256"`
}

type request struct {
	SchemaVersion          string                     `json:"schemaVersion"`
	Operation              string                     `json:"operation"`
	ParentPath             string                     `json:"parentPath"`
	OutputName             string                     `json:"outputName"`
	StagingName            string                     `json:"stagingName"`
	Artifacts              []artifactRequest          `json:"artifacts"`
	Inventory              *directoryInventoryRequest `json:"inventory,omitempty"`
	ManifestPath           string                     `json:"manifestPath,omitempty"`
	ExpectedManifestSHA256 string                     `json:"expectedManifestSha256,omitempty"`
	SnapshotPaths          []string                   `json:"snapshotPaths,omitempty"`
	StagingReceipt         string                     `json:"stagingReceipt,omitempty"`
}

type directoryInventoryRequest struct {
	Directories []string                       `json:"directories"`
	Files       []existingDirectoryFileRequest `json:"files"`
}

type existingDirectoryFileRequest struct {
	RelativePath string `json:"relativePath"`
	ByteLength   string `json:"byteLength"`
	SHA256       string `json:"sha256"`
}

type artifactResponse struct {
	Name        string `json:"name"`
	BytesBase64 string `json:"bytesBase64"`
	SHA256      string `json:"sha256"`
}

type response struct {
	SchemaVersion  string             `json:"schemaVersion"`
	Outcome        string             `json:"outcome"`
	FailureCode    *string            `json:"failureCode"`
	Artifacts      []artifactResponse `json:"artifacts"`
	ManifestSHA256 string             `json:"manifestSha256,omitempty"`
	Snapshots      []snapshotResponse `json:"snapshots,omitempty"`
	StagingReceipt string             `json:"stagingReceipt,omitempty"`
	CleanupOutcome string             `json:"cleanupOutcome,omitempty"`
}

type snapshotResponse struct {
	RelativePath string `json:"relativePath"`
	ByteLength   string `json:"byteLength"`
	BytesBase64  string `json:"bytesBase64"`
	SHA256       string `json:"sha256"`
}

type publishedResult struct {
	artifacts      []artifactpublish.PublishedArtifact
	manifestSHA256 string
	snapshots      []artifactpublish.ExistingDirectorySnapshot
	stagingReceipt []byte
	cleanupOutcome artifactpublish.ExistingDirectoryCleanupOutcome
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(arguments []string, input io.Reader, output, errorOutput io.Writer) int {
	if len(arguments) == 1 && arguments[0] == selfCheckArgument {
		return writeSelfCheck(output)
	}
	if len(arguments) != 0 {
		return writeFailure(errorOutput, "protocol-invalid", 2)
	}
	request, err := decodeRequest(input)
	if err != nil {
		return writeFailure(errorOutput, "protocol-invalid", 2)
	}
	result, err := publish(request)
	if err != nil {
		if errors.Is(err, artifactpublish.ErrCollision) {
			return writeFailureResult(errorOutput, "destination-exists", 3, result)
		}
		return writeFailureResult(errorOutput, "publication-unsafe", 2, result)
	}
	encoded := response{
		SchemaVersion:  protocolVersion,
		Outcome:        "completed",
		FailureCode:    nil,
		Artifacts:      make([]artifactResponse, 0, len(result.artifacts)),
		ManifestSHA256: result.manifestSHA256,
		Snapshots:      make([]snapshotResponse, 0, len(result.snapshots)),
		StagingReceipt: base64.StdEncoding.EncodeToString(result.stagingReceipt),
		CleanupOutcome: string(result.cleanupOutcome),
	}
	for _, artifact := range result.artifacts {
		encoded.Artifacts = append(encoded.Artifacts, artifactResponse{
			Name: artifact.Name, BytesBase64: base64.StdEncoding.EncodeToString(artifact.Bytes), SHA256: artifact.SHA256,
		})
	}
	for _, snapshot := range result.snapshots {
		encoded.Snapshots = append(encoded.Snapshots, snapshotResponse{
			RelativePath: snapshot.RelativePath,
			ByteLength:   strconv.Itoa(len(snapshot.Bytes)),
			BytesBase64:  base64.StdEncoding.EncodeToString(snapshot.Bytes),
			SHA256:       snapshot.SHA256,
		})
	}
	if err := json.NewEncoder(output).Encode(encoded); err != nil {
		return writeFailure(errorOutput, "response-failed", 2)
	}
	return 0
}

func decodeRequest(input io.Reader) (request, error) {
	encoded, err := io.ReadAll(io.LimitReader(input, maximumInputSize+1))
	if err != nil {
		return request{}, err
	}
	if len(encoded) < 1 || len(encoded) > maximumInputSize {
		return request{}, errors.New("artifact helper request exceeds its byte authority")
	}
	decoder := json.NewDecoder(bytes.NewReader(encoded))
	decoder.DisallowUnknownFields()
	var decoded request
	if err := decoder.Decode(&decoded); err != nil {
		return request{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return request{}, errors.New("artifact helper request has trailing JSON")
	}
	if decoded.SchemaVersion != protocolVersion {
		return request{}, errors.New("artifact helper protocol version is invalid")
	}
	canonical, err := json.Marshal(decoded)
	if err != nil || !bytes.Equal(encoded, canonical) {
		return request{}, errors.New("artifact helper request is not canonical JSON")
	}
	return decoded, nil
}

func publish(decoded request) (publishedResult, error) {
	artifacts, err := decodeArtifacts(decoded.Artifacts)
	if err != nil {
		return publishedResult{}, err
	}

	switch decoded.Operation {
	case "directory":
		return publishDirectory(decoded, artifacts)
	case "file":
		return publishFile(decoded, artifacts)
	case "prepare-existing-directory":
		return prepareExistingDirectory(decoded)
	case "publish-existing-directory":
		return publishExistingDirectory(decoded)
	case "verify-existing-directory":
		return verifyExistingDirectory(decoded)
	case "cleanup-existing-directory":
		return cleanupExistingDirectory(decoded)
	default:
		return publishedResult{}, fmt.Errorf("artifact helper operation is invalid")
	}
}

func decodeArtifacts(encodedArtifacts []artifactRequest) ([]artifactpublish.Artifact, error) {
	artifacts := make([]artifactpublish.Artifact, 0, len(encodedArtifacts))
	for _, encoded := range encodedArtifacts {
		bytes, err := base64.StdEncoding.Strict().DecodeString(encoded.BytesBase64)
		if err != nil {
			return nil, errors.New("artifact helper bytes are not canonical base64")
		}
		artifacts = append(artifacts, artifactpublish.Artifact{
			Name: encoded.Name, Bytes: bytes, SHA256: encoded.SHA256,
		})
	}
	return artifacts, nil
}

func publishDirectory(decoded request, artifacts []artifactpublish.Artifact) (publishedResult, error) {
	if err := requireLegacyOperationShape(decoded); err != nil {
		return publishedResult{}, err
	}
	result, err := artifactpublish.PublishDirectory(artifactpublish.DirectoryRequest{
		ParentPath: decoded.ParentPath, OutputName: decoded.OutputName,
		StagingName: decoded.StagingName, Artifacts: artifacts,
	})
	return publishedResult{artifacts: result.Artifacts}, err
}

func publishFile(decoded request, artifacts []artifactpublish.Artifact) (publishedResult, error) {
	if err := requireLegacyOperationShape(decoded); err != nil {
		return publishedResult{}, err
	}
	if len(artifacts) != 1 {
		return publishedResult{}, errors.New("artifact helper file operation requires one artifact")
	}
	result, err := artifactpublish.PublishFile(artifactpublish.FileRequest{
		ParentPath: decoded.ParentPath, OutputName: decoded.OutputName,
		StagingName: decoded.StagingName, Artifact: artifacts[0],
	})
	return publishedResult{artifacts: result.Artifacts}, err
}

func prepareExistingDirectory(decoded request) (publishedResult, error) {
	if err := requirePrepareExistingDirectoryShape(decoded); err != nil {
		return publishedResult{}, err
	}
	inventory, err := existingDirectoryInventory(decoded)
	if err != nil {
		return publishedResult{}, err
	}
	receipt, err := artifactpublish.PrepareExistingDirectoryStaging(artifactpublish.ExistingDirectoryStagingRequest{
		ParentPath:             decoded.ParentPath,
		StagingName:            decoded.StagingName,
		Inventory:              inventory,
		ManifestPath:           decoded.ManifestPath,
		ExpectedManifestSHA256: decoded.ExpectedManifestSHA256,
	})
	if err != nil {
		return publishedResult{stagingReceipt: receipt.Bytes()}, err
	}
	return publishedResult{artifacts: []artifactpublish.PublishedArtifact{}, stagingReceipt: receipt.Bytes()}, nil
}

func publishExistingDirectory(decoded request) (publishedResult, error) {
	if err := requirePublishExistingDirectoryShape(decoded); err != nil {
		return publishedResult{}, err
	}
	inventory, err := existingDirectoryInventory(decoded)
	if err != nil {
		return publishedResult{}, err
	}
	receiptBytes, err := decodeStagingReceipt(decoded.StagingReceipt)
	if err != nil {
		return publishedResult{}, err
	}
	result, err := artifactpublish.PublishExistingDirectory(artifactpublish.ExistingDirectoryRequest{
		ParentPath: decoded.ParentPath, OutputName: decoded.OutputName,
		StagingName: decoded.StagingName, Inventory: inventory,
		ManifestPath:           decoded.ManifestPath,
		ExpectedManifestSHA256: decoded.ExpectedManifestSHA256,
		SnapshotPaths:          decoded.SnapshotPaths,
		Receipt:                artifactpublish.NewExistingDirectoryStagingReceipt(receiptBytes),
	})
	return existingPublishedResult(result), err
}

func verifyExistingDirectory(decoded request) (publishedResult, error) {
	if err := requireVerifyExistingDirectoryShape(decoded); err != nil {
		return publishedResult{}, err
	}
	inventory, err := existingDirectoryInventory(decoded)
	if err != nil {
		return publishedResult{}, err
	}
	result, err := artifactpublish.VerifyExistingDirectory(artifactpublish.ExistingDirectoryVerificationRequest{
		ParentPath: decoded.ParentPath, OutputName: decoded.OutputName,
		Inventory: inventory, ManifestPath: decoded.ManifestPath,
		ExpectedManifestSHA256: decoded.ExpectedManifestSHA256,
		SnapshotPaths:          decoded.SnapshotPaths,
	})
	return existingPublishedResult(result), err
}

func cleanupExistingDirectory(decoded request) (publishedResult, error) {
	if err := requireCleanupExistingDirectoryShape(decoded); err != nil {
		return publishedResult{}, err
	}
	inventory, err := existingDirectoryInventory(decoded)
	if err != nil {
		return publishedResult{}, err
	}
	receiptBytes, err := decodeStagingReceipt(decoded.StagingReceipt)
	if err != nil {
		return publishedResult{}, err
	}
	outcome, err := artifactpublish.CleanupExistingDirectoryStaging(artifactpublish.ExistingDirectoryCleanupRequest{
		ParentPath:             decoded.ParentPath,
		StagingName:            decoded.StagingName,
		Inventory:              inventory,
		ManifestPath:           decoded.ManifestPath,
		ExpectedManifestSHA256: decoded.ExpectedManifestSHA256,
		Receipt:                artifactpublish.NewExistingDirectoryStagingReceipt(receiptBytes),
	})
	return publishedResult{
		artifacts:      []artifactpublish.PublishedArtifact{},
		cleanupOutcome: outcome,
	}, err
}

func existingDirectoryInventory(decoded request) (artifactpublish.ExistingDirectoryInventory, error) {
	if decoded.Artifacts == nil || len(decoded.Artifacts) != 0 || decoded.Inventory == nil ||
		decoded.Inventory.Directories == nil || decoded.Inventory.Files == nil {
		return artifactpublish.ExistingDirectoryInventory{},
			errors.New("artifact helper existing-directory operation requires one inventory and no inline artifacts")
	}
	files := make([]artifactpublish.ExistingDirectoryFile, 0, len(decoded.Inventory.Files))
	for _, file := range decoded.Inventory.Files {
		byteLength, err := parseCanonicalUint64(file.ByteLength)
		if err != nil {
			return artifactpublish.ExistingDirectoryInventory{}, err
		}
		files = append(files, artifactpublish.ExistingDirectoryFile{
			RelativePath: file.RelativePath,
			ByteLength:   byteLength,
			SHA256:       file.SHA256,
		})
	}
	return artifactpublish.ExistingDirectoryInventory{
		Directories: decoded.Inventory.Directories,
		Files:       files,
	}, nil
}

func requireLegacyOperationShape(decoded request) error {
	if decoded.Artifacts == nil || decoded.Inventory != nil || decoded.ManifestPath != "" || decoded.ExpectedManifestSHA256 != "" ||
		len(decoded.SnapshotPaths) != 0 || decoded.StagingReceipt != "" {
		return errors.New("artifact helper legacy operation contains existing-directory authority")
	}
	return nil
}

func requirePrepareExistingDirectoryShape(decoded request) error {
	if decoded.Artifacts == nil || len(decoded.Artifacts) != 0 || decoded.Inventory == nil || len(decoded.Inventory.Files) == 0 ||
		decoded.OutputName != artifactpublish.ExistingDirectoryOutputName || decoded.ManifestPath == "" ||
		decoded.ExpectedManifestSHA256 == "" || len(decoded.SnapshotPaths) != 0 || decoded.StagingReceipt != "" {
		return errors.New("artifact helper staging preparation fields are invalid")
	}
	return nil
}

func requirePublishExistingDirectoryShape(decoded request) error {
	if decoded.Artifacts == nil || len(decoded.Artifacts) != 0 || decoded.Inventory == nil || decoded.StagingName == "" ||
		decoded.OutputName != artifactpublish.ExistingDirectoryOutputName || decoded.ManifestPath == "" ||
		decoded.ExpectedManifestSHA256 == "" || decoded.StagingReceipt == "" {
		return errors.New("artifact helper existing-directory publication fields are invalid")
	}
	return nil
}

func decodeStagingReceipt(encoded string) ([]byte, error) {
	receiptBytes, err := base64.StdEncoding.Strict().DecodeString(encoded)
	if err != nil || len(receiptBytes) < 1 || len(receiptBytes) > 4_096 ||
		base64.StdEncoding.EncodeToString(receiptBytes) != encoded {
		return nil, errors.New("artifact helper staging receipt is not canonical base64")
	}
	return receiptBytes, nil
}

func requireVerifyExistingDirectoryShape(decoded request) error {
	if decoded.Artifacts == nil || len(decoded.Artifacts) != 0 || decoded.Inventory == nil || decoded.StagingName != "" ||
		decoded.OutputName != artifactpublish.ExistingDirectoryOutputName || decoded.ManifestPath == "" ||
		decoded.ExpectedManifestSHA256 == "" || decoded.StagingReceipt != "" {
		return errors.New("artifact helper existing-directory verification fields are invalid")
	}
	return nil
}

func requireCleanupExistingDirectoryShape(decoded request) error {
	if decoded.Artifacts == nil || len(decoded.Artifacts) != 0 || decoded.Inventory == nil || decoded.StagingName == "" ||
		decoded.OutputName != artifactpublish.ExistingDirectoryOutputName || decoded.ManifestPath == "" ||
		decoded.ExpectedManifestSHA256 == "" || len(decoded.SnapshotPaths) != 0 || decoded.StagingReceipt == "" {
		return errors.New("artifact helper existing-directory cleanup fields are invalid")
	}
	return nil
}

func parseCanonicalUint64(encoded string) (uint64, error) {
	if encoded == "" || (len(encoded) > 1 && encoded[0] == '0') {
		return 0, errors.New("artifact helper byte length is not canonical unsigned decimal")
	}
	for _, current := range encoded {
		if current < '0' || current > '9' {
			return 0, errors.New("artifact helper byte length is not canonical unsigned decimal")
		}
	}
	value, err := strconv.ParseUint(encoded, 10, 64)
	if err != nil {
		return 0, errors.New("artifact helper byte length exceeds uint64 authority")
	}
	return value, nil
}

func existingPublishedResult(result artifactpublish.ExistingDirectoryResult) publishedResult {
	return publishedResult{
		artifacts:      []artifactpublish.PublishedArtifact{},
		manifestSHA256: result.ManifestSHA256,
		snapshots:      result.Snapshots,
	}
}

func writeSelfCheck(output io.Writer) int {
	// Self-check exercises the helper's protocol encoder without creating a
	// disposable publication tree or mutating caller-owned filesystem state.
	record := struct {
		SchemaVersion string `json:"schemaVersion"`
		Outcome       string `json:"outcome"`
	}{SchemaVersion: protocolVersion, Outcome: "ready"}
	if err := json.NewEncoder(output).Encode(record); err != nil {
		return 2
	}
	return 0
}

func writeFailure(output io.Writer, failureCode string, exitCode int) int {
	return writeFailureResult(output, failureCode, exitCode, publishedResult{})
}

func writeFailureResult(
	output io.Writer,
	failureCode string,
	exitCode int,
	result publishedResult,
) int {
	encoded := response{
		SchemaVersion:  protocolVersion,
		Outcome:        "failed",
		FailureCode:    &failureCode,
		Artifacts:      []artifactResponse{},
		StagingReceipt: base64.StdEncoding.EncodeToString(result.stagingReceipt),
		CleanupOutcome: string(result.cleanupOutcome),
	}
	if err := json.NewEncoder(output).Encode(encoded); err != nil {
		return 2
	}
	return exitCode
}
