package main

import (
	"testing"

	"github.com/jalberto/qilla/internal/config"
)

func TestGuardDecisionPrecedence(t *testing.T) {
	g := config.Guard{
		Deny:  []string{`rm -rf /`},
		Allow: []string{`ssh known-host `, `rm -rf /`},
		Ask:   []string{`ssh `},
	}

	if dec, _ := guardDecision(g, "rm -rf /"); dec != "deny" {
		t.Fatalf("deny must beat allow, got %q", dec)
	}
	if dec, _ := guardDecision(g, "ssh known-host uptime"); dec != "" {
		t.Fatalf("allow must beat ask, got %q", dec)
	}
	if dec, _ := guardDecision(g, "ssh other-host uptime"); dec != "ask" {
		t.Fatalf("ask must still fire when nothing allows, got %q", dec)
	}
	if dec, _ := guardDecision(g, "echo hi"); dec != "" {
		t.Fatalf("plain command must pass, got %q", dec)
	}
}
