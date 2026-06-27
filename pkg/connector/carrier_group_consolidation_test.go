package connector

import (
	"reflect"
	"testing"
)

func TestPlanCarrierGroupConsolidation(t *testing.T) {
	comma := "tel:+15551111111,tel:+15552222222"
	commaB := "tel:+15553333333,tel:+15554444444"

	tests := []struct {
		name    string
		entries []carrierConsolidationEntry
		want    []carrierConsolidationGroup
	}{
		{
			name: "multiple gid encodings collapse to one canonical group",
			entries: []carrierConsolidationEntry{
				{portalID: "gid:aaaa", canonical: comma},
				{portalID: "gid:bbbb", canonical: comma},
				{portalID: "gid:cccc", canonical: comma},
			},
			want: []carrierConsolidationGroup{
				{canonical: comma, members: []string{"gid:aaaa", "gid:bbbb", "gid:cccc"}},
			},
		},
		{
			name: "single gid still re-keyed to participant form",
			entries: []carrierConsolidationEntry{
				{portalID: "gid:aaaa", canonical: comma},
			},
			want: []carrierConsolidationGroup{
				{canonical: comma, members: []string{"gid:aaaa"}},
			},
		},
		{
			name: "already-canonical single portal is a no-op",
			entries: []carrierConsolidationEntry{
				{portalID: comma, canonical: comma},
			},
			want: nil,
		},
		{
			name: "comma survivor plus gid duplicate still consolidates",
			entries: []carrierConsolidationEntry{
				{portalID: comma, canonical: comma},
				{portalID: "gid:aaaa", canonical: comma},
			},
			want: []carrierConsolidationGroup{
				{canonical: comma, members: []string{"gid:aaaa", comma}},
			},
		},
		{
			name: "distinct participant sets stay separate",
			entries: []carrierConsolidationEntry{
				{portalID: "gid:aaaa", canonical: comma},
				{portalID: "gid:bbbb", canonical: comma},
				{portalID: "gid:cccc", canonical: commaB},
				{portalID: "gid:dddd", canonical: commaB},
			},
			want: []carrierConsolidationGroup{
				{canonical: comma, members: []string{"gid:aaaa", "gid:bbbb"}},
				{canonical: commaB, members: []string{"gid:cccc", "gid:dddd"}},
			},
		},
		{
			name: "duplicate portal IDs are de-duplicated within a group",
			entries: []carrierConsolidationEntry{
				{portalID: "gid:aaaa", canonical: comma},
				{portalID: "gid:aaaa", canonical: comma},
				{portalID: "gid:bbbb", canonical: comma},
			},
			want: []carrierConsolidationGroup{
				{canonical: comma, members: []string{"gid:aaaa", "gid:bbbb"}},
			},
		},
		{
			name: "entries with empty canonical or portal are ignored",
			entries: []carrierConsolidationEntry{
				{portalID: "gid:aaaa", canonical: ""},
				{portalID: "", canonical: comma},
			},
			want: nil,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := planCarrierGroupConsolidation(tt.entries)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("planCarrierGroupConsolidation() = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestPlanCarrierRoomMoves(t *testing.T) {
	const comma = "tel:+15551111111,tel:+15552222222"

	tests := []struct {
		name             string
		canonicalHasRoom bool
		members          []carrierMemberRoom
		want             carrierRoomMovePlan
	}{
		{
			// Canonical key already has a room: it must survive, and even a larger
			// gid: room tombstones into it (ReIDPortal would otherwise tombstone the
			// larger room — the bug this guards against).
			name:             "existing canonical room survives over a larger member",
			canonicalHasRoom: true,
			members: []carrierMemberRoom{
				{portalID: "gid:aaaa", hasRoom: true, msgCount: 9999},
			},
			want: carrierRoomMovePlan{survivor: comma, reIDs: []string{"gid:aaaa"}},
		},
		{
			name:             "no canonical room: most-history member is renamed onto canonical first",
			canonicalHasRoom: false,
			members: []carrierMemberRoom{
				{portalID: "gid:aaaa", hasRoom: true, msgCount: 10},
				{portalID: "gid:bbbb", hasRoom: true, msgCount: 50},
				{portalID: "gid:cccc", hasRoom: true, msgCount: 20},
			},
			want: carrierRoomMovePlan{survivor: "gid:bbbb", reIDs: []string{"gid:bbbb", "gid:aaaa", "gid:cccc"}},
		},
		{
			name:             "members without rooms are skipped",
			canonicalHasRoom: false,
			members: []carrierMemberRoom{
				{portalID: "gid:aaaa", hasRoom: false},
				{portalID: "gid:bbbb", hasRoom: true, msgCount: 5},
				{portalID: "gid:cccc", hasRoom: false},
			},
			want: carrierRoomMovePlan{survivor: "gid:bbbb", reIDs: []string{"gid:bbbb"}},
		},
		{
			name:             "no member has a room: nothing to move, createPortals builds it",
			canonicalHasRoom: false,
			members: []carrierMemberRoom{
				{portalID: "gid:aaaa", hasRoom: false},
				{portalID: "gid:bbbb", hasRoom: false},
			},
			want: carrierRoomMovePlan{survivor: "", reIDs: nil},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := planCarrierRoomMoves(comma, tt.canonicalHasRoom, tt.members)
			if !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("planCarrierRoomMoves() = %#v, want %#v", got, tt.want)
			}
		})
	}
}
