package connector

import "testing"

func TestCloudSyncCountersHasChanges(t *testing.T) {
	for _, tc := range []struct {
		name   string
		counts cloudSyncCounters
		want   bool
	}{
		{"empty", cloudSyncCounters{}, false},
		{"skipped only", cloudSyncCounters{Skipped: 1}, false},
		{"filtered only", cloudSyncCounters{Filtered: 1}, false},
		{"imported", cloudSyncCounters{Imported: 1}, true},
		{"updated", cloudSyncCounters{Updated: 1}, true},
		{"deleted", cloudSyncCounters{Deleted: 1}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.counts.hasChanges(); got != tc.want {
				t.Errorf("hasChanges() = %v, want %v", got, tc.want)
			}
		})
	}
}
