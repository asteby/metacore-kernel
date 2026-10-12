package claude

import "io"

// ptyProc is a child process attached to a pseudo-terminal, abstracted over
// the per-OS implementations (linux: hand-rolled /dev/ptmx; darwin:
// creack/pty; windows: ConPTY). Read returns the terminal output, Write types
// into it. Kill terminates the process (and its group where supported) and
// Close releases the terminal.
type ptyProc interface {
	io.ReadWriteCloser
	Kill()
}
