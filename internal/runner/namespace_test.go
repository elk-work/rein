package runner

import (
	"context"
	"os/exec"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/adapter"
	"github.com/elk-work/rein/internal/config"
)

func TestConfiguredRepositoryCapabilities(t *testing.T) {
	repos := map[string]string{}
	for _, tc := range []struct{ key, remote string }{
		{"https", "https://github.com/elk-work/scout.git"},
		{"ssh", "git@github.com:elk-work/rein.git"},
		{"ssh-url", "ssh://git@github.com/elk-work/rein.git"},
		{"unknown", "https://example.com/elk-work/unknown.git"},
		{"elk-work/scout", ""},
	} {
		path := t.TempDir()
		for _, args := range [][]string{{"init", path}, {"-C", path, "remote", "add", "origin", tc.remote}} {
			if tc.remote == "" && args[0] == "-C" {
				continue
			}
			if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
				t.Fatalf("git: %v: %s", err, out)
			}
		}
		repos[tc.key] = path
	}
	repos["elk-work/missing"] = filepath.Join(t.TempDir(), "missing")
	got := RepoCapabilities(context.Background(), repos)
	want := []string{"repo:elk-work/rein", "repo:elk-work/scout"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestNamespacedQueuePreflight(t *testing.T) {
	t.Setenv(EnvCapabilities, "github-cli,claude,grok")
	cfg := config.Config{
		Repos:        map[string]string{"elk-work/rein": t.TempDir()},
		Capabilities: []string{"node", "mcp:dropped", "unknown:declared", "repo:fake/repo"},
	}
	h := DetectHostCapabilities(context.Background(), cfg)
	m := adapter.Manifest{Kind: "claude", Platforms: []adapter.Platform{adapter.AnyArch(runtime.GOOS)}, Capabilities: map[adapter.Capability]adapter.Support{adapter.CapShell: adapter.SupportYes}}
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"tool:git", true}, {"tool:github-cli", true}, {"tool:node", false},
		{"tool:claude", true}, {"tool:missing", false},
		{"agent:claude", true}, {"agent:grok", false}, {"agent:", false}, {"runtime:", false},
		{"os:" + hostOS(runtime.GOOS), true}, {"os:other", false},
		{"repo:elk-work/rein", true}, {"repo:fake/repo", false},
		{"mcp:allowed", true}, {"mcp:dropped", false},
		{"does:code-change", true}, {"tier:frontier", true},
		{"unknown:declared", true}, {"unknown:value", true},
		{"github-cli", true}, {"node", true}, {"missing", true},
		{"supabase-vault", false}, {"gcloud", false}, {"ark-cli", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			missing := MissingQueueRequirements([]string{tc.name}, m, h, []string{"mcp:allowed"})
			if (len(missing) == 0) != tc.want {
				t.Fatalf("missing = %v, satisfied want %v", missing, tc.want)
			}
		})
	}
	for _, name := range []string{"runtime:shell", "runtime:approvals", "runtime:unknown", "shell"} {
		rt, host := SplitRequirements([]string{name})
		if len(host) != 0 || len(rt) != 1 {
			t.Fatalf("split %s: %v %v", name, rt, host)
		}
		err := adapter.CheckSupport(m, adapter.RunSpec{RequiredCapabilities: rt})
		if (err == nil) != (name == "runtime:shell" || name == "shell") {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestMacClaudeQueueDeclaration(t *testing.T) {
	t.Setenv(EnvCapabilities, "github-cli,xcode,node,go,supabase-cli,claude,codex,grok")
	h := DetectHostCapabilities(context.Background(), config.Config{Repos: map[string]string{
		"elk-work/rein": t.TempDir(), "elk-work/scout": t.TempDir(),
	}})
	h = h.withExtra("resolved MCP", "mcp:docs")
	m := adapter.Manifest{Kind: "claude", Capabilities: map[adapter.Capability]adapter.Support{}}
	for _, c := range adapter.Capabilities() {
		m.Capabilities[c] = adapter.SupportYes
	}
	got := strings.Join(declaredCapabilities(m, h, "darwin"), ",")
	want := "agent:claude,approvals,claude,codex,elk-connector,file_edit,git,github-cli,go,grok,mcp,mcp:docs,node,os:macos,repo:elk-work/rein,repo:elk-work/scout,resume,runtime:approvals,runtime:file_edit,runtime:git,runtime:mcp,runtime:resume,runtime:shell,runtime:steer,runtime:structured_events,runtime:worktree,shell,steer,structured_events,supabase-cli,tool:git,tool:github-cli,tool:go,tool:node,tool:supabase-cli,tool:xcode,worktree,xcode"
	if got != want {
		t.Fatalf("declared:\n%s\nwant:\n%s", got, want)
	}
	t.Log(got)
}

func TestNewDeclarationsPreserveLegacyNamesAtLimit(t *testing.T) {
	var names []string
	for i := 0; i < MaxDeclaredCapabilities; i++ {
		names = append(names, "z"+strings.Repeat("x", i))
	}
	h := NewHostCapabilities("test", names...)
	got := DeclaredCapabilities(adapter.Manifest{Kind: "claude"}, h)
	if !reflect.DeepEqual(got, h.Names()) {
		t.Fatalf("legacy names displaced: %v", got)
	}
}

func TestPushAccessProseRequiresGitHubAndConfiguredRepository(t *testing.T) {
	_, names := SplitRequirements([]string{"git push access to elk-work/rein"})
	if strings.Join(names, ",") != "github-cli,repo:elk-work/rein" {
		t.Fatal(names)
	}
	h := NewHostCapabilities("test", "github-cli")
	m := adapter.Manifest{}
	if got := MissingQueueRequirements(names, m, h, nil); strings.Join(got, ",") != "repo:elk-work/rein" {
		t.Fatal(got)
	}
	h = h.withExtra("configured checkout", "repo:elk-work/rein")
	if got := MissingQueueRequirements(names, m, h, nil); len(got) != 0 {
		t.Fatal(got)
	}
	h = NewHostCapabilities("configured checkout", "repo:elk-work/rein")
	if got := MissingQueueRequirements(names, m, h, nil); strings.Join(got, ",") != "github-cli" {
		t.Fatal(got)
	}
}
