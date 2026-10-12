//go:build darwin

package claude

import "github.com/creack/pty"

// startOnPTY runs the command on a pty via creack/pty (which sets Setsid and
// the controlling terminal). Cross-compiled only: not exercised on a real Mac
// in CI here.
func startOnPTY(bin string, args, env []string, dir string, cols uint16) (ptyProc, error) {
	cmd := newPTYCmd(bin, args, env, dir)
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 50, Cols: cols})
	if err != nil {
		return nil, err
	}
	go func() { _ = cmd.Wait() }() // reap
	return &unixPTY{cmd: cmd, master: master}, nil
}
