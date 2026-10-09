package hosted

import (
	"bytes"
	"strings"
	"testing"
)

func TestCredentialOnlyAnswersGitHubHTTPSAndNeverStores(t *testing.T) {
	t.Setenv("REIN_GITHUB_TOKEN", "synthetic-test-credential")
	for _, tc := range []struct {
		op, request string
		want        bool
	}{
		{"get", "protocol=https\nhost=github.com\n\n", true},
		{"get", "protocol=http\nhost=github.com\n\n", false},
		{"get", "protocol=https\nhost=other.example\n\n", false},
		{"store", "protocol=https\nhost=github.com\n\n", false},
		{"erase", "protocol=https\nhost=github.com\n\n", false},
	} {
		var out bytes.Buffer
		if err := Credential(tc.op, strings.NewReader(tc.request), &out); err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out.String(), "password=synthetic-test-credential") != tc.want {
			t.Fatalf("wrong helper response for %s", tc.request)
		}
	}
	for _, v := range GitEnv("/some path/rein") {
		if strings.Contains(v, "synthetic-test-credential") {
			t.Fatal("token in helper configuration")
		}
	}
}
