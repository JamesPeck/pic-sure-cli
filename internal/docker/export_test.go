package docker

// SetTerminalSignaled makes r see a SIGINT as the terminal's (true) or not.
func SetTerminalSignaled(r *ExecRunner, v bool) {
	r.terminalSignaled = func() bool { return v }
}
