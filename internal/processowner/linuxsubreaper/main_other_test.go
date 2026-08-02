//go:build !linux

package linuxsubreaper

import (
	"bytes"
	"testing"
)

func TestUnsupportedPlatformSelfCheckFailsClosed(t *testing.T) {
	var diagnostic bytes.Buffer
	exitCode := 0
	runUnsupportedPlatform(
		[]string{unsupportedSelfCheckCommand},
		&diagnostic,
		func(code int) { exitCode = code },
	)

	if exitCode != 1 {
		t.Fatalf("self-check exit code = %d, want 1", exitCode)
	}
	wantDiagnostic := unsupportedPlatformMessage + "\n"
	if diagnostic.String() != wantDiagnostic {
		t.Fatalf("self-check diagnostic = %q, want %q", diagnostic.String(), wantDiagnostic)
	}
}

func TestUnsupportedPlatformRejectsEveryOtherInvocation(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		arguments []string
	}{
		{name: "missing command"},
		{name: "run command", arguments: []string{"run"}},
		{name: "extra self-check argument", arguments: []string{unsupportedSelfCheckCommand, "extra"}},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			var diagnostic bytes.Buffer
			terminateCalled := false
			panicValue := capturePanic(func() {
				runUnsupportedPlatform(testCase.arguments, &diagnostic, func(int) {
					terminateCalled = true
				})
			})

			if terminateCalled {
				t.Fatal("unsupported command used the self-check termination path")
			}
			if diagnostic.Len() != 0 {
				t.Fatalf("unsupported command diagnostic = %q, want empty", diagnostic.String())
			}
			panicError, ok := panicValue.(error)
			if !ok || panicError.Error() != unsupportedPlatformMessage {
				t.Fatalf("unsupported command panic = %#v, want error %q", panicValue, unsupportedPlatformMessage)
			}
		})
	}
}

func capturePanic(action func()) (panicValue any) {
	defer func() { panicValue = recover() }()
	action()
	return nil
}
