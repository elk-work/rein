package service

import (
	"encoding/xml"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/config"
)

func TestLogonTaskXMLRunsAsThePersonWithNoPassword(t *testing.T) {
	args, err := LogonTaskArgs(`C:\Users\Ana Ruiz\.rein\config.toml`, `C:\Users\Ana Ruiz\.rein\log`)
	if err != nil {
		t.Fatal(err)
	}
	x := LogonTaskXML(`C:\Users\Ana Ruiz\AppData\Local\Programs\Elk\rein.exe`,
		args, `PC\Ana & Co`)

	// Well-formed once the UTF-16 declaration is set aside.
	var v struct {
		XMLName xml.Name
	}
	d := xml.NewDecoder(strings.NewReader(strings.Replace(x, `encoding="UTF-16"`, `encoding="UTF-8"`, 1)))
	if err := d.Decode(&v); err != nil || v.XMLName.Local != "Task" {
		t.Fatalf("not a task document: %v\n%s", err, x)
	}
	for _, want := range []string{
		"<LogonType>InteractiveToken</LogonType>", "<LogonTrigger>", "<UserId>PC\\Ana &amp; Co</UserId>",
		"<ExecutionTimeLimit>PT0S</ExecutionTimeLimit>", "<RestartOnFailure>",
		"<Command>conhost.exe</Command>", "--headless", `&quot;C:\Users\Ana Ruiz\AppData\Local\Programs\Elk\rein.exe&quot;`,
		"run --at-login --config", "rein.log",
	} {
		if !strings.Contains(x, want) {
			t.Errorf("missing %q", want)
		}
	}
	if strings.Contains(strings.ToLower(x), "password") {
		t.Error("the task stores or asks for a password")
	}
}

func TestUTF16HasAByteOrderMark(t *testing.T) {
	b := utf16LE("<a/>")
	if len(b) != 2+8 || b[0] != 0xFF || b[1] != 0xFE || b[2] != '<' || b[3] != 0 {
		t.Fatalf("% x", b)
	}
}

func TestLogonTaskArgsCapturesInstallingHome(t *testing.T) {
	for _, homeOverride := range []string{"", "custom home"} {
		for _, explicitConfig := range []string{"", "other config.toml"} {
			t.Run(homeOverride+"/"+explicitConfig, func(t *testing.T) {
				userHome := t.TempDir()
				t.Setenv("HOME", userHome)
				t.Setenv("USERPROFILE", userHome)
				t.Setenv(config.EnvHome, homeOverride)
				home := filepath.Join(userHome, ".rein")
				if homeOverride != "" {
					home, _ = filepath.Abs(homeOverride)
				}
				wantConfig := filepath.Join(home, config.FileName)
				if explicitConfig != "" {
					wantConfig, _ = filepath.Abs(explicitConfig)
				}
				logDir := filepath.Join(home, "log")
				got, err := LogonTaskArgs(explicitConfig, logDir)
				if err != nil {
					t.Fatal(err)
				}
				want := []string{"run", "--at-login", "--config", wantConfig, "--at-login-home", home, "--log-file", filepath.Join(logDir, LoopLogName)}
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("args = %q, want %q", got, want)
				}
			})
		}
	}
}
