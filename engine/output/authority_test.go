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
	if (Operation{}).Valid() {
		t.Fatal("empty output state accepted")
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

func TestNameAdjustmentSummaryValidationAndClone(t *testing.T) {
	if !(NameAdjustmentSummary{}).Empty() || !(NameAdjustmentSummary{}).Valid() {
		t.Fatal("zero summary must be empty and valid")
	}
	valid := NameAdjustmentSummary{
		Count: 25,
		Samples: []NameAdjustment{
			{LogicalPath: "CON.txt", PhysicalPath: "entry-CON.windshare-01.txt"},
		},
	}
	if valid.Empty() || !valid.Valid() {
		t.Fatalf("valid summary rejected: %+v", valid)
	}
	cloned := valid.Clone()
	cloned.Samples[0].LogicalPath = "mutated"
	if valid.Samples[0].LogicalPath != "CON.txt" {
		t.Fatal("Clone shared sample backing slice")
	}

	tooMany := make([]NameAdjustment, MaxNameAdjustmentSamples+1)
	for i := range tooMany {
		tooMany[i] = NameAdjustment{LogicalPath: "a", PhysicalPath: "b"}
	}
	for _, invalid := range []NameAdjustmentSummary{
		{Count: 0, Samples: []NameAdjustment{{LogicalPath: "a", PhysicalPath: "b"}}},
		{Count: 1},
		{Count: 1, Samples: []NameAdjustment{{LogicalPath: "a", PhysicalPath: "b"}, {LogicalPath: "c", PhysicalPath: "d"}}},
		{Count: uint64(len(tooMany)), Samples: tooMany},
		{Count: 1, Samples: []NameAdjustment{{LogicalPath: "", PhysicalPath: "b"}}},
		{Count: 1, Samples: []NameAdjustment{{LogicalPath: "a", PhysicalPath: ""}}},
		{Count: 1, Samples: []NameAdjustment{{LogicalPath: "same", PhysicalPath: "same"}}},
	} {
		if invalid.Valid() {
			t.Fatalf("invalid summary accepted: %+v", invalid)
		}
	}
}
