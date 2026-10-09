// Package hosted contains the environment-only credentials for one hosted run.
package hosted

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/elk-work/rein/internal/secretenv"
)

var Names = []string{"ANTHROPIC_API_KEY", "OPENAI_API_KEY", "CODEX_API_KEY", "REIN_ELK_TOKEN", "REIN_GITHUB_TOKEN"}

func Values() map[string]string {
	out := map[string]string{}
	for _, name := range Names {
		if v := os.Getenv(name); v != "" {
			out[name] = v
		}
	}
	return out
}

// ModelKey is the deliberately narrow hosted billing allow-list.
func ModelKey(name string) bool {
	return name == "ANTHROPIC_API_KEY" || name == "OPENAI_API_KEY" || name == "CODEX_API_KEY"
}

func Forbidden(name string, enabled bool) bool {
	return secretenv.Metered(name) && !(enabled && ModelKey(name))
}

// GitEnv installs a credential helper through environment configuration, not
// .git/config. Git invokes Rein with the operation only; the token is read here.
func GitEnv(executable string) map[string]string {
	executable = strings.ReplaceAll(executable, "\\", "/")
	quoted := "'" + strings.ReplaceAll(executable, "'", "'\\''") + "'"
	return map[string]string{
		"GIT_CONFIG_COUNT": "3",
		"GIT_CONFIG_KEY_0": "credential.helper", "GIT_CONFIG_VALUE_0": "",
		"GIT_CONFIG_KEY_1": "credential.https://github.com.helper", "GIT_CONFIG_VALUE_1": "!" + quoted + " hosted-credential",
		"GIT_CONFIG_KEY_2": "credential.useHttpPath", "GIT_CONFIG_VALUE_2": "true",
		"GIT_TERMINAL_PROMPT": "0",
	}
}

// Credential implements Git's helper protocol. It never stores a credential,
// and refuses destinations other than GitHub HTTPS.
func Credential(operation string, in io.Reader, out io.Writer) error {
	if operation != "get" {
		return nil
	}
	fields := map[string]string{}
	scan := bufio.NewScanner(in)
	for scan.Scan() {
		if scan.Text() == "" {
			break
		}
		k, v, ok := strings.Cut(scan.Text(), "=")
		if ok {
			fields[k] = v
		}
	}
	if err := scan.Err(); err != nil {
		return err
	}
	if fields["protocol"] != "https" || fields["host"] != "github.com" {
		return nil
	}
	token := os.Getenv("REIN_GITHUB_TOKEN")
	if token == "" || strings.ContainsAny(token, "\r\n") {
		return fmt.Errorf("hosted: REIN_GITHUB_TOKEN is missing or invalid")
	}
	_, err := fmt.Fprintf(out, "username=x-access-token\npassword=%s\n\n", token)
	return err
}
