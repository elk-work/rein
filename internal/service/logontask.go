package service

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"unicode/utf16"
)

// LogonTaskName is the Scheduled Task Rein registers on Windows when it is
// installed without the account's password (ark:scout#4306). A Windows
// service that runs as a person needs that person's password; a task that
// starts at their sign-in, with their own sign-in token, does not. It runs
// only while they are signed in, which is also when their Credential Manager
// (where Rein's Elk token lives) can be read at all.
const LogonTaskName = `\Elk\Rein`

// ErrLogonTaskUnsupported is returned off Windows.
var ErrLogonTaskUnsupported = errors.New("service: a sign-in task is a Windows feature; use `rein service install`")

// schtasks runs the Windows task scheduler's command line. A variable so the
// tests can see what would run without a Windows machine.
var schtasks = func(args ...string) ([]byte, error) {
	return exec.Command("schtasks.exe", args...).CombinedOutput()
}

// LogonTaskXML is the task definition: start at this person's sign-in, with
// their own token and no stored password, never stop on a time limit or on
// battery, and come back a minute after anything kills it. The action runs
// through `conhost.exe --headless` so no console window opens on their desk.
func LogonTaskXML(exe string, args []string, user string) string {
	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-16"?>
<Task version="1.2" xmlns="http://schemas.microsoft.com/windows/2004/02/mit/task">
  <RegistrationInfo><Description>`)
	b.WriteString(xmlEscape(Description))
	b.WriteString(`</Description></RegistrationInfo>
  <Triggers><LogonTrigger><Enabled>true</Enabled><UserId>`)
	b.WriteString(xmlEscape(user))
	b.WriteString(`</UserId></LogonTrigger></Triggers>
  <Principals><Principal id="Author"><UserId>`)
	b.WriteString(xmlEscape(user))
	b.WriteString(`</UserId><LogonType>InteractiveToken</LogonType><RunLevel>LeastPrivilege</RunLevel></Principal></Principals>
  <Settings>
    <MultipleInstancesPolicy>IgnoreNew</MultipleInstancesPolicy>
    <DisallowStartIfOnBatteries>false</DisallowStartIfOnBatteries>
    <StopIfGoingOnBatteries>false</StopIfGoingOnBatteries>
    <ExecutionTimeLimit>PT0S</ExecutionTimeLimit>
    <RestartOnFailure><Interval>PT1M</Interval><Count>999</Count></RestartOnFailure>
    <StartWhenAvailable>true</StartWhenAvailable>
    <Enabled>true</Enabled>
  </Settings>
  <Actions Context="Author"><Exec><Command>conhost.exe</Command><Arguments>`)
	b.WriteString(xmlEscape(strings.Join(append([]string{"--headless", quoteArg(exe)}, quoteArgs(args)...), " ")))
	b.WriteString(`</Arguments></Exec></Actions>
</Task>
`)
	return b.String()
}

// LogonTaskArgs is what the task runs: `rein run --at-login`, which keeps one
// supervised runner alive and restarts it after an upgrade or a settings
// change (cli/run.go), the job the service manager does elsewhere.
func LogonTaskArgs(configPath, logDir string) []string {
	args := []string{"run", "--at-login"}
	if p := strings.TrimSpace(configPath); p != "" {
		args = append(args, "--config", p)
	}
	return append(args, "--log-file", filepath.Join(logDir, LoopLogName))
}

// InstallLogonTask registers (or replaces) the task for user.
func InstallLogonTask(exe string, args []string, user string) error {
	if runtime.GOOS != "windows" {
		return ErrLogonTaskUnsupported
	}
	f, err := os.CreateTemp("", "rein-task-*.xml")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	// Task Scheduler reads its XML as UTF-16 with a byte-order mark.
	if _, err := f.Write(utf16LE(LogonTaskXML(exe, args, user))); err != nil {
		f.Close()
		return err
	}
	f.Close()
	if out, err := schtasks("/Create", "/TN", LogonTaskName, "/XML", f.Name(), "/F"); err != nil {
		return fmt.Errorf("service: register the sign-in task: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// StartLogonTask runs the task now rather than at the next sign-in.
func StartLogonTask() error { return runTask("/Run", "start") }

// StopLogonTask ends the running task; it starts again at the next sign-in.
func StopLogonTask() error { return runTask("/End", "stop") }

// UninstallLogonTask stops and removes the task.
func UninstallLogonTask() error {
	_ = StopLogonTask()
	if runtime.GOOS != "windows" {
		return ErrLogonTaskUnsupported
	}
	if out, err := schtasks("/Delete", "/TN", LogonTaskName, "/F"); err != nil {
		return fmt.Errorf("service: remove the sign-in task: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// LogonTaskInstalled reports whether the task exists, and whether it is
// running now.
func LogonTaskInstalled() (installed, running bool) {
	if runtime.GOOS != "windows" {
		return false, false
	}
	out, err := schtasks("/Query", "/TN", LogonTaskName, "/FO", "LIST")
	if err != nil {
		return false, false
	}
	return true, strings.Contains(string(out), "Running")
}

func runTask(flag, verb string) error {
	if runtime.GOOS != "windows" {
		return ErrLogonTaskUnsupported
	}
	if out, err := schtasks(flag, "/TN", LogonTaskName); err != nil {
		return fmt.Errorf("service: %s the sign-in task: %v: %s", verb, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func utf16LE(s string) []byte {
	var buf bytes.Buffer
	buf.Write([]byte{0xFF, 0xFE})
	for _, u := range utf16.Encode([]rune(s)) {
		_ = binary.Write(&buf, binary.LittleEndian, u)
	}
	return buf.Bytes()
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}

func quoteArg(a string) string {
	if a == "" || strings.ContainsAny(a, " \t\"") {
		return `"` + strings.ReplaceAll(a, `"`, `\"`) + `"`
	}
	return a
}

func quoteArgs(args []string) []string {
	out := make([]string, len(args))
	for i, a := range args {
		out[i] = quoteArg(a)
	}
	return out
}
