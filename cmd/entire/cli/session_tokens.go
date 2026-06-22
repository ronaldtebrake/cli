package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/bits"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent"
	"github.com/entireio/cli/cmd/entire/cli/strategy"
	"github.com/spf13/cobra"
)

type sessionTokensReport struct {
	SessionID       string                        `json:"session_id"`
	Agent           string                        `json:"agent"`
	Model           string                        `json:"model,omitempty"`
	Status          string                        `json:"status"`
	Source          string                        `json:"source"`
	Tokens          *sessionTokensUsage           `json:"tokens,omitempty"`
	Context         *sessionTokensContext         `json:"context,omitempty"`
	Contributors    []sessionTokensContributor    `json:"contributors,omitempty"`
	Recommendations []sessionTokensRecommendation `json:"recommendations,omitempty"`
	Limitations     []string                      `json:"limitations,omitempty"`
}

type sessionTokensUsage struct {
	Total         int `json:"total"`
	Input         int `json:"input"`
	CacheRead     int `json:"cache_read"`
	CacheWrite    int `json:"cache_write"`
	Output        int `json:"output"`
	APICalls      int `json:"api_calls"`
	SubagentTotal int `json:"subagent_total,omitempty"`
}

type sessionTokensContext struct {
	Tokens     int `json:"tokens"`
	WindowSize int `json:"window_size"`
	Percent    int `json:"percent"`
}

type sessionTokensContributor struct {
	Kind       string   `json:"kind"`
	Label      string   `json:"label"`
	Tokens     int      `json:"tokens,omitempty"`
	Percent    int      `json:"percent,omitempty"`
	Confidence string   `json:"confidence"`
	Signals    []string `json:"signals,omitempty"`
}

type sessionTokensRecommendation struct {
	ID       string   `json:"id"`
	Severity string   `json:"severity"`
	Message  string   `json:"message"`
	Signals  []string `json:"signals,omitempty"`
}

type tokenRecommendationSignals struct {
	Tokens          *sessionTokensUsage
	Context         *sessionTokensContext
	TurnCount       int
	CheckpointCount int
}

func newTokensCmd() *cobra.Command {
	var jsonFlag bool
	var currentFlag bool

	cmd := &cobra.Command{
		Use:   "tokens [session-id]",
		Short: "Show token usage and optimization recommendations for a session",
		Long: `Show token usage and optimization recommendations for a session.

When no session ID is provided, Entire reports on the most recently active
session, preferring the current worktree and falling back to the newest session
if no state matches this worktree. The report uses token and context data Entire
already captured for the session.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if currentFlag && len(args) > 0 {
				return errors.New("--current and session ID argument are mutually exclusive")
			}

			sessionID := ""
			if len(args) > 0 {
				sessionID = args[0]
			}
			return runSessionTokens(cmd.Context(), cmd, sessionID, currentFlag, jsonFlag)
		},
	}

	cmd.Flags().BoolVar(&jsonFlag, "json", false, "Output as JSON")
	cmd.Flags().BoolVar(&currentFlag, "current", false, "Prefer the current worktree's most recent session")
	return cmd
}

func runSessionTokens(ctx context.Context, cmd *cobra.Command, sessionID string, current, jsonOutput bool) error {
	if sessionID == "" || current {
		sessionID = strategy.FindMostRecentSession(ctx)
		if sessionID == "" {
			fmt.Fprintln(cmd.OutOrStdout(), "No active session found in this worktree.")
			return nil
		}
	}

	state, err := strategy.LoadSessionState(ctx, sessionID)
	if err != nil {
		return tokenCommandError(fmt.Errorf("failed to load session: %w", err))
	}
	if state == nil {
		cmd.SilenceUsage = true
		fmt.Fprintln(cmd.ErrOrStderr(), "Session not found.")
		return NewSilentError(fmt.Errorf("session not found: %s", sessionID))
	}

	report := buildSessionTokensReport(state, sessionPhaseLabel(state))
	if jsonOutput {
		return writeSessionTokensJSON(cmd.OutOrStdout(), report)
	}
	writeSessionTokensText(cmd.OutOrStdout(), report)
	return nil
}

func tokenCommandError(err error) error {
	if err == nil {
		return nil
	}
	var silent *SilentError
	if errors.As(err, &silent) {
		return err
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return NewSilentError(err)
	}
	return err
}

func buildSessionTokensReport(state *strategy.SessionState, status string) sessionTokensReport {
	agentLabel := string(state.AgentType)
	if agentLabel == "" {
		agentLabel = unknownPlaceholder
	}

	report := sessionTokensReport{
		SessionID: state.SessionID,
		Agent:     agentLabel,
		Model:     state.ModelName,
		Status:    status,
		Source:    "session_state",
	}

	if tokens := buildSessionTokensUsage(state.TokenUsage); tokens != nil {
		report.Tokens = tokens
		if tokens.SubagentTotal > 0 {
			report.Contributors = append(report.Contributors, sessionTokensContributor{
				Kind:       "subagents",
				Label:      "Subagents",
				Tokens:     tokens.SubagentTotal,
				Confidence: "reported",
				Signals:    []string{"subagent_tokens"},
			})
		}
	} else {
		report.Limitations = append(report.Limitations, "No token usage recorded for this session.")
		report.Recommendations = append(report.Recommendations, sessionTokensRecommendation{
			ID:       "no-token-data",
			Severity: "low",
			Message:  "Token usage is unavailable for this session; the agent may not expose token data yet, or no checkpoint has captured it.",
			Signals:  []string{"missing_token_usage"},
		})
	}

	if contextInfo := buildSessionTokensContext(state.ContextTokens, state.ContextWindowSize); contextInfo != nil {
		report.Context = contextInfo
		report.Contributors = append(report.Contributors, sessionTokensContributor{
			Kind:       "context_pressure",
			Label:      "Context pressure",
			Percent:    contextInfo.Percent,
			Confidence: "reported",
			Signals:    []string{"context_tokens"},
		})
	}

	if labels := skillEventLabels(state.SkillEvents); len(labels) > 0 {
		report.Contributors = append(report.Contributors, sessionTokensContributor{
			Kind:       "skills",
			Label:      "Skills/slash commands: " + strings.Join(labels, ", "),
			Confidence: "reported",
			Signals:    []string{"skill_events"},
		})
	}

	report.Recommendations = append(report.Recommendations, recommendationRules(tokenRecommendationSignals{
		Tokens:          report.Tokens,
		Context:         report.Context,
		TurnCount:       state.SessionTurnCount,
		CheckpointCount: state.StepCount,
	})...)
	return report
}

func buildSessionTokensUsage(usage *agent.TokenUsage) *sessionTokensUsage {
	if usage == nil {
		return nil
	}
	total := totalTokens(usage)
	if total == 0 && usage.APICallCount == 0 {
		return nil
	}
	return &sessionTokensUsage{
		Total:         total,
		Input:         usage.InputTokens,
		CacheRead:     usage.CacheReadTokens,
		CacheWrite:    usage.CacheCreationTokens,
		Output:        usage.OutputTokens,
		APICalls:      usage.APICallCount,
		SubagentTotal: totalTokens(usage.SubagentTokens),
	}
}

func buildSessionTokensContext(tokens, windowSize int) *sessionTokensContext {
	if tokens <= 0 || windowSize <= 0 {
		return nil
	}
	return &sessionTokensContext{
		Tokens:     tokens,
		WindowSize: windowSize,
		Percent:    roundedPercent(tokens, windowSize),
	}
}

func roundedPercent(value, total int) int {
	if total <= 0 {
		return 0
	}
	if value <= 0 {
		return 0
	}

	const maxPercent = 100

	hi, lo := bits.Mul64(uint64(value), maxPercent)
	lo, carry := bits.Add64(lo, uint64(total)/2, 0)
	hi += carry
	divisor := uint64(total)
	if hi >= divisor {
		return maxPercent
	}
	quotient, _ := bits.Div64(hi, lo, divisor)
	if quotient > maxPercent {
		return maxPercent
	}
	return int(quotient)
}

func recommendationRules(signals tokenRecommendationSignals) []sessionTokensRecommendation {
	var recs []sessionTokensRecommendation

	cacheReadHotspot := false
	if signals.Tokens != nil && signals.Tokens.Total > 0 && signals.Tokens.CacheRead > 0 {
		cacheReadPercent := tokenPercent(signals.Tokens.CacheRead, signals.Tokens.Total)
		if cacheReadPercent >= 80 {
			cacheReadHotspot = true
			recs = append(recs, sessionTokensRecommendation{
				ID:       "context-replay-hotspot",
				Severity: "high",
				Message: fmt.Sprintf(
					"Cache/context replay is %s of token volume; reduce unnecessary follow-up calls in this large-context session.",
					formatPercent(cacheReadPercent),
				),
				Signals: []string{"cache_read_tokens"},
			})
		}
	}
	if signals.Tokens != nil && signals.Tokens.APICalls >= 20 {
		message := fmt.Sprintf("API call count is high for one session: %d calls. Batch the next diagnosis and reduce iterative calls.", signals.Tokens.APICalls)
		if cacheReadHotspot {
			message = fmt.Sprintf("Large context was replayed across %d API calls; batch the next diagnosis and reduce iterative tool calls.", signals.Tokens.APICalls)
		}
		recs = append(recs, sessionTokensRecommendation{
			ID:       "api-call-amplification",
			Severity: "medium",
			Message:  message,
			Signals:  []string{"api_call_count"},
		})
	}
	if signals.Tokens != nil && signals.Tokens.SubagentTotal > 0 && signals.Tokens.SubagentTotal*100 >= signals.Tokens.Total*10 {
		recs = append(recs, sessionTokensRecommendation{
			ID:       "subagent-heavy",
			Severity: "medium",
			Message:  "Scope subagent tasks tightly; give each subagent a narrow objective and expected output.",
			Signals:  []string{"subagent_tokens"},
		})
	}
	if signals.Context != nil && signals.Context.Percent >= 80 {
		recs = append(recs, sessionTokensRecommendation{
			ID:       "high-context-pressure",
			Severity: "medium",
			Message:  fmt.Sprintf("Context pressure is %d%% of the window; preserve only relevant context before continuing.", signals.Context.Percent),
			Signals:  []string{"context_tokens"},
		})
	}
	if cacheReadHotspot && signals.Tokens != nil && signals.Tokens.APICalls >= 20 {
		recs = append(recs, sessionTokensRecommendation{
			ID:       "summarize-before-boundary",
			Severity: "low",
			Message:  "Compact or restart after summarizing this investigation; do not discard useful findings just because cache read is high.",
			Signals:  []string{"cache_read_tokens", "api_call_count"},
		})
	}
	if signals.TurnCount >= 10 || signals.CheckpointCount >= 5 {
		recs = append(recs, sessionTokensRecommendation{
			ID:       "long-session",
			Severity: "low",
			Message:  "Compact or restart after summarizing the useful findings if older context is no longer needed.",
			Signals:  []string{"turn_count", "checkpoint_count"},
		})
	}

	return recs
}

func tokenPercent(value, total int) float64 {
	if total <= 0 {
		return 0
	}
	return float64(value) * 100 / float64(total)
}

func formatPercent(percent float64) string {
	formatted := fmt.Sprintf("%.1f", percent)
	formatted = strings.TrimSuffix(formatted, ".0")
	return formatted + "%"
}

func skillEventLabels(events []agent.SkillEvent) []string {
	seen := make(map[string]struct{}, len(events))
	labels := make([]string, 0, len(events))
	for _, event := range events {
		label := event.Collapse.Label
		if label == "" && event.Native != nil {
			label = event.Native["command"]
		}
		if label == "" {
			label = event.Skill.Name
		}
		if label == "" {
			continue
		}
		if _, ok := seen[label]; ok {
			continue
		}
		seen[label] = struct{}{}
		labels = append(labels, label)
	}
	return labels
}

func writeSessionTokensJSON(w io.Writer, report sessionTokensReport) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	if err := enc.Encode(report); err != nil {
		return fmt.Errorf("failed to encode session token report: %w", err)
	}
	return nil
}

func writeSessionTokensText(w io.Writer, report sessionTokensReport) {
	fmt.Fprintln(w, "Session tokens")
	fmt.Fprintln(w)
	fmt.Fprintf(w, "Session: %s\n", report.SessionID)
	fmt.Fprintf(w, "Agent:   %s\n", report.Agent)
	if report.Model != "" {
		fmt.Fprintf(w, "Model:   %s\n", report.Model)
	}
	fmt.Fprintf(w, "Status:  %s\n", report.Status)

	writeTokenUsageSection(w, report.Tokens)
	if len(report.Recommendations) > 0 {
		writeTokenRecommendations(w, report.Recommendations)
	}

	writeTokenContributors(w, report.Contributors, report.Context)
	writeTokenLimitations(w, report.Limitations)
}

func writeTokenRecommendations(w io.Writer, recs []sessionTokensRecommendation) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Recommendations")
	for _, rec := range recs {
		fmt.Fprintf(w, "- %s\n", rec.Message)
	}
}

func writeTokenUsageSection(w io.Writer, tokens *sessionTokensUsage) {
	fmt.Fprintln(w)
	fmt.Fprintln(w, "Token usage")
	if tokens != nil {
		fmt.Fprintf(w, "Total:  %s tokens\n", formatTokenCount(tokens.Total))
		parts := []string{
			"Input: " + formatTokenCount(tokens.Input),
			"Cache read: " + formatTokenCount(tokens.CacheRead),
			"Cache write: " + formatTokenCount(tokens.CacheWrite),
			"Output: " + formatTokenCount(tokens.Output),
			fmt.Sprintf("API calls: %d", tokens.APICalls),
		}
		fmt.Fprintf(w, "  %s\n", strings.Join(parts, " | "))
	} else {
		fmt.Fprintln(w, "Token data: unavailable")
	}
}

func writeTokenContributors(w io.Writer, contributors []sessionTokensContributor, contextInfo *sessionTokensContext) {
	if len(contributors) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Likely contributors")
		for _, contributor := range contributors {
			switch contributor.Kind {
			case "subagents":
				fmt.Fprintf(w, "- %s: %s tokens\n", contributor.Label, formatTokenCount(contributor.Tokens))
			case "context_pressure":
				if contextInfo != nil {
					fmt.Fprintf(w, "- %s: %d%% of %s tokens\n", contributor.Label, contextInfo.Percent, formatTokenCount(contextInfo.WindowSize))
				}
			default:
				fmt.Fprintf(w, "- %s\n", contributor.Label)
			}
		}
	}
}

func writeTokenLimitations(w io.Writer, limitations []string) {
	if len(limitations) > 0 {
		fmt.Fprintln(w)
		fmt.Fprintln(w, "Limitations")
		for _, limitation := range limitations {
			fmt.Fprintf(w, "- %s\n", limitation)
		}
	}
}
