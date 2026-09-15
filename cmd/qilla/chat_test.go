package main

import (
	"strings"
	"testing"
	"time"

	"github.com/jalberto/qilla/internal/config"
)

func TestChatEnvAutocompact(t *testing.T) {
	n := 70
	cfg := &config.Config{Chat: config.Chat{AutocompactPct: &n}}
	env := chatEnv(cfg, "chief", []string{"CLAUDE_CODE_SSE_PORT=9", "CLAUDECODE=1", "PATH=/bin"})
	var claudeVars []string
	for _, kv := range env {
		if strings.HasPrefix(kv, "CLAUDE") {
			claudeVars = append(claudeVars, kv)
		}
	}
	if len(claudeVars) != 1 || claudeVars[0] != "CLAUDE_AUTOCOMPACT_PCT_OVERRIDE=70" {
		t.Fatalf("want only the override, got %v", claudeVars)
	}
	if !contains(env, "QILLA_RUN=1") || !contains(env, "PATH=/bin") {
		t.Fatalf("markers or inherited env lost: %v", env)
	}

	zero := 0
	cfg.Chat.AutocompactPct = &zero
	for _, kv := range chatEnv(cfg, "chief", nil) {
		if strings.HasPrefix(kv, "CLAUDE") {
			t.Fatalf("0 must leave Claude Code's default, got %q", kv)
		}
	}
	cfg.Chat.AutocompactPct = nil
	for _, kv := range chatEnv(cfg, "chief", nil) {
		if strings.HasPrefix(kv, "CLAUDE") {
			t.Fatalf("unset must not emit an override, got %q", kv)
		}
	}
}

func contains(ss []string, s string) bool {
	for _, x := range ss {
		if x == s {
			return true
		}
	}
	return false
}

func TestChatAutocompactValidate(t *testing.T) {
	mk := func(n int) *config.Config {
		c := &config.Config{Chat: config.Chat{AutocompactPct: &n}}
		c.Memory.Backend = "sqlite"
		return c
	}
	if err := mk(70).Validate(); err != nil {
		t.Fatalf("70 should validate: %v", err)
	}
	if err := mk(0).Validate(); err != nil {
		t.Fatalf("0 should validate: %v", err)
	}
	if err := mk(25).Validate(); err != nil {
		t.Fatalf("25 should validate: %v", err)
	}
	if err := mk(10).Validate(); err != nil {
		t.Fatalf("10 should validate: %v", err)
	}
	if err := mk(9).Validate(); err == nil {
		t.Fatal("9 should fail validation")
	}
	if err := mk(96).Validate(); err == nil {
		t.Fatal("96 should fail validation")
	}
}

func TestChatSessionMaxIdle(t *testing.T) {
	d, err := config.Chat{}.MaxIdle()
	if err != nil || d != config.DefaultSessionMaxIdle {
		t.Fatalf("unset = %v, %v; want 6h", d, err)
	}
	if d, err := (config.Chat{SessionMaxIdle: "0"}).MaxIdle(); err != nil || d != 0 {
		t.Fatalf(`"0" = %v, %v; want never`, d, err)
	}
	if d, err := (config.Chat{SessionMaxIdle: "90m"}).MaxIdle(); err != nil || d != 90*time.Minute {
		t.Fatalf(`"90m" = %v, %v`, d, err)
	}
	c := &config.Config{Chat: config.Chat{SessionMaxIdle: "later"}}
	c.Memory.Backend = "sqlite"
	if err := c.Validate(); err == nil {
		t.Fatal("a bad duration should fail validation")
	}
}
