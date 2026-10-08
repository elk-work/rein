//go:build !windows

package claude

import (
	"errors"
	"os"
	"os/exec"
	"syscall"
)

// setProcAttr puts the child in its own process group.
//
// This is the lesson from Loom's sweep.md (elk docs/rein.md §4): a `claude -p`
// session spawns children — MCP servers, hook commands, whatever the agent
// runs — and signalling only the parent leaves them behind. Signalling the
// group takes the whole tree.
func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// interruptProcess sends SIGINT to the session's process group.
func interruptProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return errors.New("claude: the session has no process to interrupt")
	}
	// A negative pid addresses the process group, which is why Setpgid is set.
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGINT); err != nil {
		// The group may already be gone; fall back to the process itself.
		return cmd.Process.Signal(os.Interrupt)
	}
	return nil
}

// killProcess sends SIGKILL to the session's process group.
func killProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
