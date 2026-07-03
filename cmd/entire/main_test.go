package main

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"testing"
)

// dieFromSignalEnvVar, when set on a re-executed test binary, makes TestMain
// invoke dieFromSignal with the named signal instead of running the test suite.
// The parent process then inspects how the child died. This is how
// TestDieFromSignal_TerminatesBySignal exercises real process death without
// killing the test runner itself.
const dieFromSignalEnvVar = "ENTIRE_TEST_DIE_FROM_SIGNAL"

func TestMain(m *testing.M) {
	// Child mode: exercise dieFromSignal and let it terminate this process by
	// the signal. dieFromSignal never returns on success; the os.Exit below is
	// only reached if the re-raise couldn't be delivered.
	switch os.Getenv(dieFromSignalEnvVar) {
	case "INT":
		dieFromSignal(os.Interrupt)
		os.Exit(exitCodeForSignal(os.Interrupt))
	case "TERM":
		dieFromSignal(syscall.SIGTERM)
		os.Exit(exitCodeForSignal(syscall.SIGTERM))
	}
	os.Exit(m.Run())
}

// nonNumericSignal is an os.Signal that isn't a syscall.Signal, exercising
// exitCodeForSignal's fallback branch.
type nonNumericSignal struct{}

func (nonNumericSignal) String() string { return "non-numeric" }
func (nonNumericSignal) Signal()        {}

// TestDieFromSignal_TerminatesBySignal is the regression guard for the headline
// behavior: an enclosing `while true; do entire …; done` loop only breaks when
// the process is *killed by* the signal (WIFSIGNALED), not when it exits
// normally with code 130. A "simplification" of dieFromSignal back to a plain
// os.Exit(130) would leave the exit code looking right while silently breaking
// loop-escape — this test catches exactly that by re-executing the test binary
// in child mode and asserting it died by the signal.
func TestDieFromSignal_TerminatesBySignal(t *testing.T) {
	t.Parallel()
	if runtime.GOOS == "windows" {
		t.Skip("signal-to-self / WIFSIGNALED semantics do not apply on Windows")
	}

	tests := []struct {
		name string
		env  string
		want syscall.Signal
	}{
		{"SIGINT", "INT", syscall.SIGINT},
		{"SIGTERM", "TERM", syscall.SIGTERM},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			// -test.run=^$ matches no test; TestMain's child branch runs before
			// m.Run() and terminates the process, so no test actually executes.
			cmd := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^$")
			cmd.Env = append(os.Environ(), dieFromSignalEnvVar+"="+tc.env)

			err := cmd.Run()

			var exitErr *exec.ExitError
			if !errors.As(err, &exitErr) {
				t.Fatalf("child did not exit with an error status; err=%v (expected death by signal)", err)
			}
			ws, ok := exitErr.Sys().(syscall.WaitStatus)
			if !ok {
				t.Fatalf("no syscall.WaitStatus available: %T", exitErr.Sys())
			}
			if !ws.Signaled() {
				t.Fatalf("child exited normally with code %d; want death by signal %v — dieFromSignal must re-raise, not os.Exit", ws.ExitStatus(), tc.want)
			}
			if ws.Signal() != tc.want {
				t.Fatalf("child killed by %v, want %v", ws.Signal(), tc.want)
			}
		})
	}
}

// TestExitCodeForSignal locks the conventional 128+signum mapping the
// Ctrl-C/SIGTERM fix relies on, so a future "simplification" back to a
// hardcoded 130 can't silently regress SIGTERM's 143.
func TestExitCodeForSignal(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		sig  os.Signal
		want int
	}{
		{"SIGINT", os.Interrupt, 130},
		{"SIGTERM", syscall.SIGTERM, 143},
		{"non-numeric signal falls back to 130", nonNumericSignal{}, 130},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := exitCodeForSignal(tc.sig); got != tc.want {
				t.Errorf("exitCodeForSignal(%v) = %d, want %d", tc.sig, got, tc.want)
			}
		})
	}
}
