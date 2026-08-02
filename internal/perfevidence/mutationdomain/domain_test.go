package mutationdomain

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/windshare/windshare/internal/perfevidence"
)

const mutationTargetEnvironment = "WINDSHARE_MUTATION_TARGET"

const mutationTargetTimingCeiling = 2 * time.Second

const (
	mutationCopyExecutableEnvironment     = "WINDSHARE_MUTATION_COPY_EXECUTABLE"
	mutationPromotedExecutableEnvironment = "WINDSHARE_MUTATION_PROMOTED_EXECUTABLE"
	mutationPromotedTargetEnvironment     = "WINDSHARE_MUTATION_PROMOTED_TARGET"
	mutationReuseTargetEnvironment        = "WINDSHARE_MUTATION_REUSE_TARGET"
)

func TestMain(m *testing.M) {
	if handled, code := MaybeRunHelper(os.Args[1:], os.Stdin, os.Stdout, os.Stderr); handled {
		os.Exit(code)
	}
	os.Exit(m.Run())
}

func TestPrivateDomainFramesOutputsAndMeasuresOnlyTarget(t *testing.T) {
	inputRoot := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(inputRoot, "target.test")
	if runtime.GOOS == "windows" {
		target += ".exe"
	}
	if err := copyRegularTestFile(executable, target); err != nil {
		t.Fatal(err)
	}
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(runtimeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	domain, err := NewFactory().Open(context.Background(), perfevidence.MutationDomainSpec{
		RuntimeRoot: runtimeRoot,
		Roots:       []perfevidence.MutationRoot{{Name: "test", HostPath: inputRoot}},
	})
	if err != nil {
		t.Fatalf("private mutation primitive is unavailable: %v", err)
	}
	defer func() {
		if err := domain.Close(); err != nil {
			t.Error(err)
		}
	}()
	hostOutput := filepath.Join(t.TempDir(), "result.bin")
	sink := &memorySink{}
	environment := []string{
		mutationTargetEnvironment + "=1",
		"WINDSHARE_MUTATION_OUTPUT=" + hostOutput,
	}
	if runtime.GOOS == "windows" {
		environment = append(environment, "SystemRoot="+os.Getenv("SystemRoot"), "WINDIR="+os.Getenv("WINDIR"))
	}
	result, err := domain.Run(context.Background(), perfevidence.MutationDomainCommand{
		Executable:  target,
		Arguments:   []string{"-test.run=^TestMutationDomainTarget$"},
		Directory:   inputRoot,
		Environment: environment,
		Outputs:     []perfevidence.MutationOutput{{HostPath: hostOutput, MaxBytes: 1 << 20}},
	}, map[string]perfevidence.MutationOutputSink{hostOutput: sink})
	if err != nil {
		t.Fatal(err)
	}
	if result.ExitCode != 0 || !strings.HasPrefix(string(result.Stdout), "target-stdout") {
		t.Fatalf("isolated result = exit %d stdout %q stderr %q", result.ExitCode, result.Stdout, result.Stderr)
	}
	if elapsed := result.FinishedAt.Sub(result.StartedAt); elapsed < 20*time.Millisecond || elapsed > mutationTargetTimingCeiling {
		t.Fatalf("target timing = %s, want target-only sleep interval", elapsed)
	}
	if string(sink.content) != "protected-output" || !sink.committed {
		t.Fatalf("framed output = %q committed=%v", sink.content, sink.committed)
	}
}

func TestMutationDomainTarget(t *testing.T) {
	if os.Getenv(mutationTargetEnvironment) != "1" {
		t.Skip("target-only helper test")
	}
	time.Sleep(25 * time.Millisecond)
	if err := os.WriteFile(os.Getenv("WINDSHARE_MUTATION_OUTPUT"), []byte("protected-output"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _ = fmt.Fprint(os.Stdout, "target-stdout")
}

func TestPrivateDomainPromotesReusableOutputIntoImmutableGeneration(t *testing.T) {
	inputRoot := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(inputRoot, "promotion-source.test")
	if runtime.GOOS == "windows" {
		target += ".exe"
	}
	if err := copyRegularTestFile(executable, target); err != nil {
		t.Fatal(err)
	}
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(runtimeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	domain, err := NewFactory().Open(context.Background(), perfevidence.MutationDomainSpec{
		RuntimeRoot: runtimeRoot,
		Roots:       []perfevidence.MutationRoot{{Name: "test", HostPath: inputRoot}},
	})
	if err != nil {
		t.Fatalf("private mutation primitive is unavailable: %v", err)
	}
	defer func() {
		if err := domain.Close(); err != nil {
			t.Error(err)
		}
	}()
	hostOutput := filepath.Join(t.TempDir(), "promoted.test")
	if runtime.GOOS == "windows" {
		hostOutput += ".exe"
	}
	firstEnvironment := mutationDomainTestEnvironment(
		mutationCopyExecutableEnvironment + "=" + hostOutput,
	)
	first, err := domain.Run(context.Background(), perfevidence.MutationDomainCommand{
		Executable:  target,
		Arguments:   []string{"-test.run=^TestMutationDomainCopiesExecutableForPromotion$"},
		Directory:   inputRoot,
		Environment: firstEnvironment,
		Outputs: []perfevidence.MutationOutput{{
			HostPath: hostOutput, MaxBytes: 64 << 20,
		}},
	}, map[string]perfevidence.MutationOutputSink{hostOutput: &memorySink{}})
	if err != nil || first.ExitCode != 0 {
		t.Fatalf("produce promotable executable = exit %d stderr=%q err=%v", first.ExitCode, first.Stderr, err)
	}
	second, err := domain.Run(context.Background(), perfevidence.MutationDomainCommand{
		Executable: hostOutput,
		Arguments:  []string{"-test.run=^TestMutationDomainPromotedExecutableTarget$"},
		Directory:  inputRoot,
		Environment: mutationDomainTestEnvironment(
			mutationPromotedTargetEnvironment+"=1",
			mutationPromotedExecutableEnvironment+"="+hostOutput,
		),
	}, nil)
	if err != nil || second.ExitCode != 0 || !strings.Contains(string(second.Stdout), "promoted-generation-is-immutable") {
		t.Fatalf("reuse promoted executable = exit %d stdout=%q stderr=%q err=%v", second.ExitCode, second.Stdout, second.Stderr, err)
	}
}

func TestMutationDomainCopiesExecutableForPromotion(t *testing.T) {
	destination := os.Getenv(mutationCopyExecutableEnvironment)
	if destination == "" {
		t.Skip("target-only output promotion producer")
	}
	sourcePath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	source, err := os.Open(sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	destinationFile, err := os.OpenFile(destination, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o700)
	if err != nil {
		t.Fatal(err)
	}
	_, copyErr := io.Copy(destinationFile, source)
	targetErr := errors.Join(copyErr, destinationFile.Sync(), destinationFile.Close(), source.Close())
	if targetErr != nil {
		t.Fatal(targetErr)
	}
}

func TestMutationDomainPromotedExecutableTarget(t *testing.T) {
	if os.Getenv(mutationPromotedTargetEnvironment) != "1" {
		t.Skip("target-only promoted executable probe")
	}
	path := os.Getenv(mutationPromotedExecutableEnvironment)
	if path == "" {
		t.Fatal("promoted executable path was not rewritten")
	}
	if err := os.Rename(path, path+".displaced"); err == nil {
		t.Fatal("promoted executable generation allowed a namespace displacement")
	}
	if err := os.WriteFile(path, []byte("replacement"), 0o700); err == nil {
		t.Fatal("promoted executable generation allowed an in-place replacement")
	}
	if err := os.WriteFile(filepath.Join(filepath.Dir(path), "replacement"), []byte("replacement"), 0o700); err == nil {
		t.Fatal("promoted executable generation allowed a sibling replacement")
	}
	_, _ = fmt.Fprint(os.Stdout, "promoted-generation-is-immutable")
}

func TestPrivateDomainDoesNotPromoteFailedProducerOutput(t *testing.T) {
	inputRoot := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(inputRoot, "failed-producer.test")
	if runtime.GOOS == "windows" {
		target += ".exe"
	}
	if err := copyRegularTestFile(executable, target); err != nil {
		t.Fatal(err)
	}
	runtimeRoot := filepath.Join(t.TempDir(), "runtime")
	if err := os.Mkdir(runtimeRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	domain, err := NewFactory().Open(context.Background(), perfevidence.MutationDomainSpec{
		RuntimeRoot: runtimeRoot,
		Roots:       []perfevidence.MutationRoot{{Name: "test", HostPath: inputRoot}},
	})
	if err != nil {
		t.Fatalf("private mutation primitive is unavailable: %v", err)
	}
	defer func() { _ = domain.Close() }()
	hostOutput := filepath.Join(t.TempDir(), "failed-output.test")
	sink := &memorySink{}
	failed, err := domain.Run(context.Background(), perfevidence.MutationDomainCommand{
		Executable: target,
		Arguments:  []string{"-test.run=^TestMutationDomainFailedProducerTarget$"},
		Directory:  inputRoot,
		Environment: mutationDomainTestEnvironment(
			mutationCopyExecutableEnvironment + "=" + hostOutput,
		),
		Outputs: []perfevidence.MutationOutput{{HostPath: hostOutput, MaxBytes: 64 << 20}},
	}, map[string]perfevidence.MutationOutputSink{hostOutput: sink})
	if err == nil || failed.ExitCode == 0 || sink.committed {
		t.Fatalf("failed producer = exit %d committed=%v err=%v", failed.ExitCode, sink.committed, err)
	}
	if _, err := domain.Run(context.Background(), perfevidence.MutationDomainCommand{
		Executable: hostOutput, Directory: inputRoot,
	}, nil); err == nil || !strings.Contains(err.Error(), "outside the admitted immutable inputs") {
		t.Fatalf("failed producer output became executable: %v", err)
	}
	reused, err := domain.Run(context.Background(), perfevidence.MutationDomainCommand{
		Executable: target,
		Arguments:  []string{"-test.run=^TestMutationDomainCleanReuseTarget$"},
		Directory:  inputRoot,
		Environment: mutationDomainTestEnvironment(
			mutationReuseTargetEnvironment + "=1",
		),
	}, nil)
	if err != nil || reused.ExitCode != 0 || !strings.Contains(string(reused.Stdout), "failed-output-was-not-admitted") {
		t.Fatalf("reuse after rejected failed output = exit %d stdout=%q stderr=%q err=%v", reused.ExitCode, reused.Stdout, reused.Stderr, err)
	}
}

func TestMutationDomainFailedProducerTarget(t *testing.T) {
	destination := os.Getenv(mutationCopyExecutableEnvironment)
	if destination == "" {
		t.Skip("target-only failed output producer")
	}
	sourcePath, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	if err := copyRegularTestFile(sourcePath, destination); err != nil {
		t.Fatal(err)
	}
	t.Fatal("intentional producer failure after writing output")
}

func TestMutationDomainCleanReuseTarget(t *testing.T) {
	if os.Getenv(mutationReuseTargetEnvironment) != "1" {
		t.Skip("target-only post-failure reuse probe")
	}
	_, _ = fmt.Fprint(os.Stdout, "failed-output-was-not-admitted")
}

func mutationDomainTestEnvironment(entries ...string) []string {
	if runtime.GOOS == "windows" {
		entries = append(entries, "SystemRoot="+os.Getenv("SystemRoot"), "WINDIR="+os.Getenv("WINDIR"))
	}
	return entries
}

type memorySink struct {
	content   []byte
	committed bool
}

func (sink *memorySink) WriteContext(_ context.Context, content []byte) (int, error) {
	sink.content = append(sink.content, content...)
	return len(content), nil
}

func (sink *memorySink) Seal(_ context.Context, bytes int64, expected string) error {
	digest := sha256.Sum256(sink.content)
	if bytes != int64(len(sink.content)) || expected != hex.EncodeToString(digest[:]) {
		return fmt.Errorf("identity mismatch")
	}
	sink.committed = true
	return nil
}

func (sink *memorySink) Abort(context.Context) error { return nil }

func copyRegularTestFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o700)
	if err != nil {
		_ = input.Close()
		return err
	}
	_, copyErr := io.Copy(output, input)
	return errors.Join(copyErr, output.Close(), input.Close())
}
