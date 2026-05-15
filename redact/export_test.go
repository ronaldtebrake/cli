package redact

// This file exposes internal helpers to redact-package tests only.
// It is excluded from regular builds (the _test.go suffix is invisible to
// importers), so production code outside this package cannot reach the
// global OPF config. Tests in other packages that need to manipulate the
// global state can do so via thin re-exports declared in their own
// _test.go files, but those re-exports stay this side of the package
// boundary.

// GetOPFConfigForTest returns the current OPF configuration, or nil if
// never configured.
func GetOPFConfigForTest() *OPFConfig {
	return getOPFConfig()
}

// ResetOPFConfigForTest clears configuration and the circuit breaker.
// Used to return to a "never configured" state between test cases.
func ResetOPFConfigForTest() {
	resetOPFConfig()
}
