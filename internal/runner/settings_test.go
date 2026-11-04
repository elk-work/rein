package runner

import (
	"strings"
	"testing"

	"github.com/elk-work/rein/internal/config"
	"github.com/elk-work/rein/internal/elk"
)

func settingsCfg(q config.Queue) config.Config {
	q.Workspace = "Elk Scout"
	if q.Name == "" {
		q.Name = "mac-claude"
	}
	if q.AgentKind == "" {
		q.AgentKind = "claude"
	}
	return config.Config{HostID: "mac", Workspace: "Elk Scout", Queues: []config.Queue{q}}
}

func ptr(s string) *string { return &s }

func TestApplyElkSettingsSetsLandAndManagedSecrets(t *testing.T) {
	cfg := settingsCfg(config.Queue{Secrets: map[string]string{"MINE": "acme-key"}})
	next, changed, err := ApplyElkSettings(cfg, "Elk Scout", "mac-claude",
		elk.WorkerSettings{Land: ptr("pr"), Secrets: []string{"POSTHOG_API_KEY", "MINE"}})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	q, _ := next.QueueIn("Elk Scout", "mac-claude")
	if q.Land != config.LandPR {
		t.Errorf("land = %q", q.Land)
	}
	if q.Secrets["POSTHOG_API_KEY"] != "elk-secret/POSTHOG_API_KEY" {
		t.Errorf("managed secret = %q", q.Secrets["POSTHOG_API_KEY"])
	}
	if q.Secrets["MINE"] != "acme-key" {
		t.Errorf("a person's own mapping was replaced: %q", q.Secrets["MINE"])
	}
	// The original is untouched: the caller decides whether to save.
	if orig, _ := cfg.QueueIn("Elk Scout", "mac-claude"); orig.Land != "" {
		t.Errorf("ApplyElkSettings changed its input")
	}

	// Removing a name in the app removes only the managed entry.
	again, changed, err := ApplyElkSettings(next, "Elk Scout", "mac-claude", elk.WorkerSettings{Secrets: nil})
	if err != nil || !changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	q, _ = again.QueueIn("Elk Scout", "mac-claude")
	if _, ok := q.Secrets["POSTHOG_API_KEY"]; ok || q.Secrets["MINE"] != "acme-key" || q.Land != config.LandPR {
		t.Errorf("after removal: land=%q secrets=%v", q.Land, q.Secrets)
	}
}

func TestApplyElkSettingsIsANoOpWhenNothingChanged(t *testing.T) {
	cfg := settingsCfg(config.Queue{Land: config.LandMerge})
	_, changed, err := ApplyElkSettings(cfg, "Elk Scout", "mac-claude", elk.WorkerSettings{Land: ptr("merge")})
	if err != nil || changed {
		t.Fatalf("changed=%v err=%v", changed, err)
	}
	_, changed, _ = ApplyElkSettings(cfg, "Elk Scout", "mac-claude", elk.WorkerSettings{})
	if changed {
		t.Fatal("nil land and no secrets changed the queue")
	}
}

func TestApplyElkSettingsRefusesWhatWouldNotLoad(t *testing.T) {
	cfg := settingsCfg(config.Queue{})
	if _, changed, err := ApplyElkSettings(cfg, "Elk Scout", "mac-claude", elk.WorkerSettings{Land: ptr("yolo")}); err == nil || changed {
		t.Fatal("a bad landing policy was accepted")
	}
	inherit := settingsCfg(config.Queue{InheritEnv: []string{"FOO"}})
	_, changed, err := ApplyElkSettings(inherit, "Elk Scout", "mac-claude", elk.WorkerSettings{Secrets: []string{"X"}})
	if err == nil || changed || !strings.Contains(err.Error(), "inherit_env") {
		t.Fatalf("secrets on an inherit_env queue: changed=%v err=%v", changed, err)
	}
	if _, _, err := ApplyElkSettings(cfg, "Elk Scout", "other", elk.WorkerSettings{}); err == nil {
		t.Fatal("an unknown queue was accepted")
	}
}
