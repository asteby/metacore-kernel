//go:build windows

package claude

import (
	"context"
	"os"
	"time"

	"github.com/UserExistsError/conpty"
	"golang.org/x/sys/windows"
)

// winPTY wraps a ConPTY session. NOT validated on a real Windows host yet.
type winPTY struct {
	cpty *conpty.ConPty
}

func (p *winPTY) Read(b []byte) (int, error)  { return p.cpty.Read(b) }
func (p *winPTY) Write(b []byte) (int, error) { return p.cpty.Write(b) }
func (p *winPTY) Close() error                { return p.cpty.Close() }

func (p *winPTY) Kill() {
	if pid := p.cpty.Pid(); pid > 0 {
		if proc, err := os.FindProcess(pid); err == nil {
			_ = proc.Kill()
		}
	}
}

func startOnPTY(bin string, args, env []string, dir string, cols uint16) (ptyProc, error) {
	if !conpty.IsConPtyAvailable() {
		return nil, os.ErrInvalid // Windows < 10 1809: no ConPTY
	}
	cmdline := windows.ComposeCommandLine(append([]string{bin}, args...))
	cp, err := conpty.Start(cmdline,
		conpty.ConPtyDimensions(int(cols), 50),
		conpty.ConPtyWorkDir(dir),
		conpty.ConPtyEnv(env),
	)
	if err != nil {
		return nil, err
	}
	go func() { // reap; closes the output so the reader sees EOF on exit
		_, _ = cp.Wait(context.Background())
		time.Sleep(200 * time.Millisecond)
		_ = cp.Close()
	}()
	return &winPTY{cpty: cp}, nil
}
