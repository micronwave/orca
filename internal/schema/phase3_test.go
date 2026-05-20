package schema

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestPhase3SchemaJSONNames(t *testing.T) {
	recordedAt := time.Date(2026, 5, 19, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		v    any
		want []string
	}{
		{
			name: "claim artifact dispute fields",
			v: ClaimArtifact{
				ClaimID:              "CL-1",
				Status:               ClaimContested,
				LastValidatedAgainst: "SNAP-1",
				ContradictedBy:       []string{"CL-2"},
				InvalidatedBy:        []string{"CL-3"},
			},
			want: []string{`"last_validated_against":"SNAP-1"`, `"contradicted_by":["CL-2"]`, `"invalidated_by":["CL-3"]`},
		},
		{
			name: "sidecar claim dispute fields",
			v: SidecarClaim{
				Claim:       "old claim",
				Type:        SidecarClaimProposed,
				Contradicts: []string{"CL-2"},
				Invalidates: []string{"CL-3"},
			},
			want: []string{`"contradicts":["CL-2"]`, `"invalidates":["CL-3"]`},
		},
		{
			name: "evidence reuse fields",
			v: EvidenceArtifact{
				EvidenceID:       "EV-1",
				Type:             EvidenceTestResult,
				ContentHash:      "sha256:abc",
				ReuseKey:         "go-test",
				ValidatedAgainst: "SNAP-1",
				ReusedFromID:     "EV-0",
				CreatedAt:        recordedAt,
			},
			want: []string{`"content_hash":"sha256:abc"`, `"reuse_key":"go-test"`, `"validated_against":"SNAP-1"`, `"reused_from_id":"EV-0"`},
		},
		{
			name: "failure recurrence fields",
			v: FailureFingerprint{
				FailureID:             "FAIL-1",
				PriorAttemptCount:     2,
				PriorCapsuleIDs:       []string{"CAP-1", "CAP-2"},
				RecommendedNextAction: "run focused test",
			},
			want: []string{`"prior_attempt_count":2`, `"prior_capsule_ids":["CAP-1","CAP-2"]`, `"recommended_next_action":"run focused test"`},
		},
		{
			name: "topology outcome fields",
			v: TopologyOutcomeRecord{
				OutcomeID:       "TO-1",
				GoalID:          "G-1",
				Topology:        TopologySingle,
				ObligationCount: 2,
				MaxRiskLevel:    RiskLow,
				RecordedAt:      recordedAt,
			},
			want: []string{`"outcome_id":"TO-1"`, `"goal_id":"G-1"`, `"topology":"single"`, `"obligation_count":2`, `"max_risk_level":"low"`, `"recorded_at":"2026-05-19T12:00:00Z"`},
		},
		{
			name: "claim status payload phase 3 fields",
			v: ClaimStatusPayload{
				ClaimID:              "CL-1",
				Status:               ClaimInvalidated,
				LastValidatedAgainst: "SNAP-1",
				ContradictedBy:       []string{"CL-2"},
				InvalidatedBy:        []string{"CL-3"},
			},
			want: []string{`"last_validated_against":"SNAP-1"`, `"contradicted_by":["CL-2"]`, `"invalidated_by":["CL-3"]`},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, err := json.Marshal(tc.v)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			got := string(data)
			for _, want := range tc.want {
				if !strings.Contains(got, want) {
					t.Fatalf("JSON %s missing %s", got, want)
				}
			}
		})
	}
}
