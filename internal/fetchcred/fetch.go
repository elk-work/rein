// Package fetchcred holds a claim's ephemeral, single-repository credential.
package fetchcred

import (
	"encoding/base64"
	"strings"
	"time"
)

type Credential struct {
	Kind      string `json:"kind"`
	Repo      string `json:"repo"`
	URL       string `json:"url"`
	Username  string `json:"username"`
	Token     string `json:"token"`
	ExpiresAt string `json:"expires_at"`
}

func (c *Credential) Valid(repo string) bool {
	if c == nil {
		return false
	}
	expires, err := time.Parse(time.RFC3339, c.ExpiresAt)
	return err == nil && c.Kind == "github-token" && c.Repo == repo &&
		c.URL == "https://github.com/"+repo+".git" && c.Username == "x-access-token" &&
		len(c.Token) >= 8 && !strings.ContainsAny(c.Token, "\r\n") && expires.After(time.Now())
}

func (c *Credential) Secrets() map[string]string {
	if c == nil {
		return nil
	}
	return map[string]string{"fetch_token": c.Token, "fetch_basic": base64.StdEncoding.EncodeToString([]byte(c.Username + ":" + c.Token))}
}

func (c *Credential) Header() string {
	return "Authorization: Basic " + c.Secrets()["fetch_basic"]
}
