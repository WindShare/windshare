package outputformat

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestGoArchiveZipConsumesDirectZipOwnershipMarkerFixture(t *testing.T) {
	fixtureDirectory := filepath.Join("..", "..", "testdata", "direct-zip", "v1")
	manifestBytes, err := os.ReadFile(filepath.Join(fixtureDirectory, "ownership-marker-v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var manifest struct {
		Schema        string `json:"schema"`
		Archive       string `json:"archive"`
		ArchiveBytes  int    `json:"archiveBytes"`
		ArchiveSHA256 string `json:"archiveSha256"`
		RootMember    string `json:"rootMember"`
	}
	if err := json.Unmarshal(manifestBytes, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Schema != "windshare/direct-zip-marker-fixture/v1" ||
		manifest.Archive != "ownership-marker-v1.zip" {
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
}
