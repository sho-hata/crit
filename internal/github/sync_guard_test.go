package github

import (
	"strings"
	"testing"
)

func TestGitHubSyncGuard(t *testing.T) {
	tests := []struct {
		name      string
		cj        CritJSON
		op        string
		wantError bool
	}{
		{"live review pull", CritJSON{ReviewType: "live", Origin: "http://localhost:3000"}, "crit pull", true},
		{"live review push", CritJSON{ReviewType: "live", Origin: "http://localhost:3000"}, "crit push", true},
		{"code review pull", CritJSON{ReviewRound: 1}, "crit pull", false},
		{"code review push", CritJSON{ReviewRound: 1}, "crit push", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkGitHubSyncAllowed(tt.cj, tt.op)
			if tt.wantError {
				if err == nil {
					t.Fatalf("expected error for %s on live review", tt.op)
				}
				if !strings.Contains(err.Error(), "live") {
					t.Errorf("error should mention live: %v", err)
				}
				if !strings.Contains(err.Error(), tt.op) {
					t.Errorf("error should mention op %q: %v", tt.op, err)
				}
			} else if err != nil {
				t.Errorf("code review should be allowed: %v", err)
			}
		})
	}
}
