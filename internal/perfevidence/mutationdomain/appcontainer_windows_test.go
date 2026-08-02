//go:build windows

package mutationdomain

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/windshare/windshare/internal/perfevidence"
	"golang.org/x/sys/windows"
)

const (
	claimProbeEnvironment       = "WINDSHARE_CLAIM_PROBE"
	claimProbeChildEnvironment  = "WINDSHARE_CLAIM_PROBE_CHILD"
	claimProbeOutputEnvironment = "WINDSHARE_CLAIM_PROBE_OUTPUT"
)

type claimProbeResult struct {
	Parent appContainerProcessClaim
	Child  appContainerProcessClaim
}

func TestPrivateDomainMeasuresNativeProcessClaimInheritance(t *testing.T) {
	inputRoot := t.TempDir()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(inputRoot, "claim-probe.exe")
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
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := domain.Close(); err != nil {
			t.Error(err)
		}
	})
	hostOutput := filepath.Join(t.TempDir(), "claim.json")
	sink := &memorySink{}
	result, err := domain.Run(context.Background(), perfevidence.MutationDomainCommand{
		Executable: target,
		Arguments:  []string{"-test.run=^TestMutationDomainNativeProcessClaimProbe$"},
		Directory:  inputRoot,
		Environment: mutationDomainTestEnvironment(
			claimProbeEnvironment+"=1",
			claimProbeOutputEnvironment+"="+hostOutput,
		),
		Outputs: []perfevidence.MutationOutput{{HostPath: hostOutput, MaxBytes: 1 << 20}},
	}, map[string]perfevidence.MutationOutputSink{hostOutput: sink})
	if err != nil || result.ExitCode != 0 {
		t.Fatalf(
			"probe native AppContainer claims = exit %d stdout=%q stderr=%q err=%v",
			result.ExitCode,
			result.Stdout,
			result.Stderr,
			err,
		)
	}
	var observed claimProbeResult
	if err := json.Unmarshal(sink.content, &observed); err != nil {
		t.Fatal(err)
	}
	if observed.Parent.Value0 == 0 || observed.Parent.Value1 == 0 ||
		observed.Child.Value0 == 0 || observed.Child.Value1 == 0 {
		t.Fatalf("native process claims contain zero components: %#v", observed)
	}
	if observed.Parent == observed.Child {
		t.Fatalf("TSA://ProcUnique unexpectedly propagated unchanged to a descendant: %#v", observed)
	}
	t.Logf("native parent claim=%#v descendant claim=%#v", observed.Parent, observed.Child)
}

func TestMutationDomainNativeProcessClaimProbe(t *testing.T) {
	if os.Getenv(claimProbeEnvironment) != "1" {
		t.Skip("target-only native AppContainer claim probe")
	}
	claim, err := currentNativeProcessClaim()
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv(claimProbeChildEnvironment) == "1" {
		if err := json.NewEncoder(os.Stdout).Encode(claim); err != nil {
			t.Fatal(err)
		}
		return
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	child := exec.Command(executable, "-test.run=^TestMutationDomainNativeProcessClaimProbe$")
	child.Env = append(os.Environ(), claimProbeChildEnvironment+"=1")
	encodedChild, err := child.Output()
	if err != nil {
		t.Fatal(err)
	}
	var childClaim appContainerProcessClaim
	if err := json.NewDecoder(bytes.NewReader(encodedChild)).Decode(&childClaim); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(claimProbeResult{Parent: claim, Child: childClaim})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(os.Getenv(claimProbeOutputEnvironment), encoded, 0o600); err != nil {
		t.Fatal(err)
	}
}

func currentNativeProcessClaim() (appContainerProcessClaim, error) {
	var token windows.Token
	if err := windows.OpenProcessToken(windows.CurrentProcess(), windows.TOKEN_QUERY, &token); err != nil {
		return appContainerProcessClaim{}, err
	}
	claim, claimErr := appContainerProcessClaimForToken(token)
	return claim, errors.Join(claimErr, token.Close())
}
