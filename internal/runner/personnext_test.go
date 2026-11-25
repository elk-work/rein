package runner

import "testing"

func TestTakePersonNext(t *testing.T) {
	for _, tc := range []struct {
		name, in, want, kind string
	}{
		{"absent", "Nothing needs you. Merged.\n\nDetail.", "Nothing needs you. Merged.\n\nDetail.", ""},
		{"plain", "Needs you: check the card.\n\nPerson next: check\n\nDetail.",
			"Needs you: check the card.\n\nDetail.", "check"},
		{"case and full stop", "Nothing needs you.\n\nPERSON NEXT: None.", "Nothing needs you.", "none"},
		{"bold label", "Needs you: decide.\n\n**Person next:** decision", "Needs you: decide.", "decision"},
		{"bold line", "Done.\n\n**Person next: check**\n\nMore.", "Done.\n\nMore.", "check"},
		{"underscore", "Done.\nperson_next: none", "Done.", "none"},
		{"inside a code block", "Done.\n\n```\nPerson next: check\n```", "Done.\n\n```\nPerson next: check\n```", ""},
		{"after a code block", "```\nPerson next: none\n```\nPerson next: check", "```\nPerson next: none\n```", "check"},
		{"unknown value stays", "Done.\n\nPerson next: maybe", "Done.\n\nPerson next: maybe", ""},
		{"in a sentence stays", "It said Person next: check in prose.", "It said Person next: check in prose.", ""},
		{"last wins", "Person next: none\nDone.\nPerson next: check", "Done.", "check"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, kind := takePersonNext(tc.in)
			if got != tc.want || kind != tc.kind {
				t.Errorf("takePersonNext(%q) = %q, %q; want %q, %q", tc.in, got, kind, tc.want, tc.kind)
			}
		})
	}
}
