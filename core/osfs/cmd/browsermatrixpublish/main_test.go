package main

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"hash"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/osfs/artifactpublish"
)

func TestRunRejectsArgumentsAndMalformedRequests(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name      string
		arguments []string
		input     string
	}{
		{name: "arguments", arguments: []string{"unexpected"}, input: "{}"},
		{name: "malformed", input: "{"},
		{name: "unknown field", input: `{"schemaVersion":"` + protocolVersion + `","extra":true}`},
		{name: "wrong version", input: `{"schemaVersion":"wrong"}`},
		{name: "trailing", input: `{"schemaVersion":"` + protocolVersion + `"}{}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var output bytes.Buffer
			if exitCode := run(test.arguments, strings.NewReader(test.input), &output, &output); exitCode != 2 {
				t.Fatalf("run exit code = %d, want 2", exitCode)
			}
			assertFailureResponse(t, output.Bytes(), "protocol-invalid")
		})
	}
}

func TestRunSelfCheckIsExactAndMutationFree(t *testing.T) {
	t.Parallel()
	var output, failure bytes.Buffer
	if exitCode := run([]string{selfCheckArgument}, strings.NewReader("ignored"), &output, &failure); exitCode != 0 {
		t.Fatalf("self-check exit code = %d", exitCode)
	}
	expected := "{\"schemaVersion\":\"windshare.artifact-publisher/v2\",\"outcome\":\"ready\"}\n"
	if output.String() != expected || failure.Len() != 0 {
		t.Fatalf("self-check channels differ: stdout=%q stderr=%q", output.String(), failure.String())
	}
}

func TestPublishRejectsInvalidOperationAndFileCardinality(t *testing.T) {
	t.Parallel()
	if _, err := publish(request{Operation: "unknown"}); err == nil {
		t.Fatal("unknown operation unexpectedly accepted")
	}
	if _, err := publish(request{Operation: "file"}); err == nil {
		t.Fatal("empty file operation unexpectedly accepted")
	}
	if _, err := publish(request{
		Operation: "file",
		Artifacts: []artifactRequest{{BytesBase64: "***"}},
	}); err == nil {
		t.Fatal("invalid base64 unexpectedly accepted")
	}
}

func TestPublishRejectsCrossOperationAuthorityFields(t *testing.T) {
	t.Parallel()
	legacy := validFileRequest(t)
	legacy.Inventory = &directoryInventoryRequest{Directories: []string{}, Files: []existingDirectoryFileRequest{}}
	if _, err := publish(legacy); err == nil {
		t.Fatal("legacy operation accepted existing-directory inventory")
	}
	verify := existingDirectoryHelperRequest(t, "verify-existing-directory", false)
	verify.StagingName = ".browser-evidence-upload-aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := publish(verify); err == nil {
		t.Fatal("verification accepted staging mutation authority")
	}
	prepare := existingDirectoryHelperRequest(t, "prepare-existing-directory", false)
	prepare.SnapshotPaths = []string{"manifest.json"}
	if _, err := publish(prepare); err == nil {
		t.Fatal("staging preparation accepted snapshot authority")
	}
	publishExisting := existingDirectoryHelperRequest(t, "publish-existing-directory", false)
	if _, err := publish(publishExisting); err == nil {
		t.Fatal("existing-directory publication accepted an absent preparation receipt")
	}
}

func TestPublishRejectsMalformedOperationSpecificAuthorities(t *testing.T) {
	t.Parallel()

	invalidLength := func(value *request) {
		value.Inventory.Files[0].ByteLength = "01"
	}
	validReceipt := base64.StdEncoding.EncodeToString([]byte("prepared-receipt"))
	tests := []struct {
		name   string
		value  request
		mutate func(*request)
	}{
		{
			name:  "directory authority",
			value: request{Operation: "directory", Artifacts: []artifactRequest{}},
			mutate: func(value *request) {
				value.Inventory = &directoryInventoryRequest{Directories: []string{}, Files: []existingDirectoryFileRequest{}}
			},
		},
		{
			name:   "file cardinality",
			value:  request{Operation: "file", Artifacts: []artifactRequest{}},
			mutate: func(*request) {},
		},
		{
			name:   "prepare inventory length",
			value:  existingDirectoryHelperRequest(t, "prepare-existing-directory", false),
			mutate: invalidLength,
		},
		{
			name:   "prepare native policy",
			value:  existingDirectoryHelperRequest(t, "prepare-existing-directory", false),
			mutate: func(value *request) { value.ExpectedManifestSHA256 = "invalid" },
		},
		{
			name:   "publish inventory length",
			value:  existingDirectoryHelperRequest(t, "publish-existing-directory", false),
			mutate: func(value *request) { value.StagingReceipt = validReceipt; invalidLength(value) },
		},
		{
			name:   "publish receipt encoding",
			value:  existingDirectoryHelperRequest(t, "publish-existing-directory", false),
			mutate: func(value *request) { value.StagingReceipt = "***" },
		},
		{
			name:   "verify inventory length",
			value:  existingDirectoryHelperRequest(t, "verify-existing-directory", false),
			mutate: func(value *request) { value.StagingName = ""; invalidLength(value) },
		},
		{
			name:  "cleanup shape",
			value: existingDirectoryHelperRequest(t, "cleanup-existing-directory", false),
			mutate: func(value *request) {
				value.StagingReceipt = validReceipt
				value.SnapshotPaths = []string{"manifest.json"}
			},
		},
		{
			name:   "cleanup inventory length",
			value:  existingDirectoryHelperRequest(t, "cleanup-existing-directory", false),
			mutate: func(value *request) { value.StagingReceipt = validReceipt; invalidLength(value) },
		},
		{
			name:   "cleanup receipt encoding",
			value:  existingDirectoryHelperRequest(t, "cleanup-existing-directory", false),
			mutate: func(value *request) { value.StagingReceipt = "***" },
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			test.mutate(&test.value)
			if _, err := publish(test.value); err == nil {
				t.Fatal("malformed operation-specific authority unexpectedly accepted")
			}
		})
	}
}

func TestPublishRejectsNullExistingDirectoryArrays(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		mutate func(*request)
	}{
		{name: "artifacts", mutate: func(value *request) { value.Artifacts = nil }},
		{name: "directories", mutate: func(value *request) { value.Inventory.Directories = nil }},
		{name: "files", mutate: func(value *request) { value.Inventory.Files = nil }},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			request := existingDirectoryHelperRequest(t, "prepare-existing-directory", false)
			test.mutate(&request)
			if _, err := publish(request); err == nil {
				t.Fatalf("existing-directory operation accepted null %s", test.name)
			}
		})
	}
}

func TestDecodeRequestRequiresOneCanonicalJSONValue(t *testing.T) {
	t.Parallel()
	request := validFileRequest(t)
	request.ParentPath += "&authority"
	canonical, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("encode canonical request: %v", err)
	}
	if !bytes.Contains(canonical, []byte(`\u0026`)) {
		t.Fatalf("canonical request did not use encoding/json escaping: %s", canonical)
	}
	decoded, err := decodeRequest(bytes.NewReader(canonical))
	if err != nil || decoded.ParentPath != request.ParentPath {
		t.Fatalf("decode canonical request: decoded=%#v err=%v", decoded, err)
	}
	for _, noncanonical := range [][]byte{
		append(append([]byte(nil), canonical...), '\n'),
		append([]byte{' '}, canonical...),
	} {
		if _, err := decodeRequest(bytes.NewReader(noncanonical)); err == nil {
			t.Fatalf("noncanonical request unexpectedly accepted: %q", noncanonical)
		}
	}
}

func TestDecodeAndScalarParsersRejectNonCanonicalBoundaries(t *testing.T) {
	t.Parallel()
	if _, err := decodeRequest(strings.NewReader("")); err == nil {
		t.Fatal("empty request unexpectedly accepted")
	}
	for _, encoded := range []string{"", "00", "1x", "18446744073709551616"} {
		if _, err := parseCanonicalUint64(encoded); err == nil {
			t.Fatalf("noncanonical uint64 %q unexpectedly accepted", encoded)
		}
	}
	if exitCode := writeSelfCheck(failingWriter{}); exitCode != 2 {
		t.Fatalf("self-check writer failure exit code = %d, want 2", exitCode)
	}
}

func TestRunPublishesEachOperationAndReturnsNativeRereadBytes(t *testing.T) {
	t.Parallel()
	for _, operation := range []string{"directory", "file"} {
		t.Run(operation, func(t *testing.T) {
			t.Parallel()
			content := []byte("authenticated artifact\n")
			digest := sha256.Sum256(content)
			artifact := artifactRequest{
				Name:        "aggregate.json",
				BytesBase64: base64.StdEncoding.EncodeToString(content),
				SHA256:      hex.EncodeToString(digest[:]),
			}
			request := request{
				SchemaVersion: protocolVersion,
				Operation:     operation,
				ParentPath:    t.TempDir(),
				OutputName:    "published",
				StagingName:   ".stage-success",
				Artifacts:     []artifactRequest{artifact},
			}
			encoded, err := json.Marshal(request)
			if err != nil {
				t.Fatalf("encode request: %v", err)
			}
			var output, errorOutput bytes.Buffer
			if exitCode := run(nil, bytes.NewReader(encoded), &output, &errorOutput); exitCode != 0 {
				t.Fatalf("run exit code = %d, stderr=%s", exitCode, errorOutput.String())
			}
			var response response
			if err := json.Unmarshal(output.Bytes(), &response); err != nil {
				t.Fatalf("decode response: %v", err)
			}
			if response.Outcome != "completed" || response.FailureCode != nil ||
				len(response.Artifacts) != 1 || response.Artifacts[0].Name != artifact.Name ||
				response.Artifacts[0].BytesBase64 != artifact.BytesBase64 ||
				response.Artifacts[0].SHA256 != artifact.SHA256 {
				t.Fatalf("unexpected completed response: %#v", response)
			}
		})
	}
}

func TestRunPublishesExistingLargeFileWithoutSerializingItsBytes(t *testing.T) {
	t.Parallel()
	request := existingDirectoryHelperRequest(t, "prepare-existing-directory", true)
	prepareBytes := runHelperRequest(t, request)
	var prepared response
	if err := json.Unmarshal(prepareBytes, &prepared); err != nil || prepared.StagingReceipt == "" {
		t.Fatalf("decode prepared staging receipt: response=%q err=%v", prepareBytes, err)
	}
	stage := filepath.Join(request.ParentPath, request.StagingName)
	manifestBytes := []byte("{\"sealed\":true}\n")
	if err := os.WriteFile(filepath.Join(stage, "manifest.json"), manifestBytes, 0o600); err != nil {
		t.Fatalf("write manifest: %v", err)
	}
	video, err := os.OpenFile(filepath.Join(stage, "video.mp4"), os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open native-created video: %v", err)
	}
	if err := video.Sync(); err != nil {
		_ = video.Close()
		t.Fatalf("sync sparse video: %v", err)
	}
	if err := video.Close(); err != nil {
		t.Fatalf("close sparse video: %v", err)
	}
	request.Operation = "publish-existing-directory"
	request.SnapshotPaths = []string{"manifest.json"}
	request.StagingReceipt = prepared.StagingReceipt
	responseBytes := runHelperRequest(t, request)
	if len(responseBytes) > 4_096 {
		t.Fatalf("large attachment inflated helper response to %d bytes", len(responseBytes))
	}
	var result response
	if err := json.Unmarshal(responseBytes, &result); err != nil {
		t.Fatalf("decode existing-directory response: %v", err)
	}
	if result.ManifestSHA256 != request.ExpectedManifestSHA256 || len(result.Snapshots) != 1 ||
		result.Snapshots[0].BytesBase64 != base64.StdEncoding.EncodeToString(manifestBytes) {
		t.Fatalf("unexpected existing-directory response: %#v", result)
	}
}

func TestRunVerifiesPublishedDirectoryAndCleansPreparedStaging(t *testing.T) {
	t.Parallel()

	publishRequest := existingDirectoryHelperRequest(t, "prepare-existing-directory", false)
	var prepared response
	if err := json.Unmarshal(runHelperRequest(t, publishRequest), &prepared); err != nil || prepared.StagingReceipt == "" {
		t.Fatalf("decode publication preparation: response=%#v err=%v", prepared, err)
	}
	manifestBytes := []byte("{\"sealed\":true}\n")
	stagePath := filepath.Join(publishRequest.ParentPath, publishRequest.StagingName)
	if err := os.WriteFile(filepath.Join(stagePath, "manifest.json"), manifestBytes, 0o600); err != nil {
		t.Fatalf("write staged manifest: %v", err)
	}
	publishRequest.Operation = "publish-existing-directory"
	publishRequest.SnapshotPaths = []string{"manifest.json"}
	publishRequest.StagingReceipt = prepared.StagingReceipt
	runHelperRequest(t, publishRequest)

	publishRequest.Operation = "verify-existing-directory"
	publishRequest.StagingName = ""
	publishRequest.StagingReceipt = ""
	var verified response
	if err := json.Unmarshal(runHelperRequest(t, publishRequest), &verified); err != nil {
		t.Fatalf("decode verification response: %v", err)
	}
	if verified.ManifestSHA256 != publishRequest.ExpectedManifestSHA256 || len(verified.Snapshots) != 1 ||
		verified.Snapshots[0].RelativePath != "manifest.json" ||
		verified.Snapshots[0].BytesBase64 != base64.StdEncoding.EncodeToString(manifestBytes) {
		t.Fatalf("unexpected verification response: %#v", verified)
	}

	cleanupRequest := existingDirectoryHelperRequest(t, "prepare-existing-directory", false)
	prepared = response{}
	if err := json.Unmarshal(runHelperRequest(t, cleanupRequest), &prepared); err != nil || prepared.StagingReceipt == "" {
		t.Fatalf("decode cleanup preparation: response=%#v err=%v", prepared, err)
	}
	cleanupStagePath := filepath.Join(cleanupRequest.ParentPath, cleanupRequest.StagingName)
	cleanupRequest.Operation = "cleanup-existing-directory"
	cleanupRequest.StagingReceipt = prepared.StagingReceipt
	var cleaned response
	if err := json.Unmarshal(runHelperRequest(t, cleanupRequest), &cleaned); err != nil {
		t.Fatalf("decode cleanup response: %v", err)
	}
	if cleaned.CleanupOutcome != string(artifactpublish.ExistingDirectoryCleanupCompleted) {
		t.Fatalf("unexpected cleanup response: %#v", cleaned)
	}
	if _, err := os.Lstat(cleanupStagePath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("prepared staging survived authenticated cleanup: %v", err)
	}
}

func TestRunMapsUnsafePublicationAndInputReadFailure(t *testing.T) {
	t.Parallel()

	unsafeRequest := validFileRequest(t)
	unsafeRequest.Artifacts[0].SHA256 = strings.Repeat("0", sha256.Size*2)
	encoded, err := json.Marshal(unsafeRequest)
	if err != nil {
		t.Fatalf("encode unsafe request: %v", err)
	}
	var failure bytes.Buffer
	if exitCode := run(nil, bytes.NewReader(encoded), new(bytes.Buffer), &failure); exitCode != 2 {
		t.Fatalf("unsafe publication exit code = %d, want 2", exitCode)
	}
	assertFailureResponse(t, failure.Bytes(), "publication-unsafe")

	failure.Reset()
	if exitCode := run(nil, failingReader{}, new(bytes.Buffer), &failure); exitCode != 2 {
		t.Fatalf("input read failure exit code = %d, want 2", exitCode)
	}
	assertFailureResponse(t, failure.Bytes(), "protocol-invalid")
}

func TestRunMapsCollisionAndResponseWriteFailure(t *testing.T) {
	t.Parallel()
	request := validFileRequest(t)
	if err := os.WriteFile(filepath.Join(request.ParentPath, request.OutputName), []byte("foreign\n"), 0o600); err != nil {
		t.Fatalf("create collision: %v", err)
	}
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("encode collision request: %v", err)
	}
	var failure bytes.Buffer
	if exitCode := run(nil, bytes.NewReader(encoded), new(bytes.Buffer), &failure); exitCode != 3 {
		t.Fatalf("collision exit code = %d, want 3", exitCode)
	}
	assertFailureResponse(t, failure.Bytes(), "destination-exists")

	responseFailure := validFileRequest(t)
	encoded, err = json.Marshal(responseFailure)
	if err != nil {
		t.Fatalf("encode response failure request: %v", err)
	}
	failure.Reset()
	if exitCode := run(nil, bytes.NewReader(encoded), failingWriter{}, &failure); exitCode != 2 {
		t.Fatalf("response failure exit code = %d, want 2", exitCode)
	}
	assertFailureResponse(t, failure.Bytes(), "response-failed")
}

func TestWriteFailureUsesFrozenProtocol(t *testing.T) {
	t.Parallel()
	var output bytes.Buffer
	if exitCode := writeFailure(&output, "destination-exists", 3); exitCode != 3 {
		t.Fatalf("writeFailure exit code = %d, want 3", exitCode)
	}
	assertFailureResponse(t, output.Bytes(), "destination-exists")

	if exitCode := writeFailure(failingWriter{}, "response-failed", 9); exitCode != 2 {
		t.Fatalf("writeFailure writer error exit code = %d, want 2", exitCode)
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("injected writer failure")
}

type failingReader struct{}

func (failingReader) Read([]byte) (int, error) {
	return 0, errors.New("injected reader failure")
}

func validFileRequest(t *testing.T) request {
	t.Helper()
	content := []byte("file bytes\n")
	digest := sha256.Sum256(content)
	return request{
		SchemaVersion: protocolVersion,
		Operation:     "file",
		ParentPath:    t.TempDir(),
		OutputName:    "aggregate.json",
		StagingName:   ".stage-file",
		Artifacts: []artifactRequest{{
			Name:        "aggregate.json",
			BytesBase64: base64.StdEncoding.EncodeToString(content),
			SHA256:      hex.EncodeToString(digest[:]),
		}},
	}
}

func existingDirectoryHelperRequest(t *testing.T, operation string, includeLargeFile bool) request {
	t.Helper()
	manifest := []byte("{\"sealed\":true}\n")
	manifestDigest := sha256.Sum256(manifest)
	files := []existingDirectoryFileRequest{{
		RelativePath: "manifest.json",
		ByteLength:   strconv.Itoa(len(manifest)),
		SHA256:       hex.EncodeToString(manifestDigest[:]),
	}}
	if includeLargeFile {
		const videoBytes = 64 << 20
		files = append(files, existingDirectoryFileRequest{
			RelativePath: "video.mp4",
			ByteLength:   strconv.Itoa(videoBytes),
			SHA256:       zeroDigest(t, videoBytes),
		})
	}
	return request{
		SchemaVersion:          protocolVersion,
		Operation:              operation,
		ParentPath:             filepath.Join(t.TempDir(), "native-owned-publication-root"),
		OutputName:             artifactpublish.ExistingDirectoryOutputName,
		StagingName:            ".browser-evidence-upload-0123456789abcdef0123456789abcdef",
		Artifacts:              []artifactRequest{},
		Inventory:              &directoryInventoryRequest{Directories: []string{}, Files: files},
		ManifestPath:           "manifest.json",
		ExpectedManifestSHA256: hex.EncodeToString(manifestDigest[:]),
	}
}

func runHelperRequest(t *testing.T, request request) []byte {
	t.Helper()
	encoded, err := json.Marshal(request)
	if err != nil {
		t.Fatalf("encode helper request: %v", err)
	}
	var output, failure bytes.Buffer
	if exitCode := run(nil, bytes.NewReader(encoded), &output, &failure); exitCode != 0 {
		t.Fatalf("helper exit code = %d stderr=%s", exitCode, failure.String())
	}
	return output.Bytes()
}

func zeroDigest(t *testing.T, size int) string {
	t.Helper()
	digest := sha256.New()
	writeZeros(t, digest, size)
	return hex.EncodeToString(digest.Sum(nil))
}

func writeZeros(t *testing.T, destination hash.Hash, size int) {
	t.Helper()
	block := make([]byte, 64<<10)
	for written := 0; written < size; written += len(block) {
		current := len(block)
		if remaining := size - written; remaining < current {
			current = remaining
		}
		if _, err := destination.Write(block[:current]); err != nil {
			t.Fatalf("hash zero block: %v", err)
		}
	}
}

func assertFailureResponse(t *testing.T, encoded []byte, failureCode string) {
	t.Helper()
	var response response
	if err := json.Unmarshal(encoded, &response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.SchemaVersion != protocolVersion || response.Outcome != "failed" ||
		response.FailureCode == nil || *response.FailureCode != failureCode || len(response.Artifacts) != 0 {
		t.Fatalf("unexpected failure response: %#v", response)
	}
}
