package hook

import (
	"fmt"

	"github.com/feder-cr/open_scion/internal/gitaly/config"
)

// EvaluateGraftPolicy evaluates the standalone access policy for the update of reference from oldOID
// to newOID by actor. It applies the same rules as the reference-transaction hook: references
// outside refs/ and HEAD are not governed. When allowed is false, message holds the reason of the
// denial. The caller decides whether the policy applies to the update, for example only to agent
// callers, as the hook does.
func EvaluateGraftPolicy(cfg config.Graft, actor string, reference, oldOID, newOID string) (bool, string, error) {
	changes := fmt.Sprintf("%s %s %s\n", oldOID, newOID, reference)

	message, err := graftPolicyDenial(cfg.Policy, actor, []byte(changes))
	if err != nil {
		return false, "", fmt.Errorf("evaluating graft policy: %w", err)
	}

	return message == "", message, nil
}
