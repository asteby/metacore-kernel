//go:build !linux && !darwin && !windows

package claude

import "errors"

func startOnPTY(bin string, args, env []string, dir string, cols uint16) (ptyProc, error) {
	return nil, errors.New("pty: claude connect is not supported on this platform")
}
