package v2peer_test

import (
	"errors"
	"testing"
	"time"
)

func TestIntegrationRoutineJoinContract(t *testing.T) {
	canceled := make(chan error, 1)
	canceled <- errors.New("wrapped: context canceled")
	if err := joinIntegrationRoutine(
		"routine",
		canceled,
		time.Second,
		func(err error) bool { return err != nil },
	); err != nil {
		t.Fatal(err)
	}

	unexpected := make(chan error, 1)
	unexpected <- nil
	if err := joinIntegrationRoutine("routine", unexpected, time.Second, func(err error) bool {
		return err != nil
	}); err == nil {
		t.Fatal("unexpected terminal result was accepted")
	}
	if err := joinIntegrationRoutine("", unexpected, time.Second, func(error) bool { return true }); err == nil {
		t.Fatal("invalid join contract was accepted")
	}
	if err := joinIntegrationRoutine(
		"blocked routine",
		make(chan error),
		time.Millisecond,
		func(error) bool { return true },
	); err == nil {
		t.Fatal("routine timeout was accepted")
	}
}

func TestIntegrationSignalJoinContract(t *testing.T) {
	done := make(chan struct{})
	close(done)
	want := errors.New("terminal failure")
	if err := joinIntegrationSignal("signal", done, time.Second, func() error { return want }); !errors.Is(err, want) {
		t.Fatalf("signal result = %v, want %v", err, want)
	}
	if err := joinIntegrationSignal("", done, time.Second, func() error { return nil }); err == nil {
		t.Fatal("invalid join contract was accepted")
	}
	if err := joinIntegrationSignal(
		"blocked signal",
		make(chan struct{}),
		time.Millisecond,
		func() error { return nil },
	); err == nil {
		t.Fatal("signal timeout was accepted")
	}
}
