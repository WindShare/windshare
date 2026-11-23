package powerinhibit

import (
	"errors"
	"testing"
)

func TestInhibitorNormalizesReasonAndDeduplicatesRelease(t *testing.T) {
	t.Parallel()

	var gotReason string
	releases := 0
	inhibitor := newWithAcquire(func(reason string) (func(), error) {
		gotReason = reason
		return func() { releases++ }, nil
	})

	release, err := inhibitor.InhibitSleep("   ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if gotReason != defaultInhibitReason {
		t.Fatalf("reason = %q, want %q", gotReason, defaultInhibitReason)
	}
	release()
	release()
	if releases != 1 {
		t.Fatalf("releases = %d, want 1", releases)
	}
}

func TestInhibitorHandlesNilAndErrorsSafely(t *testing.T) {
	t.Parallel()

	var nilInhibitor *Inhibitor
	release, err := nilInhibitor.InhibitSleep("test")
	if err != nil || release == nil {
		t.Fatalf("nil inhibitor returned nilRelease=%v err=%v", release == nil, err)
	}
	release()

	sentinel := errors.New("power request denied")
	failing := newWithAcquire(func(string) (func(), error) {
		return nil, sentinel
	})
	release, err = failing.InhibitSleep("active share")
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}
	if release == nil {
		t.Fatal("expected non-nil release on failure")
	}
	release()

	nilRelease := newWithAcquire(func(string) (func(), error) {
		return nil, nil
	})
	release, err = nilRelease.InhibitSleep("active share")
	if err != nil || release == nil {
		t.Fatalf("nil release backend returned nilRelease=%v err=%v", release == nil, err)
	}
	release()
}
