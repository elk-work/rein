package runner

import "testing"

func TestWrongRepoDeclaration(t *testing.T) {
	for _, tc := range []struct {
		name, account string
		want          bool
	}{
		{"valid", "stop_code: wrong_repo\nrepo: elk-work/signal\nStopped.", true},
		{"CRLF", "stop_code: wrong_repo\r\nrepo: elk-work/signal\r\nStopped.", true},
		{"same repo", "stop_code: wrong_repo\nrepo: ELK-WORK/SCOUT", false},
		{"missing target", "stop_code: wrong_repo", false},
		{"path target", "stop_code: wrong_repo\nrepo: /tmp/signal", false},
		{"URL target", "stop_code: wrong_repo\nrepo: https://github.com/elk-work/signal", false},
		{"quoted", "> stop_code: wrong_repo\n> repo: elk-work/signal", false},
		{"fenced", "```\nstop_code: wrong_repo\nrepo: elk-work/signal\n```", false},
		{"prose", "Wrong repository. Please requeue.", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := declaresWrongRepo(tc.account, RepoResolution{Name: "elk-work/scout"}); got != tc.want {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
		})
	}
}
