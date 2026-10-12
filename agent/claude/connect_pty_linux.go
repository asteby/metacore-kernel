//go:build linux

package claude

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"unsafe"
)

// startOnPTY runs cmd attached to a fresh pseudo-terminal of the given width
// (very wide so the OAuth URL is never wrapped) and returns the master side.
// Hand-rolled over /dev/ptmx to avoid a new module dependency.
func openLinuxPTY(cmd *exec.Cmd, cols uint16) (*os.File, error) {
	master, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, fmt.Errorf("pty: %w", err)
	}
	fail := func(e error) (*os.File, error) { master.Close(); return nil, fmt.Errorf("pty: %w", e) }

	var unlock int32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCSPTLCK, uintptr(unsafe.Pointer(&unlock))); e != 0 {
		return fail(e)
	}
	var n uint32
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, master.Fd(), syscall.TIOCGPTN, uintptr(unsafe.Pointer(&n))); e != 0 {
		return fail(e)
	}
	slave, err := os.OpenFile(fmt.Sprintf("/dev/pts/%d", n), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return fail(err)
	}
	defer slave.Close()

	ws := struct{ Row, Col, X, Y uint16 }{Row: 50, Col: cols}
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, slave.Fd(), syscall.TIOCSWINSZ, uintptr(unsafe.Pointer(&ws))); e != 0 {
		return fail(e)
	}

	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	cmd.SysProcAttr.Setsid = true
	cmd.SysProcAttr.Setctty = true
	cmd.SysProcAttr.Ctty = 0
	if err := cmd.Start(); err != nil {
		master.Close()
		return nil, err
	}
	return master, nil
}

func startOnPTY(bin string, args, env []string, dir string, cols uint16) (ptyProc, error) {
	cmd := newPTYCmd(bin, args, env, dir)
	master, err := openLinuxPTY(cmd, cols)
	if err != nil {
		return nil, err
	}
	go func() { _ = cmd.Wait() }() // reap
	return &unixPTY{cmd: cmd, master: master}, nil
}
