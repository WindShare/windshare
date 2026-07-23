package osfs

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"math"
	"os"
	"reflect"
	"testing"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/content"
	"github.com/windshare/windshare/core/transfer"
)

func outputJournalStage(seed byte) string {
	raw := make([]byte, outputStageRandomBytes)
	raw[0] = seed
	return outputStagePrefix + encodeOutputFilenameToken(raw)
}

func outputJournalFixture(t *testing.T) (outputJournalDocument, outputJournalFile) {
	t.Helper()
	config := outputTestConfig(t.TempDir())
	root, err := os.OpenRoot(config.RootPath)
	if err != nil {
		t.Fatal(err)
	}
	rootBinding, err := bindOutputRoot(config.RootPath, root)
	if closeErr := root.Close(); err != nil || closeErr != nil {
		t.Fatal(errors.Join(err, closeErr))
	}
	descriptor := outputTestDescriptor(t, config, 1, 2, 64, catalog.ModifiedTime{})
	locator, err := transfer.NewPathOutputLocator("file.bin")
	if err != nil {
		t.Fatal(err)
	}
	var identity transfer.OutputObjectIdentity
	identity[0] = 1
	binding, err := transfer.NewOutputFileBinding(filesystemOutputBackendID, config.SessionID, descriptor, locator, identity)
	if err != nil {
		t.Fatal(err)
	}
	ranges, _ := content.NewRangeSet([]content.Range{{Offset: 0, End: 32}})
	file := journalFileFromBinding(binding, outputJournalStage(1), 1, ranges, false)
	document := newOutputJournal(filesystemOutputBackendID, config.SessionID, config.ShareInstance, config.ResumeIntent, rootBinding)
	document.put(file)
	return document, file
}

func TestOutputJournalRejectsEveryBindingAndRangeMutation(t *testing.T) {
	document, file := outputJournalFixture(t)
	zero16 := encodeOutputBytes(make([]byte, 16))
	zero32 := encodeOutputBytes(make([]byte, 32))
	tests := []struct {
		name   string
		mutate func(*outputJournalDocument)
	}{
		{name: "schema", mutate: func(d *outputJournalDocument) { d.Schema++ }},
		{name: "nil files", mutate: func(d *outputJournalDocument) { d.Files = nil }},
		{name: "backend", mutate: func(d *outputJournalDocument) { d.Backend = "" }},
		{name: "session length", mutate: func(d *outputJournalDocument) { d.OutputSession = "AA" }},
		{name: "session zero", mutate: func(d *outputJournalDocument) { d.OutputSession = zero16 }},
		{name: "share length", mutate: func(d *outputJournalDocument) { d.ShareInstance = "AA" }},
		{name: "share zero", mutate: func(d *outputJournalDocument) { d.ShareInstance = zero16 }},
		{name: "intent length", mutate: func(d *outputJournalDocument) { d.ResumeIntent = "AA" }},
		{name: "intent zero", mutate: func(d *outputJournalDocument) { d.ResumeIntent = zero32 }},
		{name: "root locator", mutate: func(d *outputJournalDocument) { d.RootLocator = zero32 }},
		{name: "root identity", mutate: func(d *outputJournalDocument) { d.RootIdentity = zero32 }},
		{name: "path", mutate: func(d *outputJournalDocument) { d.Files[0].Path = "../escape" }},
		{name: "stage", mutate: func(d *outputJournalDocument) { d.Files[0].Stage = "user-file" }},
		{name: "stage suffix", mutate: func(d *outputJournalDocument) { d.Files[0].Stage = outputStagePrefix + "bad:stream" }},
		{name: "file length", mutate: func(d *outputJournalDocument) { d.Files[0].FileID = "AA" }},
		{name: "file zero", mutate: func(d *outputJournalDocument) { d.Files[0].FileID = zero16 }},
		{name: "revision length", mutate: func(d *outputJournalDocument) { d.Files[0].Revision = "AA" }},
		{name: "revision zero", mutate: func(d *outputJournalDocument) { d.Files[0].Revision = zero16 }},
		{name: "size", mutate: func(d *outputJournalDocument) { d.Files[0].ExactSize = catalog.MaxFileSize + 1 }},
		{name: "locator length", mutate: func(d *outputJournalDocument) { d.Files[0].LocatorDigest = "AA" }},
		{name: "locator zero", mutate: func(d *outputJournalDocument) { d.Files[0].LocatorDigest = zero32 }},
		{name: "object length", mutate: func(d *outputJournalDocument) { d.Files[0].ObjectIdentity = "AA" }},
		{name: "object zero", mutate: func(d *outputJournalDocument) { d.Files[0].ObjectIdentity = zero32 }},
		{name: "range empty", mutate: func(d *outputJournalDocument) { d.Files[0].Ranges = [][2]uint64{{1, 1}} }},
		{name: "nil ranges", mutate: func(d *outputJournalDocument) { d.Files[0].Ranges = nil }},
		{name: "range adjacent", mutate: func(d *outputJournalDocument) { d.Files[0].Ranges = [][2]uint64{{0, 1}, {1, 2}} }},
		{name: "range outside", mutate: func(d *outputJournalDocument) { d.Files[0].Ranges = [][2]uint64{{0, 65}} }},
		{name: "generation zero with ranges", mutate: func(d *outputJournalDocument) { d.Files[0].Generation = 0 }},
		{name: "generation rollback ceiling", mutate: func(d *outputJournalDocument) { d.Files[0].Generation = math.MaxUint64 }},
		{name: "positive generation without progress", mutate: func(d *outputJournalDocument) { d.Files[0].Ranges = [][2]uint64{} }},
		{name: "published incomplete", mutate: func(d *outputJournalDocument) { d.Files[0].Published = true }},
		{name: "published generation zero", mutate: func(d *outputJournalDocument) {
			d.Files[0].Generation = 0
			d.Files[0].Ranges = [][2]uint64{{0, 64}}
			d.Files[0].Published = true
		}},
		{name: "unsorted paths", mutate: func(d *outputJournalDocument) {
			second := d.Files[0]
			second.Path = "a.bin"
			second.Stage = outputJournalStage(2)
			d.Files = append(d.Files, second)
		}},
		{name: "duplicate stage", mutate: func(d *outputJournalDocument) {
			second := d.Files[0]
			second.Path = "z.bin"
			d.Files = append(d.Files, second)
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			candidate := document.clone()
			test.mutate(&candidate)
			if err := validateOutputJournal(candidate); !errors.Is(err, ErrOutputJournalCorrupt) {
				t.Fatalf("validation error=%v candidate=%+v", err, candidate)
			}
		})
	}
	if got, found := document.file(file.Path); !found || !reflect.DeepEqual(*got, file) {
		t.Fatalf("valid file lookup=(%+v,%v)", got, found)
	}
	if _, found := document.file("missing.bin"); found || document.remove("missing.bin") {
		t.Fatal("missing journal file reported present")
	}
}

func TestOutputJournalCodecIsCanonicalBoundedAndChecksummed(t *testing.T) {
	document, _ := outputJournalFixture(t)
	encoded, err := encodeOutputJournal(document)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := decodeOutputJournal(encoded)
	if err != nil || !reflect.DeepEqual(decoded.Files, document.Files) {
		t.Fatalf("decoded=%+v err=%v", decoded, err)
	}
	for name, hostile := range map[string][]byte{
		"short":    []byte("short"),
		"magic":    append([]byte(nil), encoded...),
		"checksum": append([]byte(nil), encoded...),
	} {
		switch name {
		case "magic":
			hostile[0] ^= 0xff
		case "checksum":
			hostile[len(hostile)-1] ^= 0xff
		}
		if _, err := decodeOutputJournal(hostile); !errors.Is(err, ErrOutputJournalCorrupt) {
			t.Fatalf("%s error=%v", name, err)
		}
	}
	payload, err := json.MarshalIndent(document, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := decodeOutputJournal(outputJournalPayload(payload)); !errors.Is(err, ErrOutputJournalCorrupt) {
		t.Fatalf("noncanonical JSON error=%v", err)
	}
	canonical, _ := json.Marshal(document)
	unknown := append(append([]byte(nil), canonical[:len(canonical)-1]...), []byte(`,"unknown":1}`)...)
	if _, err := decodeOutputJournal(outputJournalPayload(unknown)); !errors.Is(err, ErrOutputJournalCorrupt) {
		t.Fatalf("unknown field error=%v", err)
	}
	invalid := document.clone()
	invalid.Schema++
	if _, err := encodeOutputJournal(invalid); !errors.Is(err, ErrOutputJournalCorrupt) {
		t.Fatalf("invalid encode error=%v", err)
	}
}

func outputJournalPayload(payload []byte) []byte {
	encoded := append(append([]byte(nil), outputJournalMagic[:]...), payload...)
	checksum := sha256.Sum256(encoded)
	return append(encoded, checksum[:]...)
}
