package observation

import (
	"testing"
)

func TestSourcePathFailureRejectsUnrelatedContext(t *testing.T) {
	for _, code := range []FailureCode{FailureSourceMissing, FailureSourceAccessDenied,
		FailureSourceUnsupported, FailureSourceUnavailable, FailureSourceRevisionChanged} {
		failure, err := NewSourcePathFailure(code, NewDisplayPath("selected"))
		path, located := failure.SourcePath()
		if err != nil || !failure.Valid() || !located || path.Text() != "selected" {
			t.Fatalf("source context rejected: %+v, %v", failure, err)
		}
	}
	for _, code := range []FailureCode{FailureUnexpected, FailureOutputStateIO, FailureRelayTransport, FailureSelectionCollision, 0} {
		if _, err := NewSourcePathFailure(code, NewDisplayPath("selected")); err == nil {
			t.Fatalf("accepted source path for unrelated failure %v", code)
		}
	}
	if _, err := NewSourcePathFailure(FailureSourceMissing, DisplayPath{}); err == nil {
		t.Fatal("accepted an empty source path")
	}
}

func TestSelectionCollisionContextPreservesBothPaths(t *testing.T) {
	first, second := NewDisplayPath("a/notes.txt"), NewDisplayPath("b/NOTES.TXT")
	for _, paths := range [][2]DisplayPath{{first, second}, {first, first}} {
		failure, err := NewSelectionCollisionFailure(paths[0], paths[1])
		collision, located := failure.SelectionCollision()
		if err != nil || !failure.Valid() || failure.Code() != FailureSelectionCollision || !located ||
			collision.FirstPath() != paths[0] || collision.SecondPath() != paths[1] {
			t.Fatalf("collision context rejected: %+v, %v", failure, err)
		}
		if _, located := failure.SourcePath(); located {
			t.Fatal("a two-selection conflict became a single-source failure")
		}
	}
	for _, paths := range [][2]DisplayPath{{{}, {}}, {first, {}}, {{}, second}} {
		if _, err := NewSelectionCollisionFailure(paths[0], paths[1]); err == nil {
			t.Fatal("accepted a collision without both paths")
		}
	}
	collision := SelectionCollision{firstPath: first, secondPath: second}
	for _, invalid := range []Failure{
		{code: FailureUnexpected, selectionCollision: collision},
		{code: FailureSelectionCollision, selectionCollision: SelectionCollision{firstPath: first}},
		{code: FailureSelectionCollision, selectionCollision: collision, sourcePath: first},
		{code: FailureSelectionCollision, selectionCollision: collision, hasFault: true},
		{code: FailureSelectionCollision, selectionCollision: collision, hasRetryAfter: true},
	} {
		if invalid.Valid() {
			t.Fatalf("accepted contradictory collision context: %+v", invalid)
		}
	}
}

func TestFailureRegistryIsClosedAndComplete(t *testing.T) {
	for code := FailureUnexpected; code <= FailureRetainedOutputMissing; code++ {
		name, nameOK := code.Name()
		key, keyOK := code.MessageKey()
		keyName, keyNameOK := key.Name()
		failure, err := NewFailure(code)
		if !nameOK || name == "" || !keyOK || !keyNameOK || keyName == "" || err != nil || !failure.Valid() {
			t.Fatalf("failure code %d registry incomplete: name=%q key=%d/%q err=%v", code, name, key, keyName, err)
		}
	}
	if _, err := NewFailure(0); err == nil {
		t.Fatal("accepted zero failure code")
	}
	if _, err := NewFailure(FailureRetainedOutputMissing + 1); err == nil {
		t.Fatal("accepted unknown failure code")
	}
}

func TestFailureContextCannotBeAttachedToTheWrongSemanticCode(t *testing.T) {
	context, err := NewFaultContext(FaultSource, FaultFileLocal, 2)
	if err != nil {
		t.Fatal(err)
	}
	failure, err := NewFaultFailure(FailureSourceRevisionChanged, context)
	if err != nil || !failure.Valid() {
		t.Fatalf("valid fault failure rejected: %+v err=%v", failure, err)
	}
	if _, err := NewFaultFailure(FailureOutputStateIO, context); err == nil {
		t.Fatal("accepted source context under output code")
	}
	if _, err := NewFaultFailure(FailureSourceUnavailable, context); err == nil {
		t.Fatal("accepted mismatched numeric source code")
	}
	if _, err := NewFaultFailure(FailureUnexpected, context); err == nil {
		t.Fatal("accepted fault context on non-fault code")
	}

	retry, err := NewRetryableFailure(FailureRelayStarting, 1250)
	if err != nil || !retry.Valid() {
		t.Fatalf("valid retry failure rejected: %+v err=%v", retry, err)
	}
	if millis, ok := retry.RetryAfterMillis(); !ok || millis != 1250 {
		t.Fatalf("retry context = %d,%t", millis, ok)
	}
	if _, err := NewRetryableFailure(FailureRelayStopped, 1); err == nil {
		t.Fatal("accepted retry context for non-retryable relay code")
	}
}
