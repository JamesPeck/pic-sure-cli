package docker

// SetTerminalSignaledChild makes the foreground runner see a SIGINT as the
// terminal's (true) or not, until the returned func restores it.
func SetTerminalSignaledChild(v bool) (restore func()) {
	old := terminalSignaledChild
	terminalSignaledChild = func() bool { return v }
	return func() { terminalSignaledChild = old }
}
