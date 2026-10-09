package config_test

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/elk-work/rein/internal/config"
)

func TestDirHonoursReinHome(t *testing.T) {
	t.Setenv(config.EnvHome, "/tmp/rein-home")
	dir, err := config.Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if dir != "/tmp/rein-home" {
		t.Fatalf("Dir = %q, want /tmp/rein-home", dir)
	}
	path, err := config.Path()
	if err != nil {
		t.Fatalf("Path: %v", err)
	}
	if want := filepath.Join("/tmp/rein-home", config.FileName); path != want {
		t.Fatalf("Path = %q, want %q", path, want)
	}
}

func TestDirFallsBackToHome(t *testing.T) {
	t.Setenv(config.EnvHome, "")
	home := t.TempDir()
	t.Setenv("HOME", home)        // unix
	t.Setenv("USERPROFILE", home) // windows
	dir, err := config.Dir()
	if err != nil {
		t.Fatalf("Dir: %v", err)
	}
	if want := filepath.Join(home, ".rein"); dir != want {
		t.Fatalf("Dir = %q, want %q", dir, want)
	}
}

func TestParse(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	path := filepath.Join(t.TempDir(), "config.toml")
	const src = `
host_id = "mac"
workspace = "elk-scout"
work_dir = "/var/rein/work"

[elk]
mcp_url = "https://example.supabase.co/functions/v1/elk-mcp"

[[queues]]
name = "mac-claude"
agent_kind = "claude"

[[queues]]
name = "mac-codex"
agent_kind = "codex"
workspace = "signal"
`
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if cfg.HostID != "mac" {
		t.Errorf("HostID = %q, want mac", cfg.HostID)
	}
	if cfg.Workspace != "elk-scout" {
		t.Errorf("Workspace = %q, want elk-scout", cfg.Workspace)
	}
	if cfg.WorkDir != "/var/rein/work" {
		t.Errorf("WorkDir = %q", cfg.WorkDir)
	}
	if cfg.Elk.MCPURL == "" {
		t.Error("Elk.MCPURL is empty")
	}
	if len(cfg.Queues) != 2 {
		t.Fatalf("got %d queues, want 2", len(cfg.Queues))
	}

	claude, ok := cfg.Queue("mac-claude")
	if !ok {
		t.Fatal("Queue(mac-claude) not found")
	}
	if got := cfg.WorkspaceFor(claude); got != "elk-scout" {
		t.Errorf("WorkspaceFor(mac-claude) = %q, want the config default elk-scout", got)
	}
	codex, _ := cfg.Queue("mac-codex")
	if got := cfg.WorkspaceFor(codex); got != "signal" {
		t.Errorf("WorkspaceFor(mac-codex) = %q, want the queue override signal", got)
	}
	if _, ok := cfg.Queue("nope"); ok {
		t.Error("Queue(nope) reported found")
	}
}

func TestRoundtrip(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)

	want := config.Config{
		HostID:       "win",
		MachineID:    "9f1c4a2b",
		Workspace:    "elk-scout",
		WorkDir:      filepath.Join(home, "work"),
		DefaultRepo:  filepath.Join(home, "dev", "elk"),
		Capabilities: []string{"docker", "SUPABASE_SERVICE_ROLE_KEY"},
		Repos:        map[string]string{"scout": filepath.Join(home, "dev", "elk", "scout")},
		Elk:          config.Elk{MCPURL: "https://example.test/elk-mcp"},
		Queues: []config.Queue{
			{Name: "win-codex", AgentKind: "codex"},
			{Name: "win-claude", AgentKind: "claude", Workspace: "pulse",
				Repo: filepath.Join(home, "dev", "rein"), PermissionMode: "accept_edits",
				Capabilities: []string{"xcode"}},
		},
	}
	if err := want.Save(); err != nil {
		t.Fatalf("Save: %v", err)
	}

	path, err := config.Path()
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	// The config names endpoints and workspaces; 0600 keeps a future secret
	// field from having to remember to tighten it.
	if perm := info.Mode().Perm(); perm != 0o600 && runtimeIsUnix() {
		t.Errorf("mode = %v, want 0600", perm)
	}

	got, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if got.HostID != want.HostID || got.MachineID != want.MachineID ||
		got.Workspace != want.Workspace ||
		got.WorkDir != want.WorkDir || got.Elk.MCPURL != want.Elk.MCPURL {
		t.Errorf("scalars did not round-trip:\n got %+v\nwant %+v", got, want)
	}
	if len(got.Queues) != len(want.Queues) {
		t.Fatalf("got %d queues, want %d", len(got.Queues), len(want.Queues))
	}
	for i := range want.Queues {
		// DeepEqual rather than !=: a Queue carries slices now (capabilities),
		// so it is no longer a comparable struct.
		if !reflect.DeepEqual(got.Queues[i], want.Queues[i]) {
			t.Errorf("queue %d: got %+v, want %+v", i, got.Queues[i], want.Queues[i])
		}
	}
	if !reflect.DeepEqual(got.Capabilities, want.Capabilities) {
		t.Errorf("capabilities did not round-trip: got %v, want %v", got.Capabilities, want.Capabilities)
	}
	if !reflect.DeepEqual(got.Repos, want.Repos) {
		t.Errorf("repos did not round-trip: got %v, want %v", got.Repos, want.Repos)
	}
	if got.DefaultRepo != want.DefaultRepo {
		t.Errorf("default_repo did not round-trip: got %q, want %q", got.DefaultRepo, want.DefaultRepo)
	}
}

func TestLoadMissingFile(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	cfg, err := config.Load()
	if !errors.Is(err, config.ErrNotExist) {
		t.Fatalf("err = %v, want ErrNotExist", err)
	}
	// Defaults still come back, so `rein status` can print something useful
	// before the machine has enrolled.
	if cfg.WorkDir == "" {
		t.Error("WorkDir is empty on a missing config; want the default")
	}
}

func TestDefaultsApplied(t *testing.T) {
	home := t.TempDir()
	t.Setenv(config.EnvHome, home)
	path := filepath.Join(home, config.FileName)
	if err := os.WriteFile(path, []byte("workspace = \"elk-scout\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if want := filepath.Join(home, "work"); cfg.WorkDir != want {
		t.Errorf("WorkDir = %q, want the default %q", cfg.WorkDir, want)
	}
}

func TestValidate(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  config.Config
		want string // substring of the expected error; "" means valid
	}{
		{"empty is valid", config.Config{}, ""},
		{
			"queue over the cap is rejected, not truncated",
			config.Config{Queues: []config.Queue{{Name: "mac-verylongname", AgentKind: "claude"}}},
			"over the 12-byte cap",
		},
		{"queue at the cap is fine", config.Config{Queues: []config.Queue{{Name: "mac-claudex", AgentKind: "claude"}}}, ""},
		{"name required", config.Config{Queues: []config.Queue{{AgentKind: "claude"}}}, "name is required"},
		{"agent_kind required", config.Config{Queues: []config.Queue{{Name: "mac-claude"}}}, "agent_kind is required"},
		{
			"uppercase name rejected",
			config.Config{Queues: []config.Queue{{Name: "Mac-Claude", AgentKind: "claude"}}},
			"lowercase letters",
		},
		{
			"duplicate names rejected",
			config.Config{Queues: []config.Queue{
				{Name: "mac-claude", AgentKind: "claude"},
				{Name: "mac-claude", AgentKind: "codex"},
			}},
			"appears twice",
		},
		{"hyphen in host_id rejected", config.Config{HostID: "mac-01"}, "no hyphen"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.cfg.Validate()
			if tc.want == "" {
				if err != nil {
					t.Fatalf("Validate = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("Validate = nil, want an error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Validate = %q, want it to contain %q", err, tc.want)
			}
		})
	}
}

func TestSaveRefusesInvalid(t *testing.T) {
	t.Setenv(config.EnvHome, t.TempDir())
	cfg := config.Config{Queues: []config.Queue{{Name: "mac-waytoolongname", AgentKind: "claude"}}}
	if err := cfg.Save(); err == nil {
		t.Fatal("Save accepted an invalid config")
	}
	path, _ := config.Path()
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("Save wrote a file it had already refused")
	}
}

func TestSuggestQueueName(t *testing.T) {
	cfg := config.Config{HostID: "mac"}
	if got, ok := cfg.SuggestQueueName("claude"); !ok || got != "mac-claude" {
		t.Errorf("SuggestQueueName(claude) = %q, %v; want mac-claude, true", got, ok)
	}
	// Ruling 3: a name over the cap is a rejection, never a truncation.
	if got, ok := cfg.SuggestQueueName("verylongagent"); ok {
		t.Errorf("SuggestQueueName(verylongagent) = %q, true; want a rejection", got)
	}
	if _, ok := (config.Config{}).SuggestQueueName("claude"); ok {
		t.Error("SuggestQueueName succeeded with no host_id")
	}
}

func TestDefaultHostID(t *testing.T) {
	got := config.DefaultHostID()
	if len(got) > 3 {
		t.Errorf("DefaultHostID = %q, want at most 3 characters", got)
	}
	for _, r := range got {
		if !(r >= 'a' && r <= 'z') && !(r >= '0' && r <= '9') {
			t.Errorf("DefaultHostID = %q, contains %q", got, r)
		}
	}
}

func TestParseError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	if err := os.WriteFile(path, []byte("this is not = = toml"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := config.LoadFile(path); err == nil {
		t.Fatal("LoadFile accepted invalid TOML")
	}
}

func runtimeIsUnix() bool {
	return os.PathSeparator == '/'
}

// How many run logs to keep, and the two ways of saying "all of them" and
// "the default". A negative number is the escape hatch a person reaches for
// when they are debugging and want nothing thrown away.
func TestKeepRuns(t *testing.T) {
	for _, tc := range []struct {
		set  int
		want int
	}{
		{0, config.DefaultLogKeepRuns},
		{10, 10},
		{-1, 0},
	} {
		if got := (config.Config{LogKeepRuns: tc.set}).KeepRuns(); got != tc.want {
			t.Errorf("log_keep_runs = %d: KeepRuns() = %d, want %d", tc.set, got, tc.want)
		}
	}
}

// The knob survives a round trip through the file, which is the only thing
// that makes it a knob rather than a field.
func TestLogKeepRunsRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.FileName)
	c := config.Config{HostID: "mac", LogKeepRuns: 7}
	if err := c.SaveFile(path); err != nil {
		t.Fatal(err)
	}
	got, err := config.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.LogKeepRuns != 7 || got.KeepRuns() != 7 {
		t.Errorf("log_keep_runs = %d", got.LogKeepRuns)
	}
}

// The telemetry block, and the reason its interval is text rather than a count
// of nanoseconds: the CLI's duration flags take the same spelling, and a config
// that disagreed with the flags about how long twenty minutes is written down
// would be its own small trap.
func TestTelemetryBlock(t *testing.T) {
	const src = `
host_id = "mac"

[telemetry]
interval = "90s"
disabled = true
`
	dir := t.TempDir()
	path := filepath.Join(dir, config.FileName)
	if err := os.WriteFile(path, []byte(src), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := config.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Telemetry.Interval.Duration() != 90*time.Second {
		t.Errorf("interval = %s, want 90s", got.Telemetry.Interval.Duration())
	}
	if !got.Telemetry.Disabled {
		t.Error("disabled was not read")
	}
}

// A config with no telemetry block is a config with telemetry ON. `disabled`
// is a negative switch precisely so the zero value is the wanted one.
func TestTelemetryDefaultsToOn(t *testing.T) {
	c := config.Config{HostID: "mac"}
	if c.Telemetry.Disabled {
		t.Error("telemetry is off in a config that never mentioned it")
	}
	if c.Telemetry.Interval != 0 {
		t.Errorf("interval = %s, want zero so the runner's default applies", c.Telemetry.Interval.Duration())
	}
}

func TestTelemetryIntervalRoundTrips(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.FileName)
	c := config.Config{HostID: "mac", Telemetry: config.Telemetry{Interval: config.Duration(2 * time.Minute)}}
	if err := c.SaveFile(path); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Written as text a person can read and edit, not as 120000000000.
	if !strings.Contains(string(blob), `interval = "2m0s"`) {
		t.Errorf("the interval was not written as a duration string:\n%s", blob)
	}
	got, err := config.LoadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if got.Telemetry.Interval.Duration() != 2*time.Minute {
		t.Errorf("interval = %s after a round trip", got.Telemetry.Interval.Duration())
	}
}

func TestTelemetryIntervalRejectsNonsense(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, config.FileName)
	if err := os.WriteFile(path, []byte("[telemetry]\ninterval = \"a while\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := config.LoadFile(path)
	if err == nil {
		t.Fatal("\"a while\" was accepted as a duration")
	}
	if !strings.Contains(err.Error(), "60s") {
		t.Errorf("the error does not say what a duration looks like: %v", err)
	}

	// And a negative one is a typo, not an intention. Clamping it silently
	// would leave the typo in the file for the next person.
	c := config.Config{Telemetry: config.Telemetry{Interval: config.Duration(-time.Second)}}
	if err := c.Validate(); err == nil {
		t.Error("a negative interval was accepted")
	}
}

func TestWranglerQueueValidation(t *testing.T) {
	for _, spelling := range []string{"wrangler", "pm"} {
		on := func(q config.Queue) config.Queue {
			if spelling == "wrangler" {
				q.Wrangler = true
			} else {
				q.PM = true
			}
			return q
		}
		for _, tc := range []struct {
			kind      string
			enabled   bool
			wantError bool
		}{
			{"claude", false, false}, {"claude", true, false}, {"codex", true, false}, {"grok", true, true}, {"codex", false, false},
		} {
			q := config.Queue{Name: "mac-agent", AgentKind: tc.kind}
			if tc.enabled {
				q = on(q)
			}
			err := config.Config{Queues: []config.Queue{q}}.Validate()
			if (err != nil) != tc.wantError {
				t.Errorf("%s %s=%v: %v", tc.kind, spelling, tc.enabled, err)
			}
			// The error quotes the line the file actually has.
			if tc.wantError && !strings.Contains(err.Error(), spelling+" = true requires agent_kind = claude or codex") {
				t.Fatal(err)
			}
		}
		c := config.Config{Queues: []config.Queue{on(config.Queue{Name: "mac-claude", AgentKind: "claude", MCPServers: []string{"extra"}})}}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "only their owner Elk connector") {
			t.Fatalf("%s: a Wrangler queue allowed extra servers", spelling)
		}
	}
}

// TestWranglerQueueTOML is the alias as a config file spells it:
// `wrangler = true` is the documented key, `pm = true` the old one that Elk
// Scout's live Wrangler queue still says, and each — with its connector
// account key — must read as exactly the same queue.
func TestWranglerQueueTOML(t *testing.T) {
	for _, tc := range []struct {
		name, body  string
		wantEnabled bool
		wantAccount string
	}{
		{"neither", "", false, ""},
		{"new key", "wrangler = true\nwrangler_connector_account = \"owner\"", true, "owner"},
		{"old key", "pm = true\npm_connector_account = \"owner\"", true, "owner"},
		{"new key, default binding", "wrangler = true", true, ""},
		{"old key, default binding", "pm = true", true, ""},
		{"both, agreeing", "wrangler = true\npm = true", true, ""},
		{"both off", "wrangler = false\npm = false", false, ""},
		{"new on, old account", "wrangler = true\npm_connector_account = \"owner\"", true, "owner"},
		{"old on, new account", "pm = true\nwrangler_connector_account = \"owner\"", true, "owner"},
		{"both accounts, agreeing", "wrangler = true\nwrangler_connector_account = \"owner\"\npm_connector_account = \"owner\"", true, "owner"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loadQueueTOML(t, tc.body)
			if err != nil {
				t.Fatal(err)
			}
			q := cfg.Queues[0]
			if q.IsWrangler() != tc.wantEnabled {
				t.Errorf("IsWrangler = %v, want %v", q.IsWrangler(), tc.wantEnabled)
			}
			if q.WranglerAccount() != tc.wantAccount {
				t.Errorf("WranglerAccount = %q, want %q", q.WranglerAccount(), tc.wantAccount)
			}
		})
	}
}

// TestWranglerSpellingsThatDisagreeAreRefused: the case that matters is
// `wrangler = false` written to turn the Wrangler off beside a `pm = true`
// left behind. Reading the two as "either" would leave production authority
// on, so a disagreement refuses the file instead of guessing.
func TestWranglerSpellingsThatDisagreeAreRefused(t *testing.T) {
	for _, tc := range []struct {
		name, body, want string
	}{
		{"turned off under the new key", "wrangler = false\npm = true", "wrangler = false and pm = true disagree"},
		{"turned on beside an old false", "wrangler = true\npm = false", "wrangler = true and pm = false disagree"},
		{"two accounts", "wrangler = true\nwrangler_connector_account = \"a\"\npm_connector_account = \"b\"", "name different keychain accounts"},
		{"new account without the opt-in", "wrangler_connector_account = \"a\"", "wrangler_connector_account requires wrangler = true"},
		{"old account without the opt-in", "pm_connector_account = \"a\"", "pm_connector_account requires wrangler = true"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := loadQueueTOML(t, tc.body)
			if err == nil {
				t.Fatal("accepted")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error does not say %q: %v", tc.want, err)
			}
		})
	}
}

// TestWranglerSpellingSurvivesARewrite: `rein enrol` rewrites the whole file,
// and a queue that says `pm = true` must come back saying `pm = true` — not
// silently moved to the new key, which the release before this one would not
// read — while the new key round-trips as itself. Neither key is written for
// a queue that has the Wrangler off.
func TestWranglerSpellingSurvivesARewrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	want := config.Config{HostID: "mac", Queues: []config.Queue{
		{Name: "mac-claude", AgentKind: "claude", PM: true, PMConnectorAccount: "owner"},
		{Name: "acme-claude", AgentKind: "claude", Wrangler: true},
		{Name: "mac-codex", AgentKind: "codex"},
	}}
	if err := want.SaveFile(path); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	blob, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(blob)
	if strings.Count(text, "pm = true") != 1 || strings.Count(text, "wrangler = true") != 1 {
		t.Errorf("each spelling should be written once, as it was set:\n%s", text)
	}
	if strings.Contains(text, "pm = false") || strings.Contains(text, "wrangler = false") || strings.Contains(text, `connector_account = ""`) {
		t.Errorf("an unset opt-in was written out:\n%s", text)
	}
	got, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	if !got.Queues[0].PM || got.Queues[0].Wrangler || got.Queues[0].WranglerAccount() != "owner" {
		t.Errorf("pm = true did not round-trip as itself: %+v", got.Queues[0])
	}
	if !got.Queues[1].Wrangler || got.Queues[1].PM {
		t.Errorf("wrangler = true did not round-trip as itself: %+v", got.Queues[1])
	}
	if got.Queues[2].IsWrangler() {
		t.Error("a queue without the opt-in came back a Wrangler")
	}
}

// TestWranglerCannotBeListedAsACapability: the opt-in is the only way to
// declare the Wrangler's wire capability, `pm`, and `wrangler` is no
// capability at all.
func TestWranglerCannotBeListedAsACapability(t *testing.T) {
	for _, name := range []string{"pm", "wrangler"} {
		c := config.Config{Capabilities: []string{name}}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "wrangler = true") {
			t.Errorf("machine-wide %s: %v", name, err)
		}
		c = config.Config{Queues: []config.Queue{{Name: "mac-claude", AgentKind: "claude", Capabilities: []string{name}}}}
		if err := c.Validate(); err == nil || !strings.Contains(err.Error(), "wrangler = true") {
			t.Errorf("per-queue %s on an ordinary queue: %v", name, err)
		}
	}
	// On a Wrangler queue `pm` is redundant but true; `wrangler` is still not a capability.
	ok := config.Config{Queues: []config.Queue{{Name: "mac-claude", AgentKind: "claude", Wrangler: true, Capabilities: []string{"pm"}}}}
	if err := ok.Validate(); err != nil {
		t.Errorf("pm listed on a Wrangler queue: %v", err)
	}
	bad := config.Config{Queues: []config.Queue{{Name: "mac-claude", AgentKind: "claude", Wrangler: true, Capabilities: []string{"wrangler"}}}}
	if err := bad.Validate(); err == nil || !strings.Contains(err.Error(), "not a capability") {
		t.Errorf("wrangler listed on a Wrangler queue: %v", err)
	}
}

// TestLandParse covers the per-queue landing policy (ark:rein#47) as a config
// file actually spells it: each of the three values decodes, and a queue that
// never mentions it — every queue enrolled before the setting existed — reads
// as merge, so Elk's own queues behave exactly as they did.
func TestLandParse(t *testing.T) {
	for _, tc := range []struct {
		name, line, wantField, wantPolicy string
	}{
		{"unset", "", "", config.LandMerge},
		{"merge", `land = "merge"`, config.LandMerge, config.LandMerge},
		{"pr", `land = "pr"`, config.LandPR, config.LandPR},
		{"branch", `land = "branch"`, config.LandBranch, config.LandBranch},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			body := "[[queues]]\nname = \"mac-codex\"\nagent_kind = \"codex\"\n" + tc.line + "\n"
			if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadFile(path)
			if err != nil {
				t.Fatalf("LoadFile: %v", err)
			}
			q := cfg.Queues[0]
			if q.Land != tc.wantField {
				t.Errorf("Land = %q, want %q", q.Land, tc.wantField)
			}
			if got := q.LandOrDefault(); got != tc.wantPolicy {
				t.Errorf("LandOrDefault = %q, want %q", got, tc.wantPolicy)
			}
		})
	}
	if config.DefaultLand != config.LandMerge {
		t.Fatalf("DefaultLand = %q: the default must stay merge, or every existing queue changes behaviour", config.DefaultLand)
	}
}

// TestLandRejectsUnknownValues: a typo is refused at load with the line to
// fix, never read as the default. `land = "PR"` quietly becoming merge would
// merge into a repository whose owners asked for a pull request.
func TestLandRejectsUnknownValues(t *testing.T) {
	for _, bad := range []string{"PR", "pull-request", "squash", "none", " pr"} {
		path := filepath.Join(t.TempDir(), "config.toml")
		body := "[[queues]]\nname = \"mac-codex\"\nagent_kind = \"codex\"\nland = \"" + bad + "\"\n"
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := config.LoadFile(path)
		if err == nil {
			t.Errorf("land = %q was accepted", bad)
			continue
		}
		if !strings.Contains(err.Error(), "want merge, pr or branch") || !strings.Contains(err.Error(), "mac-codex") {
			t.Errorf("land = %q: the error does not say what to write instead, or where: %v", bad, err)
		}
	}
}

// TestLandRoundTrips: `rein enrol` rewrites the whole file, so a policy a
// person set must survive a save and a load.
func TestLandRoundTrips(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.toml")
	want := config.Config{HostID: "mac", Queues: []config.Queue{
		{Name: "mac-claude", AgentKind: "claude"},
		{Name: "mac-codex", AgentKind: "codex", Land: config.LandPR},
		{Name: "mac-grok", AgentKind: "grok", Land: config.LandBranch},
	}}
	if err := want.SaveFile(path); err != nil {
		t.Fatalf("SaveFile: %v", err)
	}
	got, err := config.LoadFile(path)
	if err != nil {
		t.Fatalf("LoadFile: %v", err)
	}
	for i, q := range want.Queues {
		if got.Queues[i].Land != q.Land {
			t.Errorf("%s: Land = %q after a round trip, want %q", q.Name, got.Queues[i].Land, q.Land)
		}
	}
	if got.Queues[0].LandOrDefault() != config.LandMerge {
		t.Errorf("an unset policy did not survive a round trip as merge")
	}
}

func TestWorkspaceQueueConfig(t *testing.T) {
	for _, multi := range []bool{false, true} {
		t.Run(fmtBool(multi), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			text := `workspace = "Scout"
[repos]
"elk-work/scout" = "/dev/scout"
"friend/gallery" = "/dev/gallery"
[[queues]]
name = "mac-claude"
agent_kind = "claude"
`
			if multi {
				text += `repos = ["elk-work/scout"]
[[queues]]
name = "mac-claude"
agent_kind = "claude"
workspace = "Gallery"
repos = ["friend/gallery"]
`
			}
			if err := os.WriteFile(path, []byte(text), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			q, ok := cfg.QueueIn("Scout", "mac-claude")
			if !ok {
				t.Fatal("missing queue")
			}
			repos := cfg.RepositoriesFor(q)
			want := 2
			if multi {
				want = 1
				if _, ok := cfg.Queue("mac-claude"); ok {
					t.Fatal("ambiguous name accepted")
				}
			}
			if len(repos) != want {
				t.Fatalf("repos = %v", repos)
			}
			if err := cfg.SaveFile(path); err != nil {
				t.Fatal(err)
			}
			if _, err := config.LoadFile(path); err != nil {
				t.Fatal(err)
			}
		})
	}
}
func fmtBool(b bool) string {
	if b {
		return "scoped"
	}
	return "legacy"
}

// TestRepositoryCompatibility loads the three shapes a config's repository
// rules can take (ark:rein#67): one workspace with no lists, several
// workspaces with no lists — the shape v0.8.4 stranded — and lists.
func TestRepositoryCompatibility(t *testing.T) {
	const head = `workspace = "Elk Scout"
default_repo = "/dev/elk"
[repos]
"elk-work/scout" = "/dev/scout"
signal = "/dev/signal"
[[queues]]
name = "mac-claude"
agent_kind = "claude"
`
	const signalQueue = `[[queues]]
name = "mac-codex"
agent_kind = "codex"
workspace = "Signal"
`
	for _, tc := range []struct {
		name, text, mode string
		warn, scoped     bool
		scout, signal    int // repositories each workspace's queue sees
	}{
		{"single workspace, no lists", head, "unscoped", false, false, 2, 2},
		{"several workspaces, no lists", head + signalQueue, "compatibility", true, false, 2, 2},
		{"several workspaces, one list", head + `repos = ["elk-work/scout"]
` + signalQueue, "scoped", false, true, 1, 0},
		{"several workspaces, every list", head + `repos = ["elk-work/scout"]
` + signalQueue + `repos = ["signal"]
`, "scoped", false, true, 1, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(path, []byte(tc.text), 0o600); err != nil {
				t.Fatal(err)
			}
			cfg, err := config.LoadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if got := cfg.RepositoryMode(); got != tc.mode {
				t.Errorf("mode = %q, want %q", got, tc.mode)
			}
			warning := cfg.RepositoryCompatibilityWarning()
			if (warning != "") != tc.warn || cfg.RepositoryCompatibility() != tc.warn {
				t.Errorf("warning = %q, compatibility = %v; want a warning: %v", warning, cfg.RepositoryCompatibility(), tc.warn)
			}
			if tc.warn && !strings.Contains(warning, "add repos = [...] to each [[queues]] entry") {
				t.Errorf("the warning does not name the fix: %q", warning)
			}
			for i, want := range []int{tc.scout, tc.signal} {
				if i >= len(cfg.Queues) {
					continue
				}
				q := cfg.Queues[i]
				if got := cfg.RepositoriesFor(q); len(got) != want {
					t.Errorf("%s sees %v, want %d repositories", cfg.QueueLabel(q), got, want)
				}
				if got := cfg.RepositoryScopeRequired(q); got != tc.scoped {
					t.Errorf("%s scoped = %v, want %v", cfg.QueueLabel(q), got, tc.scoped)
				}
			}
		})
	}
}
