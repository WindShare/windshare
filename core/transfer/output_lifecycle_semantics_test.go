package transfer

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/windshare/windshare/core/content"
)

func TestOutputFaultClassificationFailsClosed(t *testing.T) {
	for _, invalid := range []struct {
		scope OutputFaultScope
		code  OutputFaultCode
	}{
		{scope: 0, code: OutputFaultStateIO},
		{scope: OutputFaultFile, code: 0},
		{scope: OutputFaultRoot + 1, code: OutputFaultStateIO},
		{scope: OutputFaultFile, code: OutputFaultContract + 1},
	} {
		if err := NewOutputFault(invalid.scope, invalid.code, nil); !errors.Is(err, ErrInvalidOutputSettlement) {
			t.Fatalf("invalid fault (%d, %d) error = %v", invalid.scope, invalid.code, err)
		}
	}

	fileFailure := NewOutputFault(OutputFaultFile, OutputFaultStateIO, nil)
	var fileFault *OutputFault
	if !errors.As(fileFailure, &fileFault) || fileFault.RequiresJobPause() || errors.Unwrap(fileFault) == nil {
		t.Fatalf("file fault = %#v, unwrap = %v", fileFault, errors.Unwrap(fileFailure))
	}
	if message := fileFault.Error(); !strings.Contains(message, "scope=1 code=1") ||
		!strings.Contains(message, "output settlement failed") {
		t.Fatalf("file fault diagnostic = %q", message)
	}

	sessionFailure := NewOutputFault(OutputFaultSession, OutputFaultContract, errors.New("contract cause"))
	var sessionFault *OutputFault
	if !errors.As(sessionFailure, &sessionFault) || !sessionFault.RequiresJobPause() {
		t.Fatalf("session fault = %#v", sessionFault)
	}
}

func TestSessionNamespaceFaultPausePolicyDistinguishesFreshFromAdmittedState(t *testing.T) {
	fresh := NewOutputFault(
		OutputFaultSession,
		OutputFaultNamespaceUnsafe,
		errors.New("selected output ancestry is unsafe"),
	)
	isolating := OutputCapabilities{FileFailureIsolation: true}
	if outputFailureExplicitlyRequiresJobPause(fresh) ||
		outputFailureRequiresJobPause(fresh, isolating) {
		t.Fatal("fresh session namespace rejection required a job pause")
	}
	if !outputFailureRequiresJobPause(fresh, OutputCapabilities{}) {
		t.Fatal("non-isolating output ignored its capability-level pause policy")
	}

	admitted := NewOutputSessionError(fresh, true)
	if !outputFailureExplicitlyRequiresJobPause(admitted) ||
		!outputFailureRequiresJobPause(admitted, isolating) {
		t.Fatal("admitted session namespace failure did not explicitly require a pause")
	}

	root := NewOutputFault(OutputFaultRoot, OutputFaultNamespaceUnsafe, errors.New("root unsafe"))
	if !outputFailureRequiresJobPause(root, isolating) {
		t.Fatal("root namespace failure did not require a pause")
	}
}

func TestOutputFailurePauseReductionCoversEntireErrorTree(t *testing.T) {
	nonPausing := NewOutputFault(
		OutputFaultSession,
		OutputFaultNamespaceUnsafe,
		errors.New("fresh namespace rejection"),
	)
	pausing := NewOutputFault(
		OutputFaultSession,
		OutputFaultStateIO,
		errors.New("state cleanup failed"),
	)
	explicitPausing := NewOutputSessionError(nonPausing, true)
	explicitNonPausing := NewOutputSessionError(nonPausing, false)
	falseCycle := &outputFailureCycle{}
	falseCycle.children = []error{falseCycle, errors.New("cycle noise")}
	trueCycle := &outputFailureCycle{}
	trueCycle.children = []error{trueCycle, pausing}

	for _, test := range []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil"},
		{name: "non-output failure", err: errors.New("network failure")},
		{name: "false only", err: nonPausing},
		{name: "explicit false only", err: explicitNonPausing},
		{name: "false then true join", err: errors.Join(nonPausing, pausing), want: true},
		{name: "true then false join", err: errors.Join(pausing, nonPausing), want: true},
		{
			name: "nested joins and wrappers",
			err: fmt.Errorf("outer: %w", errors.Join(
				errors.New("diagnostic only"),
				fmt.Errorf("inner: %w", pausing),
				nonPausing,
			)),
			want: true,
		},
		{name: "explicit true wraps false", err: explicitPausing, want: true},
		{name: "explicit false wraps true", err: NewOutputSessionError(pausing, false), want: true},
		{name: "false cycle", err: falseCycle},
		{name: "true behind cycle", err: trueCycle, want: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := outputFailureExplicitlyRequiresJobPause(test.err); got != test.want {
				t.Fatalf("explicit pause = %t, want %t for %v", got, test.want, test.err)
			}
		})
	}
}

func TestOutputSettlementSumTypesRejectMalformedStates(t *testing.T) {
	binding, checkpoint := outputLifecycleFixture(t)

	if _, err := NewVerifiedFileSettlement(FilePublished, VerifiedDurableRanges{}); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("published settlement without a binding error = %v", err)
	}
	if _, err := NewOutputStateRef(OutputSessionID{}, binding.Locator().Digest()); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("state reference without a session error = %v", err)
	}
	if _, err := NewOutputStateRef(binding.OutputSessionID(), OutputLocatorDigest{}); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("state reference without a locator error = %v", err)
	}

	reference, err := NewOutputStateRef(binding.OutputSessionID(), binding.Locator().Digest())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewImmediateQuarantinedFileSettlement(
		OutputFileTarget{}, reference, QuarantineOwnershipMismatch,
	); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("unbound immediate quarantine error = %v", err)
	}
	var foreignLocator OutputLocatorDigest
	foreignLocator[0] = 99
	foreignReference, err := NewOutputStateRef(binding.OutputSessionID(), foreignLocator)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := NewTransactionQuarantinedFileSettlement(
		binding, foreignReference, QuarantineOwnershipMismatch,
	); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("foreign transaction quarantine error = %v", err)
	}

	if FileSettlementKind(0).valid() || !FilePublished.valid() || (FileQuarantined + 1).valid() {
		t.Fatal("file settlement kind admitted a value outside its closed domain")
	}
	malformedQuarantine := FileSettlement{kind: FileQuarantined, target: binding.Target()}
	if malformedQuarantine.valid() {
		t.Fatal("quarantine without durable state evidence was valid")
	}
	if (FileSettlement{kind: FileSettlementKind(255)}).valid() {
		t.Fatal("unknown settlement kind was valid")
	}
	if (FileSettlement{}).matchesBinding(binding) {
		t.Fatal("zero settlement matched an owned binding")
	}

	if _, err := NewFileTransactionStart(nil, checkpoint); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("nil transaction start error = %v", err)
	}
	if _, err := NewFileSettlementStart(FileSettlement{kind: FilePublished}); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("malformed immediate publication error = %v", err)
	}
	if (FileStart{}).valid() {
		t.Fatal("zero file start was valid")
	}
}

func TestOutputLifecycleReasonsAndAuthorityFunctionAreClosedDomains(t *testing.T) {
	if FilePauseReason(0).valid() || !FilePauseInterrupted.valid() || (FilePauseOutputFailure + 1).valid() {
		t.Fatal("file pause reason admitted a value outside its closed domain")
	}
	if JobPauseReason(0).valid() || !JobPauseInterrupted.valid() || (JobPauseOutputFailure + 1).valid() {
		t.Fatal("job pause reason admitted a value outside its closed domain")
	}
	if _, err := NewJobSettlement(JobSettlementKind(0)); !errors.Is(err, ErrInvalidOutputSettlement) {
		t.Fatalf("unknown job settlement error = %v", err)
	}

	var nilAuthority OutputAuthorityFunc
	if _, err := nilAuthority.OpenSelection(context.Background(), OutputSelection{}); !errors.Is(err, ErrInvalidOutputBinding) {
		t.Fatalf("nil output authority error = %v", err)
	}
	want := errors.New("authority invoked")
	called := false
	authority := OutputAuthorityFunc(func(context.Context, OutputSelection) (OutputSession, error) {
		called = true
		return nil, want
	})
	if _, err := authority.OpenSelection(context.Background(), OutputSelection{}); !called || !errors.Is(err, want) {
		t.Fatalf("authority call = (%v, %v), want delegated error", called, err)
	}
}

func outputLifecycleFixture(t *testing.T) (OutputFileBinding, VerifiedDurableRanges) {
	t.Helper()
	descriptor := transferDescriptor(t, 1)
	backend, err := NewOutputBackendID("lifecycle/contracts")
	if err != nil {
		t.Fatal(err)
	}
	locator, err := NewPathOutputLocator("file.bin")
	if err != nil {
		t.Fatal(err)
	}
	var objectIdentity OutputObjectIdentity
	objectIdentity[0] = 32
	binding, err := NewOutputFileBinding(
		backend,
		transferID[OutputSessionID](31),
		descriptor,
		locator,
		objectIdentity,
	)
	if err != nil {
		t.Fatal(err)
	}
	ranges, err := content.NewRangeSet([]content.Range{{Offset: 0, End: descriptor.ExactSize()}})
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := VerifyDurableRanges(binding, 1, ranges)
	if err != nil {
		t.Fatal(err)
	}
	return binding, checkpoint
}

type outputFailureCycle struct {
	children []error
}

func (*outputFailureCycle) Error() string           { return "cyclic output failure" }
func (failure *outputFailureCycle) Unwrap() []error { return failure.children }
