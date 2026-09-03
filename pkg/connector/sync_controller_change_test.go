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
		// A filtered chat is still a written row, and a portal candidate when
		// bridge_filtered_chats is on.
		{"filtered only", cloudSyncCounters{Filtered: 1}, true},
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

func TestCanSkipDelayedCloudReconciliation(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		counts                      cloudSyncCounters
		previousPassFailed          bool
		portalReconciliationPending bool
		want                        bool
	}{
		{"empty after complete reconciliation", cloudSyncCounters{}, false, false, true},
		{"CloudKit changes", cloudSyncCounters{Imported: 1}, false, false, false},
		{"previous CloudKit pass failed", cloudSyncCounters{}, true, false, false},
		{"portal reconciliation pending", cloudSyncCounters{}, false, true, false},
		{"both failures pending", cloudSyncCounters{}, true, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := canSkipDelayedCloudReconciliation(tc.counts, tc.previousPassFailed, tc.portalReconciliationPending); got != tc.want {
				t.Errorf("canSkipDelayedCloudReconciliation() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestShouldRunDelayedCloudReconciliation(t *testing.T) {
	for _, tc := range []struct {
		name                        string
		counts                      cloudSyncCounters
		currentPassFailed           bool
		previousPassFailed          bool
		portalReconciliationPending bool
		want                        bool
	}{
		{"successful empty pass", cloudSyncCounters{}, false, false, false, false},
		{"current pass failed after partial writes", cloudSyncCounters{}, true, false, false, true},
		{"current failed pass reported writes", cloudSyncCounters{Imported: 1}, true, false, false, true},
		{"previous pass failed", cloudSyncCounters{}, false, true, false, true},
		{"portal scan pending", cloudSyncCounters{}, false, false, true, true},
		{"successful pass wrote changes", cloudSyncCounters{Updated: 1}, false, false, false, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldRunDelayedCloudReconciliation(tc.counts, tc.currentPassFailed, tc.previousPassFailed, tc.portalReconciliationPending); got != tc.want {
				t.Errorf("shouldRunDelayedCloudReconciliation() = %v, want %v", got, tc.want)
			}
		})
	}
}
