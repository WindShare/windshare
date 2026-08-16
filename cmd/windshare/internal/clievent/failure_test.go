package clievent

import "testing"

func TestFailureRegistryIsClosedAndComplete(t *testing.T) {
	for code := FailureUnexpected; code <= FailureCheckpointStateIO; code++ {
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
	if _, err := NewFailure(FailureCheckpointStateIO + 1); err == nil {
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
