package agent

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/entireio/cli/cmd/entire/cli/agent/types"
)

// ForegroundCommandSpec describes a command Entire can launch in the caller's
// terminal without going through a shell.
type ForegroundCommandSpec struct {
	Binary string
	Args   []string
}

// ResumeCommandSpecFor returns the foreground command shape for agents whose
// resume command is safe for Entire to launch directly. Agents not listed here
// still expose FormatResumeCommand for print-only resume instructions.
func ResumeCommandSpecFor(name types.AgentName, sessionID string) (ForegroundCommandSpec, bool) {
	sessionID = strings.TrimSpace(sessionID)
	switch name {
	case AgentNameClaudeCode:
		if sessionID == "" {
			return ForegroundCommandSpec{}, false
		}
		return ForegroundCommandSpec{Binary: "claude", Args: []string{"-r", sessionID}}, true
	case AgentNameCodex:
		if sessionID == "" {
			return ForegroundCommandSpec{}, false
		}
		return ForegroundCommandSpec{Binary: "codex", Args: []string{"resume", sessionID}}, true
	case AgentNameCopilotCLI:
		if sessionID == "" {
			return ForegroundCommandSpec{}, false
		}
		return ForegroundCommandSpec{Binary: "copilot", Args: []string{"--resume", sessionID}}, true
	case AgentNameFactoryAIDroid:
		if sessionID == "" {
			return ForegroundCommandSpec{}, false
		}
		return ForegroundCommandSpec{Binary: "droid", Args: []string{"--session-id", sessionID}}, true
	case AgentNameGemini:
		if sessionID == "" {
			return ForegroundCommandSpec{}, false
		}
		return ForegroundCommandSpec{Binary: "gemini", Args: []string{"--resume", sessionID}}, true
	case AgentNameOpenCode:
		if sessionID == "" {
			return ForegroundCommandSpec{Binary: "opencode"}, true
		}
		return ForegroundCommandSpec{Binary: "opencode", Args: []string{"-s", sessionID}}, true
	case AgentNamePi:
		if sessionID == "" {
			return ForegroundCommandSpec{Binary: "pi", Args: []string{"--continue"}}, true
		}
		return ForegroundCommandSpec{Binary: "pi", Args: []string{"--session", sessionID}}, true
	default:
		return ForegroundCommandSpec{}, false
	}
}

// NewResumeForegroundCommand builds a foreground command for resuming a session,
// when the agent has a launchable resume command. ok=false means callers should
// print FormatResumeCommand for the user instead.
func NewResumeForegroundCommand(ctx context.Context, name types.AgentName, sessionID string) (*exec.Cmd, bool, error) {
	spec, ok := ResumeCommandSpecFor(name, sessionID)
	if !ok {
		return nil, false, nil
	}
	cmd, err := NewForegroundCommand(ctx, spec.Binary, spec.Args...)
	if err != nil {
		return nil, true, fmt.Errorf("build %s resume command: %w", spec.Binary, err)
	}
	return cmd, true, nil
}
