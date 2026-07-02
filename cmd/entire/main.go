package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/entireio/cli/cmd/entire/cli"
	"github.com/entireio/cli/cmd/entire/cli/api"
	"github.com/entireio/cli/cmd/entire/cli/versioninfo"
	"github.com/spf13/cobra"
)

func main() {
	// Resolve version/commit from build info before anything reads them.
	versioninfo.Load()

	// Create context that cancels on interrupt
	ctx, cancel := context.WithCancel(context.Background())

	// Handle interrupt signals
	sigChan := make(chan os.Signal, 1)
	signals := []os.Signal{os.Interrupt}
	if runtime.GOOS != "windows" {
		signals = append(signals, syscall.SIGTERM)
	}
	signal.Notify(sigChan, signals...)
	go func() {
		// First signal: cancel the context so in-flight work unwinds
		// cleanly. signal.Notify has disabled Go's default "SIGINT
		// terminates" behavior, so without the second read below a user
		// who Ctrl-C's again during a slow/stuck shutdown (e.g. a keyring
		// read blocked in a subprocess we can't cancel) would find every
		// further Ctrl-C swallowed. The second read restores an escape
		// hatch: press Ctrl-C again to force-exit with the conventional
		// 130 (128 + SIGINT).
		<-sigChan
		fmt.Fprintln(os.Stderr, "\nInterrupting… press Ctrl-C again to force quit.")
		cancel()
		<-sigChan
		dieFromInterrupt()
	}()

	// Create and execute root command
	rootCmd := cli.NewRootCmd()

	// Make managed-installed plugins discoverable by the kubectl-style
	// dispatcher: prepend the managed bin dir to PATH before resolution.
	// Idempotent and silent on failure (managed installs simply won't be
	// found this run; PATH-installed plugins still work). The closure
	// restores PATH so built-in commands and their subprocesses don't
	// inherit the prepended dir. When a plugin runs, we skip the restore
	// — the os.Exit ends the process, and the plugin child intentionally
	// inherits the prepended PATH so it can spawn sibling managed plugins.
	restorePATH := cli.PrependPluginBinDirToPATH(ctx)

	if handled, code := cli.MaybeRunPlugin(ctx, rootCmd, os.Args[1:]); handled {
		cancel()
		os.Exit(code)
	}
	restorePATH()

	// Retired env vars fail every built-in command up front; a removed knob
	// must never be silently ignored. Plugins (above) are exempt — what they
	// read is their business.
	if err := api.RejectRemovedAuthEnv(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		cancel()
		os.Exit(1)
	}

	executed, err := rootCmd.ExecuteContextC(ctx)
	if err != nil {
		var silent *cli.SilentError

		switch {
		case errors.Is(err, context.Canceled):
			// User aborted (Ctrl-C cancelled the root context). Don't dump
			// the raw transport/keyring cancellation string ("...: context
			// canceled", "read access token: signal: interrupt") as if it
			// were a failure — die quietly by re-raising SIGINT (see
			// dieFromInterrupt) so an enclosing `while ...; do entire; done`
			// loop actually breaks on a single Ctrl-C.
			cancel()
			dieFromInterrupt()
		case errors.As(err, &silent):
			// Command already printed the error
		case strings.Contains(err.Error(), "unknown command") || strings.Contains(err.Error(), "unknown flag"):
			showSuggestion(rootCmd, err)
		case isPositionalArgError(err):
			// Arg-count errors come from cobra's own validators (e.g.
			// cobra.ExactArgs) and surface as "accepts N arg(s), received M".
			// Show the failing subcommand's usage so the user sees what it
			// actually expects — rootCmd's usage isn't useful for a
			// subcommand-level arg mismatch. ExecuteContextC returns the
			// deepest matched command, which is the one that failed.
			showSuggestion(executed, err)
		default:
			fmt.Fprintln(rootCmd.OutOrStderr(), err)
		}

		cancel()
		os.Exit(1)
	}
	if cli.ShouldCheckCheckpointPolicyWarning(executed) {
		cli.WarnCheckpointPolicyIfNeeded(ctx, rootCmd.ErrOrStderr(), versioninfo.Version)
	}
	cancel() // Cleanup on successful exit
}

// dieFromInterrupt terminates the process as if it had been killed by SIGINT,
// rather than exiting normally with code 130. The distinction matters to an
// interactive shell: it only aborts a `while true; do entire ...; done` loop
// when the child is *killed by* SIGINT (WIFSIGNALED). A plain os.Exit(130) is
// an ordinary exit, so the loop keeps respawning entire and Ctrl-C never
// escapes it. We reset SIGINT to its default disposition, re-raise it to
// ourselves, and briefly wait for delivery; if the re-raise can't be delivered
// (e.g. Windows, where os.Interrupt-to-self is unsupported) we fall back to a
// conventional exit so we never hang.
func dieFromInterrupt() {
	signal.Reset(os.Interrupt)
	if p, err := os.FindProcess(os.Getpid()); err == nil {
		if err := p.Signal(os.Interrupt); err == nil {
			time.Sleep(500 * time.Millisecond) // signal delivery ends the process well before this elapses
		}
	}
	os.Exit(130)
}

// isPositionalArgError reports whether err looks like a cobra positional-
// arg validator failure. cobra's stock validators (ExactArgs, NoArgs,
// MinimumNArgs, MaximumNArgs, RangeArgs) all surface error strings
// containing "arg(s)", and that substring doesn't appear in cobra's other
// error paths or in the cli's own errors — so it's a stable, cheap
// discriminator without reaching into cobra internals.
func isPositionalArgError(err error) bool {
	return strings.Contains(err.Error(), "arg(s)")
}

func showSuggestion(cmd *cobra.Command, err error) {
	// Print usage first (brew style)
	fmt.Fprint(cmd.OutOrStderr(), cmd.UsageString())
	fmt.Fprintf(cmd.OutOrStderr(), "\nError: Invalid usage: %v\n", err)
}
