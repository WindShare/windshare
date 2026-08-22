package protocolcontract

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

const (
	directZipOwnershipFormatDomain = "windshare/direct-zip-ownership-extra/v1"
	zipEncodingPolicyV2Domain      = "windshare/zip-encoding/v2-store-data-descriptor-owned-marker"
	directZipLayoutPolicyV2Domain  = "windshare/zip-layout/v2-paged-owned-marker"
	directZipOwnershipHeaderID     = 0x5357
	directZipOwnershipDataBytes    = 116
	directZipOwnershipFieldBytes   = 120
	directZipMaximumPositionedByte = uint64(9_007_199_254_740_991)
)

func TestDirectZipFormatPolicyDigestsMatchTypeScriptContract(t *testing.T) {
	ownership := policyRecord(directZipOwnershipFormatDomain,
		policyFrame(uint16BigEndian(directZipOwnershipHeaderID)),
		policyFrame([]byte("WindShareZipOwn\x00")),
		policyFrame(uint16BigEndian(1)),
		policyFrame(uint16BigEndian(0)),
		policyFrame(uint16BigEndian(32)),
		policyFrame(uint16BigEndian(directZipOwnershipDataBytes)),
	)
	ownershipDigest := sha256.Sum256(ownership)
	assertDigestHex(t, ownershipDigest,
		"86715d2bfc5e0de6089ecca1bf98b861d539ed451c59e0065182d36a459a650c")

	encoding := policyRecord(zipEncodingPolicyV2Domain, policyFrame(ownershipDigest[:]))
	encodingDigest := sha256.Sum256(encoding)
	assertDigestHex(t, encodingDigest,
		"2d6363da388be94ded4d935a2f2e6dc631650d26a853386b8d3d09e38af476b7")

	layout := policyRecord(directZipLayoutPolicyV2Domain,
		policyFrame(encodingDigest[:]), policyFrame(uint64BigEndian(directZipMaximumPositionedByte)))
	layoutDigest := sha256.Sum256(layout)
	assertDigestHex(t, layoutDigest,
		"55257e0f54f0873871b82658831d7e644136ea816efa61d7038a1610b3a0187e")
}

func TestGoArchiveZipConsumesDirectZipOwnershipMarkerFixture(t *testing.T) {
	fixtureDirectory := filepath.Join("..", "..", "..", "testdata", "direct-zip", "v1")
	manifestBytes, err := os.ReadFile(filepath.Join(fixtureDirectory, "ownership-marker-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Schema            string `json:"schema"`
		Archive           string `json:"archive"`
		ArchiveBytes      int    `json:"archiveBytes"`
		ArchiveSHA256     string `json:"archiveSha256"`
		RootMember        string `json:"rootMember"`
		OperationIDHex    string `json:"operationIdHex"`
		CandidateIDHex    string `json:"candidateIdHex"`
		OwnershipNonceHex string `json:"ownershipNonceHex"`
		BindingDigestHex  string `json:"bindingDigestHex"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Schema != "windshare/direct-zip-marker-fixture/v1" ||
		manifest.Archive != "ownership-marker-v1.zip" || manifest.ArchiveBytes != 390 ||
		manifest.ArchiveSHA256 != "6fb91a264510bbd4edde826a8f0b0c390df32e1152c16fd46907491e98cdbb9a" {
		t.Fatalf("unexpected direct ZIP fixture manifest: %+v", manifest)
	}

	archiveBytes, err := os.ReadFile(filepath.Join(fixtureDirectory, manifest.Archive))
	if err != nil {
		t.Fatal(err)
	}
	archiveDigest := sha256.Sum256(archiveBytes)
	if len(archiveBytes) != manifest.ArchiveBytes || hex.EncodeToString(archiveDigest[:]) != manifest.ArchiveSHA256 {
		t.Fatal("direct ZIP fixture bytes do not match the committed manifest")
	}
	reader, err := zip.NewReader(bytes.NewReader(archiveBytes), int64(len(archiveBytes)))
	if err != nil {
		t.Fatal(err)
	}
	if len(reader.File) != 1 || reader.File[0].Name != manifest.RootMember || !reader.File[0].FileInfo().IsDir() {
		t.Fatalf("archive/zip projected unexpected fixture members: %+v", reader.File)
	}
	member, err := reader.File[0].Open()
	if err != nil {
		t.Fatal(err)
	}
	payload, readErr := io.ReadAll(member)
	closeErr := member.Close()
	if readErr != nil || closeErr != nil || len(payload) != 0 {
		t.Fatalf("fixture directory payload len=%d read=%v close=%v", len(payload), readErr, closeErr)
	}
	assertOwnershipExtraField(t, reader.File[0].Extra, manifest)
}

func assertOwnershipExtraField(t *testing.T, extra []byte, manifest struct {
	Schema            string `json:"schema"`
	Archive           string `json:"archive"`
	ArchiveBytes      int    `json:"archiveBytes"`
	ArchiveSHA256     string `json:"archiveSha256"`
	RootMember        string `json:"rootMember"`
	OperationIDHex    string `json:"operationIdHex"`
	CandidateIDHex    string `json:"candidateIdHex"`
	OwnershipNonceHex string `json:"ownershipNonceHex"`
	BindingDigestHex  string `json:"bindingDigestHex"`
}) {
	t.Helper()
	if len(extra) != directZipOwnershipFieldBytes ||
		binary.LittleEndian.Uint16(extra[0:2]) != directZipOwnershipHeaderID ||
		binary.LittleEndian.Uint16(extra[2:4]) != directZipOwnershipDataBytes {
		t.Fatalf("unexpected central ownership extra field header: %x", extra)
	}
	data := extra[4:]
	if !bytes.Equal(data[:16], []byte("WindShareZipOwn\x00")) ||
		binary.LittleEndian.Uint16(data[16:18]) != 1 ||
		binary.LittleEndian.Uint16(data[18:20]) != 0 {
		t.Fatalf("unexpected ownership marker selector: %x", data[:20])
	}
	expected := mustDecodeHex(t, manifest.OperationIDHex+manifest.CandidateIDHex+
		manifest.OwnershipNonceHex+manifest.BindingDigestHex)
	if !bytes.Equal(data[20:], expected) {
		t.Fatalf("ownership marker authority bytes=%x want=%x", data[20:], expected)
	}
}

func policyRecord(domain string, fields ...[]byte) []byte {
	record := append(append([]byte(domain), 0), 1)
	for _, field := range fields {
		record = append(record, field...)
	}
	return record
}

func policyFrame(value []byte) []byte {
	return append(uint64BigEndian(uint64(len(value))), value...)
}

func uint16BigEndian(value uint16) []byte {
	encoded := make([]byte, 2)
	binary.BigEndian.PutUint16(encoded, value)
	return encoded
}

func uint64BigEndian(value uint64) []byte {
	encoded := make([]byte, 8)
	binary.BigEndian.PutUint64(encoded, value)
	return encoded
}

func assertDigestHex(t *testing.T, digest [sha256.Size]byte, expected string) {
	t.Helper()
	if actual := hex.EncodeToString(digest[:]); actual != expected {
		t.Fatalf("policy digest=%s want=%s", actual, expected)
	}
}

func mustDecodeHex(t *testing.T, value string) []byte {
	t.Helper()
	decoded, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}
