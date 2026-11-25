package turnissuer

import (
	"reflect"
	"slices"
	"testing"

	p "github.com/windshare/windshare/core/session/protocolsession"
	"github.com/windshare/windshare/core/session/turngrant"
	"github.com/windshare/windshare/engine/relayconfig"
)

func testSource(id, role string) source {
	return source{TURNService: relayconfig.TURNService{ID: id, Kind: relayconfig.TURNStatic, Role: role, URLs: []string{"turn:local"}, Username: "user", Password: "fixture"}}
}

func sourceIDs(services []source) []string {
	ids := make([]string, len(services))
	for i, s := range services {
		ids[i] = s.ID
	}
	return ids
}

func TestActiveServicesDoesNotMutateAvailable(t *testing.T) {
	full := []source{
		testSource("p1", relayconfig.RoleParallel),
		testSource("p2", relayconfig.RoleParallel),
		testSource("p3", relayconfig.RoleParallel),
		testSource("p4", relayconfig.RoleParallel),
		testSource("fb", relayconfig.RoleFallback),
		testSource("u1", relayconfig.RoleFallback),
	}
	short := []source{
		testSource("p1", relayconfig.RoleParallel),
		testSource("fb", relayconfig.RoleFallback),
		testSource("p2", relayconfig.RoleParallel),
	}
	unknown := map[string]bool{"u1": true}

	for _, tc := range []struct {
		name      string
		available []source
		request   turngrant.IssueRequest
		want      []string
	}{
		{
			name:      "service_ids_filter",
			available: full,
			request:   turngrant.IssueRequest{Mode: p.TURNAll, ServiceIDs: []string{"p2", "fb"}},
			want:      []string{"p2", "fb"},
		},
		{
			name:      "opposite_role_slot_reservation",
			available: full,
			request:   turngrant.IssueRequest{Mode: p.TURNAll},
			want:      []string{"p1", "p2", "p3", "fb"},
		},
		{
			name:      "parallel_mode_after_role_reservation",
			available: full,
			request:   turngrant.IssueRequest{Mode: p.TURNParallel},
			want:      []string{"p1", "p2", "p3"},
		},
		{
			name:      "parallel_mode_within_grant_limit",
			available: short,
			request:   turngrant.IssueRequest{Mode: p.TURNParallel},
			want:      []string{"p1", "p2"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			available := slices.Clone(tc.available)
			snapshot := slices.Clone(available)
			got := activeServices(available, unknown, tc.request)
			if !slices.Equal(sourceIDs(got), tc.want) {
				t.Fatalf("activeServices() = %v, want %v", sourceIDs(got), tc.want)
			}
			if !reflect.DeepEqual(available, snapshot) {
				t.Fatalf("available mutated in place: got %v, want %v", sourceIDs(available), sourceIDs(snapshot))
			}
		})
	}
}
