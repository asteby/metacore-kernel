//go:build linux || darwin

package claude

import (
	"os"
	"os/exec"
	"syscall"
)

// unixPTY is the master side of a pty plus the process running on its slave.
type unixPTY struct {
	cmd    *exec.Cmd
	master *os.File
}

func (p *unixPTY) Read(b []byte) (int, error)  { return p.master.Read(b) }
func (p *unixPTY) Write(b []byte) (int, error) { return p.master.Write(b) }
func (p *unixPTY) Close() error                { return p.master.Close() }

func (p *unixPTY) Kill() {
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = syscall.Kill(-p.cmd.Process.Pid, syscall.SIGKILL)
	_ = p.cmd.Process.Kill()
}

// newPTYCmd builds the exec.Cmd shared by the unix implementations.
func newPTYCmd(bin string, args, env []string, dir string) *exec.Cmd {
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = env
	return cmd
}
