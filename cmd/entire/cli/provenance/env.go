// Package provenance owns the env-var contract that lets the lifecycle hook
// recognize a spawned agent process as part of `entire review` or `entire
// investigate`. Both spawn families set their own ENTIRE_*_* vars on the
// child agent process; the UserPromptSubmit hook reads them to tag the
// in-flight session with the right Kind and provenance metadata.
//
// Single source of truth for the names — review, investigate, and
// agentlaunch (which strips both families before spawning a fix agent) all
// reference this package.
//
// These names are stable API; renaming any constant is a breaking change.
package provenance

import "strings"

const (
	ReviewSession     = "ENTIRE_REVIEW_SESSION"
	ReviewAgent       = "ENTIRE_REVIEW_AGENT"
	ReviewSkills      = "ENTIRE_REVIEW_SKILLS"
	ReviewPrompt      = "ENTIRE_REVIEW_PROMPT"
	ReviewStartingSHA = "ENTIRE_REVIEW_STARTING_SHA"

	InvestigateSession     = "ENTIRE_INVESTIGATE_SESSION"
	InvestigateAgent       = "ENTIRE_INVESTIGATE_AGENT"
	InvestigateRunID       = "ENTIRE_INVESTIGATE_RUN_ID"
	InvestigateRound       = "ENTIRE_INVESTIGATE_ROUND"
	InvestigateTurn        = "ENTIRE_INVESTIGATE_TURN"
	InvestigateTopic       = "ENTIRE_INVESTIGATE_TOPIC"
	InvestigatePrompt      = "ENTIRE_INVESTIGATE_PROMPT"
	InvestigateFindingsDoc = "ENTIRE_INVESTIGATE_FINDINGS_DOC"
	InvestigateStateDoc    = "ENTIRE_INVESTIGATE_STATE_DOC"
	InvestigateStartingSHA = "ENTIRE_INVESTIGATE_STARTING_SHA"
)

var reviewPrefixes = []string{
	ReviewSession + "=",
	ReviewAgent + "=",
	ReviewSkills + "=",
	ReviewPrompt + "=",
	ReviewStartingSHA + "=",
}

var investigatePrefixes = []string{
	InvestigateSession + "=",
	InvestigateAgent + "=",
	InvestigateRunID + "=",
	InvestigateRound + "=",
	InvestigateTurn + "=",
	InvestigateTopic + "=",
	InvestigatePrompt + "=",
	InvestigateFindingsDoc + "=",
	InvestigateStateDoc + "=",
	InvestigateStartingSHA + "=",
}

// IsReviewEntry reports whether kv is a "KEY=VALUE" entry whose key is one
// of the ENTIRE_REVIEW_* contract variables.
func IsReviewEntry(kv string) bool {
	return hasAnyPrefix(kv, reviewPrefixes)
}

// IsInvestigateEntry reports whether kv is a "KEY=VALUE" entry whose key is
// one of the ENTIRE_INVESTIGATE_* contract variables.
func IsInvestigateEntry(kv string) bool {
	return hasAnyPrefix(kv, investigatePrefixes)
}

// IsEntry reports whether kv is a "KEY=VALUE" entry from either family.
// agentlaunch uses this to strip provenance markers before spawning a fix
// session so the child is not tagged as review or investigate.
func IsEntry(kv string) bool {
	return IsReviewEntry(kv) || IsInvestigateEntry(kv)
}

func hasAnyPrefix(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}
