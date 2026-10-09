package config_test

import (
	"github.com/elk-work/rein/internal/config"
	"testing"
	"time"
)

func TestHostedBillingRequiresBothConfigAndFlag(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, flag := range []bool{false, true} {
			c := config.Config{Hosted: config.Hosted{Enabled: enabled}}
			if err := c.CheckHosted(flag); (err == nil) != (enabled == flag) {
				t.Fatalf("enabled=%v flag=%v err=%v", enabled, flag, err)
			}
		}
	}
}
func TestHostedConfigAuthAndDefaults(t *testing.T) {
	c := config.Config{Hosted: config.Hosted{Enabled: true, AllowRepos: []string{"acme/repo"}},
		Capabilities: []string{"ANTHROPIC_API_KEY"}, InheritEnv: []string{"ANTHROPIC_API_KEY"},
		Queues: []config.Queue{{Name: "cloud-claude", AgentKind: "claude"}}}
	if err := c.Validate(); err != nil {
		t.Fatal(err)
	}
	if c.Hosted.RunCap() != 120*time.Minute {
		t.Fatal("wrong default cap")
	}
	c.Queues[0].Secrets = map[string]string{"ANTHROPIC_API_KEY": "model-key"}
	if err := c.Validate(); err != nil {
		t.Fatal("hosted API-key secret name refused", err)
	}
	c.Queues[0].Secrets = nil
	c.Hosted.Enabled = false
	if err := c.Validate(); err == nil {
		t.Fatal("unhosted API key accepted")
	}
	c.Hosted.Enabled = true
	c.Queues[0].AgentKind = "grok"
	if err := c.Validate(); err == nil {
		t.Fatal("hosted Grok accepted")
	}
	c.Queues[0].AgentKind = "claude"
	c.Capabilities = []string{"ANTHROPIC_AUTH_TOKEN"}
	if err := c.Validate(); err == nil {
		t.Fatal("hosted auth token accepted")
	}
}
