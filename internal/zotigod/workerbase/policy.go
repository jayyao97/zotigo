package workerbase

import (
	"context"
	"fmt"
	"time"

	"github.com/jayyao97/zotigo/core/agent"
	zotigosession "github.com/jayyao97/zotigo/core/session"
)

type sessionApprovalPolicyUpdater interface {
	UpdateApprovalPolicy(ctx context.Context, id string, policy agent.ApprovalPolicy, updatedAt time.Time) error
}

func PersistSessionApprovalPolicy(ctx context.Context, store zotigosession.Store, sess *zotigosession.Session) error {
	if updater, ok := store.(sessionApprovalPolicyUpdater); ok {
		return updater.UpdateApprovalPolicy(ctx, sess.ID, sess.ApprovalPolicy, sess.UpdatedAt)
	}
	return store.Put(ctx, sess)
}

func NormalizeSessionApprovalPolicy(policy agent.ApprovalPolicy, allowEmpty bool) (agent.ApprovalPolicy, error) {
	if policy == "" && allowEmpty {
		return agent.ApprovalPolicyAuto, nil
	}
	switch policy {
	case agent.ApprovalPolicyManual, agent.ApprovalPolicyAuto, agent.ApprovalPolicyBypass:
		return policy, nil
	default:
		return "", fmt.Errorf("approval_policy must be %q, %q or %q", agent.ApprovalPolicyManual, agent.ApprovalPolicyAuto, agent.ApprovalPolicyBypass)
	}
}

// LowersPermissions reports whether moving from one policy to another
// grants the agent less autonomy (bypass > auto > manual). Workers apply a
// lowering change before persisting it so a persistence failure leaves the
// runtime on the safer policy.
func LowersPermissions(from, to agent.ApprovalPolicy) bool {
	rank := func(policy agent.ApprovalPolicy) int {
		switch policy {
		case agent.ApprovalPolicyManual:
			return 0
		case agent.ApprovalPolicyBypass:
			return 2
		default:
			return 1
		}
	}
	return rank(to) < rank(from)
}

type sessionProfileUpdater interface {
	UpdateProfile(ctx context.Context, id string, profileName string, updatedAt time.Time) error
}

func PersistSessionProfile(ctx context.Context, store zotigosession.Store, sess *zotigosession.Session) error {
	if updater, ok := store.(sessionProfileUpdater); ok {
		return updater.UpdateProfile(ctx, sess.ID, sess.ProfileName, sess.UpdatedAt)
	}
	return store.Put(ctx, sess)
}
