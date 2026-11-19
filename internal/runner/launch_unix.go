//go:build unix

package runner

import (
	"os"
	"syscall"
)

func ownProcessGroup() *syscall.SysProcAttr { return &syscall.SysProcAttr{Setpgid: true} }

func terminate(p *os.Process) error { return p.Signal(syscall.SIGTERM) }

func signalOf(ps *os.ProcessState) string {
	if ws, ok := ps.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return ws.Signal().String()
	}
	return ""
}
