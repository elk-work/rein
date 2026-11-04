package service

import (
	"encoding/xml"
	"strings"
	"testing"
)

func TestLogonTaskXMLRunsAsThePersonWithNoPassword(t *testing.T) {
	x := LogonTaskXML(`C:\Users\Ana Ruiz\AppData\Local\Programs\Elk\rein.exe`,
		LogonTaskArgs(`C:\Users\Ana Ruiz\.rein\config.toml`, `C:\Users\Ana Ruiz\.rein\log`), `PC\Ana & Co`)

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
