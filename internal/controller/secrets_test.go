package controller

import (
	"testing"
	"time"
)

func TestSecretDecision(t *testing.T) {
	now := time.Unix(1000, 0)
	settle := time.Minute
	cases := []struct {
		name    string
		known   bool
		old     string
		current string
		pending pendingSecret
		want    secretAction
	}{
		{"first seen", false, "", "v1", pendingSecret{}, secretRecord},
		{"unchanged", true, "v1", "v1", pendingSecret{}, secretUnchanged},
		{"just changed", true, "v1", "v2", pendingSecret{}, secretWait},
		{"changed, caches may still be warm", true, "v1", "v2", pendingSecret{"v2", now.Add(-30 * time.Second)}, secretWait},
		{"changed and settled", true, "v1", "v2", pendingSecret{"v2", now.Add(-time.Minute)}, secretRestart},
		{"changed again while waiting", true, "v1", "v3", pendingSecret{"v2", now.Add(-time.Hour)}, secretWait},
	}
	for _, tc := range cases {
		if got := secretDecision(tc.known, tc.old, tc.current, tc.pending, now, settle); got != tc.want {
			t.Errorf("%s: got %v, want %v", tc.name, got, tc.want)
		}
	}
	if got := secretDecision(true, "v1", "v2", pendingSecret{"v2", now}, now, 0); got != secretRestart {
		t.Errorf("without a plugin cache a change restarts right away, got %v", got)
	}
}
