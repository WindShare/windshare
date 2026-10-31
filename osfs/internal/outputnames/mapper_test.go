package outputnames

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/windshare/windshare/core/catalog"
	"github.com/windshare/windshare/core/transfer/receivecontract"
	"github.com/windshare/windshare/osfs/internal/outputcap"
)

type memoryStore struct {
	records  []Mapping
	writeErr error
}

func (store *memoryStore) Load() ([]Mapping, error) { return store.records, nil }
func (store *memoryStore) Put(record Mapping) error {
	if store.writeErr != nil {
		return store.writeErr
	}
	store.records = append(store.records, record)
	return nil
}

type namespaceFunc func(string, string) (outputcap.EntryKind, error)

func (function namespaceFunc) InspectName(parent, name string) (outputcap.EntryKind, error) {
	return function(parent, name)
}

var testOperation = []byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}
var testRoot = Root{RequestedName: "docs", ReservedName: "docs", PhysicalName: "docs", Kind: receivecontract.ContainerEntryResultRoot}

func testMapper(t *testing.T, store Store, namespace Namespace) *Mapper {
	t.Helper()
	mapper, err := New(testOperation, testRoot, store, namespace, nil)
	if err != nil {
		t.Fatal(err)
	}
	return mapper
}

func TestNormalNamesNeedNoNamespaceProbeOrPersistence(t *testing.T) {
	store := &memoryStore{}
	mapper := testMapper(t, store, namespaceFunc(func(_, _ string) (outputcap.EntryKind, error) {
		t.Fatal("ordinary admission probed the destination")
		return 0, nil
	}))
	for index := range 100 {
		name := fmt.Sprintf("report-%d.txt", index)
		if physical, err := mapper.Bind("docs/" + name); err != nil || physical != name {
			t.Fatalf("%q: %v", physical, err)
		}
	}
	if len(store.records) != 0 {
		t.Fatal("ordinary admission wrote name metadata")
	}
}

func TestSupportedSourceSpellingsStayUnchanged(t *testing.T) {
	store := &memoryStore{}
	mapper := testMapper(t, store, namespaceFunc(func(_, _ string) (outputcap.EntryKind, error) { return outputcap.EntryAbsent, nil }))
	for _, name := range []string{"question?.txt", "CON .txt", "LPT¹.log", "trailing."} {
		if physical, err := mapper.Bind("docs/" + name); err != nil || physical != name {
			t.Fatalf("%q: %v", physical, err)
		}
	}
	if len(store.records) != 0 {
		t.Fatal("supported spellings were recorded as repairs")
	}
}

func TestRepairIsDurableAndNestedResumeKeepsTheChosenNames(t *testing.T) {
	store := &memoryStore{}
	reject := namespaceFunc(func(_, name string) (outputcap.EntryKind, error) {
		if !PortableName(name) {
			return 0, outputcap.ErrNameRejected
		}
		return outputcap.EntryAbsent, nil
	})
	mapper := testMapper(t, store, reject)
	parent, err := mapper.Bind("docs/folder?")
	if err != nil {
		t.Fatal(err)
	}
	file, err := mapper.Bind("docs/folder?/report?.pdf")
	if err != nil || !strings.HasPrefix(file, parent+"/") || !strings.HasSuffix(file, ".pdf") {
		t.Fatalf("nested repair = %q, %v", file, err)
	}
	if len(store.records) != 2 {
		t.Fatal("choices did not reach the store before binding returned")
	}
	reopened := testMapper(t, store, namespaceFunc(func(_, _ string) (outputcap.EntryKind, error) {
		t.Fatal("recovery re-probed a frozen name")
		return 0, nil
	}))
	if restored, err := reopened.Bind("docs/folder?/report?.pdf"); err != nil || restored != file {
		t.Fatalf("resume = %q, %v", restored, err)
	}
	if restored, err := reopened.Resolve("docs/folder?/ordinary.txt"); err != nil || restored != parent+"/ordinary.txt" {
		t.Fatalf("ordinary descendant = %q, %v", restored, err)
	}
}

func TestRepairSkipsForeignEntriesAndResolvesLaterSourceCollisions(t *testing.T) {
	first, _ := Candidate(testOperation, "docs/question?.txt", 0)
	store := &memoryStore{}
	mapper := testMapper(t, store, namespaceFunc(func(_, name string) (outputcap.EntryKind, error) {
		if name == "question?.txt" {
			return 0, outputcap.ErrNameRejected
		}
		if name == first {
			return outputcap.EntryRegularFile, nil
		}
		return outputcap.EntryAbsent, nil
	}))
	repaired, err := mapper.Bind("docs/question?.txt")
	if err != nil || repaired == first || store.records[0].Attempt != 1 {
		t.Fatalf("repair = %q, %v", repaired, err)
	}
	// This valid source spelling was not known when the first file was bound.
	later, err := mapper.Bind("docs/" + repaired)
	if err != nil || later == repaired {
		t.Fatalf("later source collision = %q, %v", later, err)
	}
	reopened := testMapper(t, store, namespaceFunc(func(_, _ string) (outputcap.EntryKind, error) { return 0, errors.New("unreachable") }))
	if restored, err := reopened.Bind("docs/" + repaired); err != nil || restored != later {
		t.Fatalf("later resume = %q, %v", restored, err)
	}
}

func TestOnlyNameRefusalAllowsRepairAndFailedPersistenceGrantsNoPath(t *testing.T) {
	ioFailure := errors.New("destination I/O failed")
	for _, failure := range []error{ioFailure, outputcap.ErrUnsafeNamespace} {
		store := &memoryStore{}
		mapper := testMapper(t, store, namespaceFunc(func(_, _ string) (outputcap.EntryKind, error) { return 0, failure }))
		if path, err := mapper.Bind("docs/question?.txt"); path != "" || !errors.Is(err, failure) || len(store.records) != 0 {
			t.Fatalf("failure became repair: %q %v", path, err)
		}
	}
	store := &memoryStore{writeErr: ioFailure}
	mapper := testMapper(t, store, namespaceFunc(func(_, name string) (outputcap.EntryKind, error) {
		if name == "question?.txt" {
			return 0, outputcap.ErrNameRejected
		}
		return outputcap.EntryAbsent, nil
	}))
	if path, err := mapper.Bind("docs/question?.txt"); path != "" || !errors.Is(err, ioFailure) || len(mapper.Mappings()) != 0 {
		t.Fatalf("unpersisted path = %q, %v", path, err)
	}
	store.writeErr = nil
	if path, err := mapper.Bind("docs/question?.txt"); err != nil || path == "" {
		t.Fatalf("retry = %q, %v", path, err)
	}
}

func TestMappingRecoveryRejectsAnotherOperationOrInventedName(t *testing.T) {
	component, _ := Candidate(testOperation, "docs/question?.txt", 0)
	record := Mapping{LogicalPath: "docs/question?.txt", PhysicalComponent: component}
	for _, mutate := range []func(*Mapping){
		func(record *Mapping) { record.PhysicalComponent = "other.txt" },
		func(record *Mapping) { record.LogicalPath = "../outside" },
		func(record *Mapping) { record.Attempt++ },
	} {
		corrupt := record
		mutate(&corrupt)
		if _, err := New(testOperation, testRoot, &memoryStore{records: []Mapping{corrupt}}, namespaceFunc(nil), nil); !errors.Is(err, ErrInvalidMapping) {
			t.Fatalf("corrupt mapping accepted: %v", err)
		}
	}
	otherOperation := append([]byte(nil), testOperation...)
	otherOperation[0]++
	if _, err := New(otherOperation, testRoot, &memoryStore{records: []Mapping{record}}, namespaceFunc(nil), nil); !errors.Is(err, ErrInvalidMapping) {
		t.Fatalf("foreign mapping accepted: %v", err)
	}
}

func TestGeneratedNamesPreserveUsableExtensionsAndStayPortable(t *testing.T) {
	for _, name := range []string{"report?.pdf", "CON .txt", "NUL", "LPT¹.txt", "trail. ", strings.Repeat("界", 80) + "?.txt", "???"} {
		candidate, err := Candidate(testOperation, "docs/"+name, 0)
		if err != nil || !PortableName(candidate) || len(candidate) > catalog.MaxNameBytes || !utf8.ValidString(candidate) {
			t.Fatalf("%q -> %q, %v", name, candidate, err)
		}
		repeated, _ := Candidate(testOperation, "docs/"+name, 0)
		next, _ := Candidate(testOperation, "docs/"+name, 1)
		if repeated != candidate || next == candidate {
			t.Fatal("name choice is unstable or cannot retry")
		}
		if strings.HasSuffix(name, ".pdf") && !strings.HasSuffix(candidate, ".pdf") || strings.HasSuffix(name, ".txt") && !strings.HasSuffix(candidate, ".txt") {
			t.Fatalf("extension lost: %q", candidate)
		}
	}
}

func TestRepairTraceDistinguishesChoiceFromReuse(t *testing.T) {
	store := &memoryStore{}
	var events []Trace
	mapper, err := New(testOperation, testRoot, store, namespaceFunc(func(_, name string) (outputcap.EntryKind, error) {
		if strings.Contains(name, "?") {
			return 0, outputcap.ErrNameRejected
		}
		return outputcap.EntryAbsent, nil
	}), func(event Trace) { events = append(events, event) })
	if err != nil {
		t.Fatal(err)
	}
	physical, err := mapper.Bind("docs/report?.pdf")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mapper.Bind("docs/report?.pdf"); err != nil {
		t.Fatal(err)
	}
	want := []Trace{{LogicalPath: "docs/report?.pdf", PhysicalPath: physical}, {LogicalPath: "docs/report?.pdf", PhysicalPath: physical, Reused: true}}
	if !reflect.DeepEqual(events, want) {
		t.Fatalf("traces = %+v", events)
	}
}
