//go:build windows

package claude

import (
	"errors"
	"os/exec"
	"strconv"
	"syscall"
)

// setProcAttr gives the child its own process group so a kill can take the
// whole tree rather than orphaning the MCP servers and hook commands a session
// spawns.
func setProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: syscall.CREATE_NEW_PROCESS_GROUP}
}

// interruptProcess always fails on Windows: there is no SIGINT to deliver to
// another process's console, and CTRL_C_EVENT cannot be aimed at a child in a
// new process group without attaching to its console first.
//
// Returning an error here is the whole design — [Session.Interrupt] escalates
// straight to a kill, which is what "on Windows kill" means in the brief.
func interruptProcess(cmd *exec.Cmd) error {
	return errors.New("claude: Windows has no SIGINT for a child process; kill instead")
}

// killProcess terminates the process and its children.
//
// taskkill /T is what reaches the tree; Process.Kill would leave the MCP
// servers running. Falling back to Kill matters when taskkill is missing from
// PATH in a stripped image.
func killProcess(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	pid := strconv.Itoa(cmd.Process.Pid)
	if err := exec.Command("taskkill", "/F", "/T", "/PID", pid).Run(); err != nil {
		return cmd.Process.Kill()
	}
	return nil
}
