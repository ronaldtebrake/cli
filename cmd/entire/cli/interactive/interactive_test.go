package interactive

import (
	"bytes"
	"os"
	"testing"
)

func TestCanPromptInteractively_ForcedOn(t *testing.T) {
	t.Setenv(EnvTestTTY, "1")
	if !CanPromptInteractively() {
		t.Errorf("CanPromptInteractively() = false; want true when %s=1", EnvTestTTY)
	}
}

func TestCanPromptInteractively_ForcedOff(t *testing.T) {
	t.Setenv(EnvTestTTY, "0")
	if CanPromptInteractively() {
		t.Errorf("CanPromptInteractively() = true; want false when %s=0", EnvTestTTY)
	}
}

// Under `go test` without an explicit override, testing.Testing() short-circuits
// to non-interactive.
func TestCanPromptInteractively_TestingDefaultsOff(t *testing.T) {
	t.Setenv(EnvTestTTY, "")
	if CanPromptInteractively() {
		t.Error("CanPromptInteractively() = true; want false under testing.Testing()")
	}
}

// CI=false is the `is-ci` escape hatch: a dev may set it to override an
// inherited CI=true. With EnvTestTTY=1 standing in for a real TTY, the gate
// must not short-circuit to false.
func TestCanPromptInteractively_CIFalseOverride(t *testing.T) {
	t.Setenv("CI", "false")
	t.Setenv(EnvTestTTY, "1")
	if !CanPromptInteractively() {
		t.Error("CanPromptInteractively() = false; want true when CI=false")
	}
}

func TestIsAgentSubprocessEnv(t *testing.T) {
	cases := []struct {
		name, key, val string
	}{
		{"gemini", "GEMINI_CLI", "1"},
		{"copilot", "COPILOT_CLI", "1"},
		{"pi", "PI_CODING_AGENT", "true"},
		{"git-terminal-prompt-off", "GIT_TERMINAL_PROMPT", "0"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			t.Setenv(c.key, c.val)
			if !isAgentSubprocessEnv() {
				t.Errorf("isAgentSubprocessEnv() = false; want true when %s=%s", c.key, c.val)
			}
		})
	}
}

// GIT_TERMINAL_PROMPT only counts when explicitly set to "0". Other values
// (or absence) shouldn't trigger the guard.
func TestIsAgentSubprocessEnv_GitTerminalPromptOnIsNotAgent(t *testing.T) {
	t.Setenv("GIT_TERMINAL_PROMPT", "1")
	if isAgentSubprocessEnv() {
		t.Error("isAgentSubprocessEnv() = true; want false when GIT_TERMINAL_PROMPT=1")
	}
}

func TestUnderTest_TrueByTestingHarness(t *testing.T) {
	t.Setenv(EnvTestTTY, "")
	if !UnderTest() {
		t.Error("UnderTest() = false; want true under testing.Testing()")
	}
}

func TestIsTerminalWriter_NonFile(t *testing.T) {
	t.Parallel()
	if IsTerminalWriter(&bytes.Buffer{}) {
		t.Error("IsTerminalWriter(*bytes.Buffer) = true; want false")
	}
}

func TestIsTerminalWriter_Pipe(t *testing.T) {
	t.Parallel()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe: %v", err)
	}
	defer r.Close()
	defer w.Close()
	if IsTerminalWriter(w) {
		t.Error("IsTerminalWriter(pipe) = true; want false")
	}
}

func TestTermLacksANSI(t *testing.T) {
	cases := []struct {
		term string
		want bool
	}{
		{"cygwin", true},
		{"xterm-256color", false},
		{"dumb", false},
		{"", false},
	}
	for _, c := range cases {
		t.Run(c.term, func(t *testing.T) {
			t.Setenv("TERM", c.term)
			if got := termLacksANSI(); got != c.want {
				t.Errorf("termLacksANSI() with TERM=%q = %v; want %v", c.term, got, c.want)
			}
		})
	}
}

// TestShouldStyle exercises the disabling gates; the enabling path needs a
// real terminal writer, which `go test` doesn't provide.
func TestShouldStyle(t *testing.T) {
	t.Run("NO_COLOR disables", func(t *testing.T) {
		t.Setenv("NO_COLOR", "1")
		if ShouldStyle(os.Stdout) {
			t.Error("ShouldStyle(os.Stdout) = true with NO_COLOR set; want false")
		}
	})
	t.Run("TERM=cygwin disables", func(t *testing.T) {
		t.Setenv("TERM", "cygwin")
		if ShouldStyle(os.Stdout) {
			t.Error("ShouldStyle(os.Stdout) = true with TERM=cygwin; want false")
		}
	})
	t.Run("non-terminal writer disables", func(t *testing.T) {
		t.Setenv("TERM", "xterm-256color")
		if ShouldStyle(&bytes.Buffer{}) {
			t.Error("ShouldStyle(*bytes.Buffer) = true; want false")
		}
	})
}
