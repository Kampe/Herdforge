package classify

// GatesFor returns the required review/integration gates for a tier.
// Order is stable for machine consumers.
func GatesFor(t Tier) []string {
	switch t {
	case TierR0:
		return []string{
			"deterministic_verification",
			"mechanical_review_optional",
		}
	case TierR1:
		return []string{
			"deterministic_verification",
			"different_family_review",
		}
	case TierR2:
		return []string{
			"deterministic_verification",
			"different_family_review",
			"integration_rerun",
		}
	case TierR3:
		return []string{
			"deterministic_verification",
			"different_family_review",
			"security_capable_review",
			"high_risk_explicit_gates",
		}
	default:
		// Unknown tier fails closed to R3 gates.
		return []string{
			"deterministic_verification",
			"different_family_review",
			"security_capable_review",
			"high_risk_explicit_gates",
		}
	}
}
