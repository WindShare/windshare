package output

import (
	"context"
	"errors"
	"testing"

	"github.com/windshare/windshare/core/transfer/receivecontract"
)

type reservationFixture struct{}

func (reservationFixture) Create(context.Context, receivecontract.ArtifactSpec) (Operation, error) {
	return Operation{}, errors.New("unused")
}

func TestLookupCannotClaimReopenedWorkWithoutAnOutputOperation(t *testing.T) {
	if (Operation{}).Valid() || (Mode(0)).Valid() || Mode(255).Valid() {
		t.Fatal("empty output state accepted")
	}
	if !OutputResumable.Valid() || !OutputLiveOnly.Valid() {
		t.Fatal("output mode rejected")
	}
	for _, invalid := range []Lookup{
		{Kind: OutputLookupCreateReady},
		{Kind: OutputLookupNotFound, Reservation: reservationFixture{}},
		{Kind: OutputLookupAlreadyRunning, Reservation: reservationFixture{}},
		{Kind: OutputLookupReopened},
		{Kind: 255, Reservation: reservationFixture{}},
	} {
		if invalid.Valid() {
			t.Fatalf("invalid lookup accepted: %+v", invalid)
		}
	}
	for _, valid := range []Lookup{
		{Kind: OutputLookupCreateReady, Reservation: reservationFixture{}},
		{Kind: OutputLookupNotFound},
		{Kind: OutputLookupAlreadyRunning},
		{Kind: OutputLookupNeedsAttention},
		{Kind: OutputLookupAmbiguous},
	} {
		if !valid.Valid() {
			t.Fatalf("valid lookup rejected: %+v", valid)
		}
	}
}

func TestFactoryOwnsProviderConstructionFailure(t *testing.T) {
	var absent FactoryFunc
	if _, err := absent.NewOutputAuthority(Config{}); !errors.Is(err, ErrContract) {
		t.Fatalf("absent factory error=%v", err)
	}
	cause := errors.New("document grant expired")
	called := false
	factory := FactoryFunc(func(Config) (Authority, error) {
		called = true
		return nil, cause
	})
	if _, err := factory.NewOutputAuthority(Config{}); !called || !errors.Is(err, cause) {
		t.Fatalf("provider failure replaced: %v", err)
	}
}
