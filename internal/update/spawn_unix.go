//go:build unix

package update

import (
	"os"
	"os/exec"
	"syscall"
)

// spawnDetached starts cmd in its own session (setsid) so the new gateway
// survives the updater's exit and no terminal hangup can reach it — the
// same guarantee deploy.sh gives its listener. The process exit is
// delivered on a one-shot buffered channel, so a fast crash is observable
// even after the child is reaped.
func spawnDetached(path string, argv []string, env []string) (*Spawned, error) {
	cmd := exec.Command(path, argv[1:]...)
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		return nil, err
	}
	exit := make(chan error, 1)
	go func() { exit <- cmd.Wait() }()
	return &Spawned{Pid: cmd.Process.Pid, Exit: exit, p: cmd.Process}, nil
}
