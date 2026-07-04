package connector

import (
	"strings"
	"testing"
)

// self is the test login's own handle; the carrier portal key always includes
// it (buildCanonicalParticipantList injects it). Chosen to sort first so the
// expected canonical strings are easy to read.
const (
	selfHandle = "tel:+15550000000"
	handleA    = "tel:+15551111111"
	handleB    = "tel:+15552222222"
	handleC    = "tel:+15553333333"
)

func testClient() *IMClient {
	return &IMClient{
		handle:     selfHandle,
		allHandles: []string{selfHandle},
	}
}

// TestCarrierGroupKeyConvergence is the load-bearing invariant of this whole
// change: the backfill path (resolvePortalIDForCloudChat) and the live path
// (makePortalKey's carrier branch) must derive the SAME portal key for a carrier
// group, regardless of participant ordering, whether self is in the roster, or
// which carrier service it is. If they diverge the group splits across rooms —
// the exact bug this PR fixes.
//
// resolvePortalIDForCloudChat's carrier branch returns before any DB access, so
// we exercise the real function. The live path's key is the same primitive
// (strings.Join(buildCanonicalParticipantList(...), ",")); we assert the backfill
// output equals it for every variant.
func TestCarrierGroupKeyConvergence(t *testing.T) {
	c := testClient()
	canonical := selfHandle + "," + handleA + "," + handleB // sorted self,A,B

	cases := []struct {
		name         string
		participants []string
		service      string
	}{
		{"self omitted (relayed roster), SMS", []string{handleA, handleB}, "SMS"},
		{"reordered participants", []string{handleB, handleA}, "SMS"},
		{"self present in roster", []string{selfHandle, handleA, handleB}, "SMS"},
		{"self present, reordered, duplicated", []string{handleB, selfHandle, handleA, handleA}, "SMS"},
		{"RCS service", []string{handleA, handleB}, "RCS"},
		{"MMS service", []string{handleA, handleB}, "MMS"},
		{"lowercase service", []string{handleA, handleB}, "sms"},
		{"service with whitespace", []string{handleA, handleB}, "  SMS "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Backfill path (real function; carrier branch is DB-free). style 43 = group.
			got := c.resolvePortalIDForCloudChat(tc.participants, nil, "some-unstable-gid", 43, tc.service)
			if got != canonical {
				t.Fatalf("backfill key = %q, want %q", got, canonical)
			}
			// Live path derives its carrier key from the same primitive.
			live := strings.Join(c.buildCanonicalParticipantList(tc.participants), ",")
			if got != live {
				t.Fatalf("backfill key %q != live primitive key %q — paths would split", got, live)
			}
		})
	}
}

// TestCarrierGroupKeyIgnoresUnstableGid pins that the carrier key does NOT depend
// on group_id: two records for the same participant set under different (unstable)
// gid encodings must collapse to one key. This is the root cause the PR addresses.
func TestCarrierGroupKeyIgnoresUnstableGid(t *testing.T) {
	c := testClient()
	parts := []string{handleA, handleB}
	k1 := c.resolvePortalIDForCloudChat(parts, nil, "gid-encoding-aaaa", 43, "SMS")
	k2 := c.resolvePortalIDForCloudChat(parts, nil, "GID-ENCODING-bbbb", 43, "SMS")
	if k1 != k2 {
		t.Fatalf("same carrier group under different gids produced %q and %q", k1, k2)
	}
	if !strings.Contains(k1, ",") || strings.HasPrefix(k1, "gid:") {
		t.Fatalf("carrier group key should be a participant (comma) key, got %q", k1)
	}
}

// TestNonCarrierGroupStillUsesGid guards that the carrier branch is correctly
// scoped: an iMessage group with a group_id must keep its gid: key, untouched.
func TestNonCarrierGroupStillUsesGid(t *testing.T) {
	c := testClient()
	got := c.resolvePortalIDForCloudChat([]string{handleA, handleB}, nil, "ABCD-1234", 43, "iMessage")
	if got != "gid:abcd-1234" {
		t.Fatalf("iMessage group key = %q, want gid:abcd-1234", got)
	}
}

// TestCountNonSelfMembers covers the group/DM routing predicate, including the
// relayed-carrier case where self is omitted and the reply arrives with fewer
// participants than the group really has.
func TestCountNonSelfMembers(t *testing.T) {
	c := testClient()
	s := func(v string) *string { return &v }

	cases := []struct {
		name         string
		participants []string
		sender       *string
		want         int
	}{
		{"two others, no sender", []string{handleA, handleB}, nil, 2},
		{"relayed group: sender duplicates a participant", []string{handleA, handleB}, s(handleA), 2},
		{"relayed group: one participant + distinct sender", []string{handleA}, s(handleB), 2},
		{"DM: one other + self in roster", []string{selfHandle, handleA}, s(handleA), 1},
		{"DM: sender equals the only participant", []string{handleA}, s(handleA), 1},
		{"order independent", []string{handleB, handleA, handleC}, nil, 3},
		{"all self collapses to zero", []string{selfHandle, selfHandle}, s(selfHandle), 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := c.countNonSelfMembers(tc.participants, tc.sender); got != tc.want {
				t.Fatalf("countNonSelfMembers(%v, %v) = %d, want %d", tc.participants, tc.sender, got, tc.want)
			}
		})
	}
}

func TestIsCarrierService(t *testing.T) {
	carrier := []string{"SMS", "RCS", "MMS", "sms", "  rcs ", "MmS"}
	for _, s := range carrier {
		if !isCarrierService(s) {
			t.Errorf("isCarrierService(%q) = false, want true", s)
		}
	}
	notCarrier := []string{"iMessage", "imessage", "", "  ", "FaceTime", "unknown"}
	for _, s := range notCarrier {
		if isCarrierService(s) {
			t.Errorf("isCarrierService(%q) = true, want false", s)
		}
	}
}
