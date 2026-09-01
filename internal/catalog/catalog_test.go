package catalog

import (
	"path/filepath"
	"testing"
)

func TestLoadAndValidate(t *testing.T) {
	p := filepath.Join("..", "..", "config", "events.yaml")
	c, err := Load(p)
	if err != nil {
		t.Fatalf("load config/events.yaml: %v", err)
	}
	want := map[string]int{
		"goyoutube": 3, "gomail": 2, "gorecomendarr": 2, "govpn": 2,
	}
	for svc, n := range want {
		if len(c.Services[svc].Events) != n {
			t.Errorf("service %s: want %d events, got %d", svc, n, len(c.Services[svc].Events))
		}
	}
	if c.DisplayName("goyoutube") != "goYouTube" {
		t.Errorf("display name wrong: %s", c.DisplayName("goyoutube"))
	}
	if !c.IsKnown("govpn", "vpn_connected") {
		t.Error("govpn/vpn_connected should be known")
	}
	if c.IsKnown("govpn", "job_progress") {
		t.Error("job_progress must not be a v1 catalog entry (D-3)")
	}
	if c.IsKnown("kidsedu", "task_submitted") {
		t.Error("kidsedu must not be in v1 catalog (D-4)")
	}
}

func TestValidateRejectsBadSeverity(t *testing.T) {
	c := &Catalog{Version: 1, Services: map[string]Service{
		"bad": {DisplayName: "B", Events: map[string]EventType{"e": {Severity: "hmm"}}},
	}}
	if err := c.Validate(); err == nil {
		t.Fatal("expected validation error for unknown severity")
	}
}

func TestValidateRejectsBadVersion(t *testing.T) {
	c := &Catalog{Version: 2}
	if err := c.Validate(); err == nil {
		t.Fatal("expected version error")
	}
}
