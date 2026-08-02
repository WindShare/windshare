package perfevidence

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/processowner/protocol"
)

const emptyOutputSHA256 = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func TestPrivateMutationValidatesEveryOutputBeforePreparingSinks(t *testing.T) {
	directory := t.TempDir()
	valid := func(name string) MutationOutput {
		return MutationOutput{HostPath: filepath.Join(directory, name), MaxBytes: 1}
	}
	testCases := map[string][]MutationOutput{
		"relative path": {{HostPath: "relative.out", MaxBytes: 1}},
		"unclean path": {{
			HostPath: directory + string(os.PathSeparator) + "child" + string(os.PathSeparator) +
				".." + string(os.PathSeparator) + "unclean.out",
			MaxBytes: 1,
		}},
		"zero maximum": {{HostPath: filepath.Join(directory, "zero.out"), MaxBytes: 0}},
		"oversize maximum": {{
			HostPath: filepath.Join(directory, "oversize.out"), MaxBytes: maximumProtectedOutputBytes + 1,
		}},
		"duplicate path": {valid("duplicate.out"), valid("duplicate.out")},
		"count N plus one": {
			valid("count-a.out"),
			valid("count-b.out"),
			valid("count-c.out"),
		},
	}
	for name, outputs := range testCases {
		t.Run(name, func(t *testing.T) {
			prepared := 0
			domain := &scriptedMutationDomain{}
			runner := ProcessRunner{
				MutationDomain: domain,
				prepareOutput: func(string) (mutationOutputSink, error) {
					prepared++
					return nil, errors.New("must not prepare")
				},
			}
			_, err := runner.Run(context.Background(), Command{
				mutationIntent: mutationIntentArtifactProduction, protectedOutputs: outputs,
			})
			if err == nil {
				t.Fatal("invalid protected outputs were accepted")
			}
			if prepared != 0 || domain.calls != 0 {
				t.Fatalf("invalid output acquired authority: prepared=%d domain_calls=%d", prepared, domain.calls)
			}
		})
	}
}

func TestPrivateMutationRejectsHugeOutputCountWithoutFilesystemWork(t *testing.T) {
	const hostileOutputCount = 1 << 20
	outputs := make([]MutationOutput, hostileOutputCount)
	prepared := 0
	domain := &scriptedMutationDomain{}
	runner := ProcessRunner{
		MutationDomain: domain,
		prepareOutput: func(string) (mutationOutputSink, error) {
			prepared++
			return nil, errors.New("must not prepare")
		},
	}

	_, err := runner.Run(context.Background(), Command{
		mutationIntent:   mutationIntentArtifactProduction,
		protectedOutputs: outputs,
	})
	if err == nil || !strings.Contains(err.Error(), "protected output count") {
		t.Fatalf("huge output count error = %v", err)
	}
	if prepared != 0 || domain.calls != 0 {
		t.Fatalf("huge output input performed work: prepared=%d domain_calls=%d", prepared, domain.calls)
	}
}

func TestPrivateMutationAbortsEveryAcquiredSinkWhenPreparationFails(t *testing.T) {
	directory := t.TempDir()
	outputs := mutationTestOutputs(directory)
	first := newLifecycleMutationSink(outputs[0].HostPath)
	cause := errors.New("second sink preparation failed")
	prepared := 0
	runner := ProcessRunner{
		MutationDomain: &scriptedMutationDomain{},
		prepareOutput: func(string) (mutationOutputSink, error) {
			prepared++
			if prepared == 1 {
				return first, nil
			}
			return nil, cause
		},
	}
	result, err := runner.Run(context.Background(), Command{
		mutationIntent: mutationIntentArtifactProduction, protectedOutputs: outputs,
	})
	if !errors.Is(err, cause) || len(result.outputAuthorities) != 0 {
		t.Fatalf("preparation failure = result %#v, error %v", result, err)
	}
	assertMutationSinkRolledBack(t, first)
}

func TestPrivateMutationGroupFailureRollsBackAllSealedOutputs(t *testing.T) {
	testCases := map[string]func([]*lifecycleMutationSink, *scriptedMutationDomain) error{
		"domain failure after seals": func(_ []*lifecycleMutationSink, domain *scriptedMutationDomain) error {
			domain.resultErr = errors.New("domain settlement failed")
			return domain.resultErr
		},
		"second seal failure": func(sinks []*lifecycleMutationSink, _ *scriptedMutationDomain) error {
			sinks[1].sealErr = errors.New("second seal failed")
			return sinks[1].sealErr
		},
		"second adoption failure": func(sinks []*lifecycleMutationSink, _ *scriptedMutationDomain) error {
			sinks[1].adoptErr = errors.New("second adoption failed")
			return sinks[1].adoptErr
		},
	}
	for name, configure := range testCases {
		t.Run(name, func(t *testing.T) {
			directory := t.TempDir()
			outputs := mutationTestOutputs(directory)
			sinks := []*lifecycleMutationSink{
				newLifecycleMutationSink(outputs[0].HostPath),
				newLifecycleMutationSink(outputs[1].HostPath),
			}
			domain := &scriptedMutationDomain{}
			cause := configure(sinks, domain)
			next := 0
			runner := ProcessRunner{
				MutationDomain: domain,
				prepareOutput: func(string) (mutationOutputSink, error) {
					sink := sinks[next]
					next++
					return sink, nil
				},
			}
			result, err := runner.Run(context.Background(), Command{
				mutationIntent: mutationIntentArtifactProduction, protectedOutputs: outputs,
			})
			if !errors.Is(err, cause) {
				t.Fatalf("group failure = %v, want %v", err, cause)
			}
			if len(result.outputAuthorities) != 0 {
				t.Fatalf("failed group exposed authorities: %#v", result.outputAuthorities)
			}
			for _, sink := range sinks {
				assertMutationSinkRolledBack(t, sink)
			}
		})
	}
}

func TestPrivateMutationFinalizesAggregateCeilingOnlyAfterWholeGroupAdoption(t *testing.T) {
	directory := t.TempDir()
	outputs := mutationTestOutputs(directory)
	for index := range outputs {
		outputs[index].MaxBytes = maximumProtectedOutputBytes
	}
	sinks := []*lifecycleMutationSink{
		newLifecycleMutationSink(outputs[0].HostPath),
		newLifecycleMutationSink(outputs[1].HostPath),
	}
	next := 0
	runner := ProcessRunner{
		MutationDomain: &scriptedMutationDomain{},
		prepareOutput: func(string) (mutationOutputSink, error) {
			sink := sinks[next]
			next++
			return sink, nil
		},
	}
	result, err := runner.Run(context.Background(), Command{
		mutationIntent: mutationIntentArtifactProduction, protectedOutputs: outputs,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.outputAuthorities) != len(outputs) {
		t.Fatalf("output authority count = %d", len(result.outputAuthorities))
	}
	for _, sink := range sinks {
		if sink.sealCalls != 1 || sink.adoptCalls != 1 || sink.finalizeCalls != 1 || sink.abortCalls != 1 {
			t.Fatalf("successful sink lifecycle = %#v", sink)
		}
		if _, err := os.Stat(sink.path); err != nil {
			t.Fatalf("finalized output %s: %v", sink.path, err)
		}
	}
}

func TestPrivateMutationBoundsWholeGroupAbortSettlement(t *testing.T) {
	const abortTimeout = 40 * time.Millisecond
	directory := t.TempDir()
	outputs := mutationTestOutputs(directory)
	sinks := []*lifecycleMutationSink{
		newLifecycleMutationSink(outputs[0].HostPath),
		newLifecycleMutationSink(outputs[1].HostPath),
	}
	for _, sink := range sinks {
		sink.waitForAbortContext = true
	}
	domain := &scriptedMutationDomain{resultErr: errors.New("domain settlement failed")}
	next := 0
	runner := ProcessRunner{
		MutationDomain:              domain,
		protectedOutputAbortTimeout: abortTimeout,
		prepareOutput: func(string) (mutationOutputSink, error) {
			sink := sinks[next]
			next++
			return sink, nil
		},
	}

	started := time.Now()
	_, err := runner.Run(context.Background(), Command{
		mutationIntent:   mutationIntentArtifactProduction,
		protectedOutputs: outputs,
	})
	elapsed := time.Since(started)
	if !errors.Is(err, domain.resultErr) || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("bounded abort error = %v", err)
	}
	if elapsed < abortTimeout/2 || elapsed > 10*abortTimeout {
		t.Fatalf("whole-group abort settlement elapsed %v for timeout %v", elapsed, abortTimeout)
	}
	for _, sink := range sinks {
		assertMutationSinkRolledBack(t, sink)
	}
}

func TestPrivateVerificationRunsWithoutProtectedOutputs(t *testing.T) {
	domain := &scriptedMutationDomain{}
	prepared := 0
	runner := ProcessRunner{
		MutationDomain: domain,
		prepareOutput: func(string) (mutationOutputSink, error) {
			prepared++
			return nil, errors.New("verification must not prepare an output sink")
		},
	}

	result, err := runner.Run(context.Background(), Command{
		mutationIntent: mutationIntentVerification,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || len(result.outputAuthorities) != 0 {
		t.Fatalf("verification result = %#v", result)
	}
	if prepared != 0 || domain.calls != 1 {
		t.Fatalf("verification lifecycle: prepared=%d domain_calls=%d", prepared, domain.calls)
	}
}

func TestPrivateMutationIntentRejectsContradictoryOutputContracts(t *testing.T) {
	directory := t.TempDir()
	testCases := map[string]Command{
		"artifact production without outputs": {
			mutationIntent: mutationIntentArtifactProduction,
		},
		"verification with output": {
			mutationIntent: mutationIntentVerification,
			protectedOutputs: []MutationOutput{{
				HostPath: filepath.Join(directory, "verification.out"),
				MaxBytes: 1,
			}},
		},
	}
	for name, command := range testCases {
		t.Run(name, func(t *testing.T) {
			prepared := 0
			domain := &scriptedMutationDomain{}
			runner := ProcessRunner{
				MutationDomain: domain,
				prepareOutput: func(string) (mutationOutputSink, error) {
					prepared++
					return nil, errors.New("invalid contract must not prepare")
				},
			}

			result, err := runner.Run(context.Background(), command)
			if err == nil {
				t.Fatal("contradictory mutation contract was accepted")
			}
			if result.ExitCode != -1 || len(result.outputAuthorities) != 0 {
				t.Fatalf("rejected mutation result = %#v", result)
			}
			if prepared != 0 || domain.calls != 0 {
				t.Fatalf("invalid contract acquired authority: prepared=%d domain_calls=%d", prepared, domain.calls)
			}
		})
	}
}

func mutationTestOutputs(directory string) []MutationOutput {
	return []MutationOutput{
		{HostPath: filepath.Join(directory, "first.out"), MaxBytes: 1},
		{HostPath: filepath.Join(directory, "second.out"), MaxBytes: 1},
	}
}

type scriptedMutationDomain struct {
	calls     int
	resultErr error
}

func (domain *scriptedMutationDomain) Run(
	ctx context.Context,
	command MutationDomainCommand,
	sinks map[string]MutationOutputSink,
) (MutationDomainResult, error) {
	domain.calls++
	for _, output := range command.Outputs {
		if err := sinks[output.HostPath].Seal(ctx, 0, emptyOutputSHA256); err != nil {
			return MutationDomainResult{ExitCode: 0}, err
		}
	}
	return MutationDomainResult{ExitCode: 0}, domain.resultErr
}

func (*scriptedMutationDomain) Close() error { return nil }

type lifecycleMutationSink struct {
	path                string
	sealErr             error
	adoptErr            error
	sealCalls           int
	adoptCalls          int
	finalizeCalls       int
	abortCalls          int
	sealed              bool
	adopted             bool
	finalized           bool
	waitForAbortContext bool
	authority           *lifecycleMutationAuthority
}

func newLifecycleMutationSink(path string) *lifecycleMutationSink {
	return &lifecycleMutationSink{path: path, authority: &lifecycleMutationAuthority{}}
}

func (*lifecycleMutationSink) WriteContext(context.Context, []byte) (int, error) { return 0, nil }

func (sink *lifecycleMutationSink) Seal(context.Context, int64, string) error {
	sink.sealCalls++
	if sink.sealErr != nil {
		return sink.sealErr
	}
	if err := os.WriteFile(sink.path, []byte("sealed"), 0o600); err != nil {
		return err
	}
	sink.sealed = true
	return nil
}

func (sink *lifecycleMutationSink) adopt() (byteConsumptionAuthority, error) {
	sink.adoptCalls++
	if sink.adoptErr != nil {
		return nil, sink.adoptErr
	}
	if !sink.sealed || sink.adopted {
		return nil, errors.New("fake sink is not adoptable")
	}
	sink.adopted = true
	return sink.authority, nil
}

func (sink *lifecycleMutationSink) finalize() {
	sink.finalizeCalls++
	sink.finalized = true
}

func (sink *lifecycleMutationSink) Abort(ctx context.Context) error {
	sink.abortCalls++
	if sink.finalized {
		return nil
	}
	var waitErr error
	if sink.waitForAbortContext {
		<-ctx.Done()
		waitErr = context.Cause(ctx)
	}
	sink.sealed = false
	sink.adopted = false
	if err := os.Remove(sink.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.Join(waitErr, err)
	}
	return waitErr
}

type lifecycleMutationAuthority struct{}

func (*lifecycleMutationAuthority) Verify() error { return nil }
func (*lifecycleMutationAuthority) VerifyProcessStart(protocol.StartEvidence, string) (bool, error) {
	return false, nil
}
func (*lifecycleMutationAuthority) Close() error { return nil }

func assertMutationSinkRolledBack(t *testing.T, sink *lifecycleMutationSink) {
	t.Helper()
	if sink.abortCalls != 1 || sink.finalizeCalls != 0 || sink.finalized {
		t.Fatalf("failed sink lifecycle = %#v", sink)
	}
	if _, err := os.Stat(sink.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("rolled-back output %s still exists: %v", sink.path, err)
	}
}
