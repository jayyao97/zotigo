package agent

import "testing"

func TestSpawnParallelEligibility(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     string
		decision ExecutionDecision
		risk     string
		snapshot bool
		want     bool
	}{
		{"default general", "{}", ExecutionDecisionRequireApproval, "low", false, false},
		{"default explore", `{"agent_type":"explore"}`, ExecutionDecisionAutoExecute, "safe", false, false},
		{"default approved explore", `{"agent_type":"explore"}`, ExecutionDecisionRequireApproval, "safe", false, false},
		{"serial explore", `{"agent_type":"explore","allow_parallel":false}`, ExecutionDecisionAutoExecute, "safe", false, false},
		{"approved serial explore", `{"agent_type":"explore","allow_parallel":false}`, ExecutionDecisionRequireApproval, "safe", false, false},
		{"independent general", `{"allow_parallel":true}`, ExecutionDecisionAutoExecute, "low", false, true},
		{"blocked", `{"allow_parallel":true}`, ExecutionDecisionBlock, "low", false, false},
		{"snapshot", `{"allow_parallel":true}`, ExecutionDecisionRequireApproval, "low", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			action := &PendingAction{Name: "spawn", Arguments: tc.args, Decision: ActionDecision{
				Decision: tc.decision, RiskLevel: tc.risk, RequiresSnapshot: tc.snapshot,
			}}
			if got := canRunActionConcurrently(action); got != tc.want {
				t.Errorf("canRunActionConcurrently = %v, want %v", got, tc.want)
			}
		})
	}
}
