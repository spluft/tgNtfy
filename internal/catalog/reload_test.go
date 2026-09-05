package catalog

import (
	"os"
	"path/filepath"
	"testing"
)

func TestReloadKeepsPreviousOnError(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "c.yaml")
	os.WriteFile(path, []byte("version: 1\nservices:\n  govpn:\n    display_name: VPN\n    events:\n      vpn_connected:\n        severity: success\n"), 0o644)
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	lk := NewLookup(c)
	lk.SetPath(path)
	if lk.Get().Services["govpn"].DisplayName != "VPN" {
		t.Fatal("initial snapshot not loaded")
	}
	os.WriteFile(path, []byte("version: 9\nnonsense\n"), 0o644)
	if err := lk.Reload(); err == nil {
		t.Fatal("Reload should fail on invalid catalog")
	}
	if lk.Get().Services["govpn"].DisplayName != "VPN" {
		t.Fatal("previous snapshot must be kept on reload error")
	}
}

func TestValidateRejectsEmptyDisplayName(t *testing.T) {
	c := &Catalog{Version: 1, Services: map[string]Service{"x": {DisplayName: ""}}}
	if err := c.Validate(); err == nil {
		t.Fatal("expected error for empty display_name")
	}
}

func TestServiceTypesAndTypeFlag(t *testing.T) {
	c := &Catalog{Version: 1, Services: map[string]Service{
		"govpn": {DisplayName: "VPN", Events: map[string]EventType{
			"vpn_connected": {Severity: "success"},
			"vpn_dropped":   {Severity: "warn"},
		}},
	}}
	if et, ok := c.TypeFlag("govpn", "vpn_connected"); !ok || et.Severity != "success" {
		t.Fatalf("TypeFlag: ok=%v et=%+v", ok, et)
	}
	if _, ok := c.TypeFlag("govpn", "nope"); ok {
		t.Fatal("unknown type must not flag")
	}
	if types := c.ServiceTypes("govpn"); len(types) != 2 {
		t.Fatalf("ServiceTypes: %v", types)
	}
	if got := c.ServiceTypes("unknown"); got != nil {
		t.Fatalf("ServiceTypes unknown: %v", got)
	}
	if c.DisplayName("unknown") != "unknown" {
		t.Fatal("DisplayName fallback should return the id")
	}
}
