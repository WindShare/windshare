package fault_test

import (
	"errors"
	"testing"

	"github.com/windshare/windshare/core/transfer/fault"
)

type faultCase struct {
	name       string
	domain     fault.Domain
	code       uint16
	codeString string
	build      func(fault.Scope) (fault.Fault, error)
}

func TestFaultValidatesEveryDomainScopeAndCode(t *testing.T) {
	t.Parallel()

	cases := []faultCase{
		sourceCase("source-unavailable", fault.SourceUnavailable, "unavailable"),
		sourceCase("source-revision-changed", fault.SourceRevisionChanged, "revision-changed"),
		sourceCase("source-revision-invalidated", fault.SourceRevisionInvalidated, "revision-invalidated"),
		sourceCase("source-permanent", fault.SourcePermanent, "permanent"),
		catalogCase("catalog-unavailable", fault.CatalogUnavailable, "unavailable"),
		catalogCase("catalog-directory-stale", fault.CatalogDirectoryStale, "directory-stale"),
		catalogCase("catalog-invalid-generation", fault.CatalogInvalidGeneration, "invalid-generation"),
		sessionCase("session-transport", fault.SessionTransport, "transport"),
		sessionCase("session-protocol", fault.SessionProtocol, "protocol"),
		sessionCase("session-resource-budget", fault.SessionResourceBudget, "resource-budget"),
		sessionCase("session-dependency-contract", fault.SessionDependencyContract, "dependency-contract"),
		outputCase("output-state-io", fault.OutputStateIO, "state-io"),
		outputCase("output-ownership", fault.OutputOwnership, "ownership"),
		outputCase("output-namespace-unsafe", fault.OutputNamespaceUnsafe, "namespace-unsafe"),
		outputCase("output-unsupported-filesystem", fault.OutputUnsupportedFilesystem, "unsupported-filesystem"),
		outputCase("output-directory-binding", fault.OutputDirectoryBinding, "directory-binding"),
		outputCase("output-directory-metadata", fault.OutputDirectoryMetadata, "directory-metadata"),
		outputCase("output-file-already-active", fault.OutputFileAlreadyActive, "file-already-active"),
		outputCase("output-resource-budget", fault.OutputResourceBudget, "resource-budget"),
		outputCase("output-mutation-ambiguous", fault.OutputMutationAmbiguous, "mutation-ambiguous"),
		outputCase("output-contract", fault.OutputContract, "contract"),
		checkpointCase("checkpoint-busy", fault.CheckpointBusy, "busy"),
		checkpointCase("checkpoint-corrupt-record", fault.CheckpointCorruptRecord, "corrupt-record"),
		checkpointCase("checkpoint-unsafe-install", fault.CheckpointUnsafeInstall, "unsafe-install"),
		checkpointCase("checkpoint-ownership-mismatch", fault.CheckpointOwnershipMismatch, "ownership-mismatch"),
		checkpointCase("checkpoint-state-io", fault.CheckpointStateIO, "state-io"),
	}
	scopes := []fault.Scope{
		fault.ScopeFileLocal,
		fault.ScopeDirectoryLocal,
		fault.ScopeOutputPause,
		fault.ScopeSessionTerminal,
	}

	for _, test := range cases {
		for _, scope := range scopes {
			t.Run(test.name+"/"+scope.String(), func(t *testing.T) {
				value, err := test.build(scope)
				if err != nil {
					t.Fatal(err)
				}
				if !value.Valid() || value.IsZero() || value.Domain() != test.domain ||
					value.Scope() != scope || value.Code() != test.code {
					t.Fatalf("fault = %#v, want domain=%s scope=%s code=%d", value, test.domain, scope, test.code)
				}
				wantString := test.domain.String() + "/" + scope.String() + "/" + test.codeString
				if value.String() != wantString {
					t.Fatalf("fault string = %q, want %q", value, wantString)
				}
			})
		}
	}

	if (fault.Fault{}).Valid() || !(fault.Fault{}).IsZero() || (fault.Fault{}).String() != "invalid" {
		t.Fatal("zero fault was accepted")
	}
	if _, err := fault.NewSource(0, fault.SourceUnavailable); !errors.Is(err, fault.ErrInvalidFault) {
		t.Fatalf("invalid scope error = %v", err)
	}
	if _, err := fault.NewSource(fault.ScopeFileLocal, 0); !errors.Is(err, fault.ErrInvalidFault) {
		t.Fatalf("invalid source code error = %v", err)
	}
	if _, err := fault.NewCatalog(fault.ScopeFileLocal, fault.CatalogInvalidGeneration+1); !errors.Is(err, fault.ErrInvalidFault) {
		t.Fatalf("invalid catalog code error = %v", err)
	}
	if _, err := fault.NewSession(fault.ScopeFileLocal, fault.SessionDependencyContract+1); !errors.Is(err, fault.ErrInvalidFault) {
		t.Fatalf("invalid session code error = %v", err)
	}
	if _, err := fault.NewOutput(fault.ScopeFileLocal, fault.OutputContract+1); !errors.Is(err, fault.ErrInvalidFault) {
		t.Fatalf("invalid output code error = %v", err)
	}
	if _, err := fault.NewCheckpoint(fault.ScopeFileLocal, fault.CheckpointStateIO+1); !errors.Is(err, fault.ErrInvalidFault) {
		t.Fatalf("invalid checkpoint code error = %v", err)
	}
	if fault.Domain(0).String() != "invalid" || fault.Scope(0).String() != "invalid" ||
		fault.SourceCode(0).String() != "invalid" || fault.CatalogCode(0).String() != "invalid" ||
		fault.SessionCode(0).String() != "invalid" || fault.OutputCode(0).String() != "invalid" ||
		fault.CheckpointCode(0).String() != "invalid" {
		t.Fatal("an unknown enum acquired a display value")
	}
}

func TestFaultDomainSpecificCodeAccessorsRejectOtherDomains(t *testing.T) {
	t.Parallel()

	value := mustOutput(t, fault.ScopeDirectoryLocal, fault.OutputDirectoryMetadata)
	if code, ok := value.OutputCode(); !ok || code != fault.OutputDirectoryMetadata {
		t.Fatalf("output code = (%v, %v)", code, ok)
	}
	if _, ok := value.SourceCode(); ok {
		t.Fatal("output fault exposed a source code")
	}
	if _, ok := value.CatalogCode(); ok {
		t.Fatal("output fault exposed a catalog code")
	}
	if _, ok := value.SessionCode(); ok {
		t.Fatal("output fault exposed a session code")
	}
	if _, ok := value.CheckpointCode(); ok {
		t.Fatal("output fault exposed a checkpoint code")
	}
}

func TestJoinIsPermutationStableAndUsesSeverityThenDomainCode(t *testing.T) {
	t.Parallel()

	file := mustOutput(t, fault.ScopeFileLocal, fault.OutputContract)
	directory := mustCatalog(t, fault.ScopeDirectoryLocal, fault.CatalogInvalidGeneration)
	output := mustCheckpoint(t, fault.ScopeOutputPause, fault.CheckpointStateIO)
	terminal := mustSource(t, fault.ScopeSessionTerminal, fault.SourceUnavailable)
	values := []fault.Fault{file, directory, output, terminal}
	for _, permutation := range faultPermutations(values) {
		if joined := fault.Join(permutation...); joined != terminal {
			t.Fatalf("join(%v) = %v, want %v", permutation, joined, terminal)
		}
	}

	sourceTie := mustSource(t, fault.ScopeOutputPause, fault.SourcePermanent)
	checkpointTie := mustCheckpoint(t, fault.ScopeOutputPause, fault.CheckpointBusy)
	if joined := fault.Join(sourceTie, checkpointTie); joined != checkpointTie {
		t.Fatalf("domain tie = %v, want %v", joined, checkpointTie)
	}
	lowCode := mustCheckpoint(t, fault.ScopeOutputPause, fault.CheckpointBusy)
	highCode := mustCheckpoint(t, fault.ScopeOutputPause, fault.CheckpointStateIO)
	if joined := fault.Join(highCode, lowCode); joined != highCode {
		t.Fatalf("code tie = %v, want %v", joined, highCode)
	}
	if fault.Compare(highCode, lowCode) <= 0 {
		t.Fatal("higher code did not win a same-domain severity tie")
	}
	if fault.Join(fault.Fault{}, file) != file || fault.Join() != (fault.Fault{}) || fault.Join(file, file) != file {
		t.Fatal("join did not preserve zero identity and idempotence")
	}
	if fault.Join(fault.Join(file, output), terminal) != fault.Join(file, fault.Join(output, terminal)) {
		t.Fatal("join was not associative")
	}
	if fault.Compare(file, output) >= 0 || fault.Compare(output, file) <= 0 || fault.Compare(file, file) != 0 {
		t.Fatal("compare did not match the join order")
	}
}

func sourceCase(name string, code fault.SourceCode, codeString string) faultCase {
	return faultCase{
		name: name, domain: fault.DomainSource, code: uint16(code), codeString: codeString,
		build: func(scope fault.Scope) (fault.Fault, error) { return fault.NewSource(scope, code) },
	}
}

func catalogCase(name string, code fault.CatalogCode, codeString string) faultCase {
	return faultCase{
		name: name, domain: fault.DomainCatalog, code: uint16(code), codeString: codeString,
		build: func(scope fault.Scope) (fault.Fault, error) { return fault.NewCatalog(scope, code) },
	}
}

func sessionCase(name string, code fault.SessionCode, codeString string) faultCase {
	return faultCase{
		name: name, domain: fault.DomainSession, code: uint16(code), codeString: codeString,
		build: func(scope fault.Scope) (fault.Fault, error) { return fault.NewSession(scope, code) },
	}
}

func outputCase(name string, code fault.OutputCode, codeString string) faultCase {
	return faultCase{
		name: name, domain: fault.DomainOutput, code: uint16(code), codeString: codeString,
		build: func(scope fault.Scope) (fault.Fault, error) { return fault.NewOutput(scope, code) },
	}
}

func checkpointCase(name string, code fault.CheckpointCode, codeString string) faultCase {
	return faultCase{
		name: name, domain: fault.DomainCheckpoint, code: uint16(code), codeString: codeString,
		build: func(scope fault.Scope) (fault.Fault, error) { return fault.NewCheckpoint(scope, code) },
	}
}

func mustFault(t *testing.T, value fault.Fault, err error) fault.Fault {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func mustSource(t *testing.T, scope fault.Scope, code fault.SourceCode) fault.Fault {
	t.Helper()
	value, err := fault.NewSource(scope, code)
	return mustFault(t, value, err)
}

func mustCatalog(t *testing.T, scope fault.Scope, code fault.CatalogCode) fault.Fault {
	t.Helper()
	value, err := fault.NewCatalog(scope, code)
	return mustFault(t, value, err)
}

func mustOutput(t *testing.T, scope fault.Scope, code fault.OutputCode) fault.Fault {
	t.Helper()
	value, err := fault.NewOutput(scope, code)
	return mustFault(t, value, err)
}

func mustCheckpoint(t *testing.T, scope fault.Scope, code fault.CheckpointCode) fault.Fault {
	t.Helper()
	value, err := fault.NewCheckpoint(scope, code)
	return mustFault(t, value, err)
}

func faultPermutations(values []fault.Fault) [][]fault.Fault {
	if len(values) == 0 {
		return [][]fault.Fault{{}}
	}
	result := make([][]fault.Fault, 0)
	for index, value := range values {
		remainder := append([]fault.Fault(nil), values[:index]...)
		remainder = append(remainder, values[index+1:]...)
		for _, suffix := range faultPermutations(remainder) {
			result = append(result, append([]fault.Fault{value}, suffix...))
		}
	}
	return result
}
