package transfer

import (
	"testing"

	"github.com/windshare/windshare/core/transfer/fault"
)

func TestFaultScopeProjectionPreservesEveryTypedDomain(t *testing.T) {
	tests := []struct {
		name  string
		value fault.Fault
		want  fault.Fault
	}{
		{
			name:  "source",
			value: mustSourceFault(fault.ScopeFileLocal, fault.SourceRevisionChanged),
			want:  mustSourceFault(fault.ScopeOutputPause, fault.SourceRevisionChanged),
		},
		{
			name:  "catalog",
			value: mustCatalogFault(fault.ScopeDirectoryLocal, fault.CatalogDirectoryStale),
			want:  mustCatalogFault(fault.ScopeOutputPause, fault.CatalogDirectoryStale),
		},
		{
			name:  "session",
			value: mustSessionFault(fault.ScopeFileLocal, fault.SessionTransport),
			want:  mustSessionFault(fault.ScopeOutputPause, fault.SessionTransport),
		},
		{
			name: "output",
			value: func() fault.Fault {
				value, _ := fault.NewOutput(fault.ScopeFileLocal, fault.OutputOwnership)
				return value
			}(),
			want: func() fault.Fault {
				value, _ := fault.NewOutput(fault.ScopeOutputPause, fault.OutputOwnership)
				return value
			}(),
		},
		{
			name: "checkpoint",
			value: func() fault.Fault {
				value, _ := fault.NewCheckpoint(fault.ScopeFileLocal, fault.CheckpointStateIO)
				return value
			}(),
			want: func() fault.Fault {
				value, _ := fault.NewCheckpoint(fault.ScopeOutputPause, fault.CheckpointStateIO)
				return value
			}(),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if projected := faultWithScope(test.value, fault.ScopeOutputPause); projected != test.want {
				t.Fatalf("projected fault = %v, want %v", projected, test.want)
			}
		})
	}
	if projected := faultWithScope(fault.Fault{}, fault.ScopeOutputPause); projected != fault.DependencyContractFault() {
		t.Fatalf("invalid fault projection = %v", projected)
	}
	invalidated := mustSourceFault(fault.ScopeFileLocal, fault.SourceRevisionInvalidated)
	changed := mustSourceFault(fault.ScopeFileLocal, fault.SourceRevisionChanged)
	if !(lifecyclePolicy{value: invalidated}).invalidatedRevision() ||
		(lifecyclePolicy{value: changed}).invalidatedRevision() ||
		(lifecyclePolicy{}).invalidatedRevision() {
		t.Fatal("revision invalidation policy escaped the typed source code")
	}
}
