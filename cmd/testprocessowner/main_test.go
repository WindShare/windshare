package main

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
)

func TestSelfCheckHasOneStableRecord(t *testing.T) {
	if os.Getenv("WINDSHARE_TESTPROCESSOWNER_SELF_CHECK") == "1" {
		if err := run([]string{commandSelfCheck}); err != nil {
			t.Fatal(err)
		}
		os.Exit(0)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	command := exec.Command(executable, "-test.run=TestSelfCheckHasOneStableRecord")
	command.Env = append(os.Environ(), "WINDSHARE_TESTPROCESSOWNER_SELF_CHECK=1")
	stdout, err := command.Output()
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"schema_version\":\"windshare.process-owner-self-check/v1\",\"component\":\"testprocessowner\",\"milestone\":\"self_check\",\"outcome\":\"ready\"}\n"
	if string(stdout) != want {
		t.Fatalf("self-check stdout = %q, want %q", stdout, want)
	}
	if strings.Contains(string(stdout), "browser-evidence") {
		t.Fatal("self-check retained a Browsergate-specific component")
	}
}

func TestRunDispatchAndSelfCheckAreTestableWithoutProcessInterception(t *testing.T) {
	var output bytes.Buffer
	if err := runWithOutput([]string{commandSelfCheck}, &output); err != nil {
		t.Fatal(err)
	}
	want := "{\"schema_version\":\"windshare.process-owner-self-check/v1\",\"component\":\"testprocessowner\",\"milestone\":\"self_check\",\"outcome\":\"ready\"}\n"
	if output.String() != want {
		t.Fatalf("self-check = %q", output.String())
	}
	if err := runWithOutput(nil, &output); err == nil {
		t.Fatal("missing command was accepted")
	}
	if err := runWithOutput([]string{"unknown"}, &output); err == nil {
		t.Fatal("unknown platform command was accepted")
	}
	if err := run([]string{"unknown"}); err == nil {
		t.Fatal("run wrapper accepted an unknown platform command")
	}
}
