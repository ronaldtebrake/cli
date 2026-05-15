package redact

// This file exposes internal helpers needed by tests in OTHER packages
// (e.g. cmd/entire/cli/strategy) that exercise OPF behavior end-to-end.
// They cannot live in export_test.go because that file is invisible to
// importers — only redact's own tests would be able to call them.
//
// The "ForTest" suffix is a convention signal: production code MUST NOT
// call these. A future lint rule (e.g. forbidigo on the "ForTest"
// suffix) could enforce that statically; today it relies on reviewer
// discipline.

// GetOPFConfigForTest returns the current OPF configuration, or nil if
// never configured. Test-only — production code should never need to
// introspect the global config; gate behavior on OPFEnabled() instead.
func GetOPFConfigForTest() *OPFConfig {
	return getOPFConfig()
}

// ResetOPFConfigForTest clears configuration and the circuit breaker.
// Used to return to a "never configured" state between test cases.
// Test-only — production code should never need to reset OPF state.
func ResetOPFConfigForTest() {
	resetOPFConfig()
}
