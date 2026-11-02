package ws

// SafeDiagnosticForTest exposes safeDiagnostic to the external ws_test package.
func SafeDiagnosticForTest(err error) string { return safeDiagnostic(err) }
