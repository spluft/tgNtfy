package admin

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spluft/tgNtfy/internal/store"
)

func TestServiceCreateListEnableDisableRotate(t *testing.T) {
	db := filepath.Join(t.TempDir(), "a.db")
	var out, errb bytes.Buffer
	Run([]string{db, "service", "create", "govpn", "--name", "VPN"}, &out, &errb)
	if errb.Len() > 0 {
		t.Fatalf("create stderr: %s", errb.String())
	}
	if !strings.Contains(out.String(), "created service") || !strings.Contains(out.String(), "RAW TOKEN") {
		t.Fatalf("create stdout: %s", out.String())
	}
	parts := strings.Split(out.String(), "RAW TOKEN (print once):\n")
	if len(parts) != 2 || len(strings.TrimSpace(parts[1])) != 64 {
		t.Fatalf("token unexpected: %q", out.String())
	}
	out.Reset(); errb.Reset()
	Run([]string{db, "service", "list"}, &out, &errb)
	if !strings.Contains(out.String(), "govpn") || !strings.Contains(out.String(), "enabled") {
		t.Fatalf("list stdout: %s", out.String())
	}
	out.Reset(); errb.Reset()
	Run([]string{db, "service", "disable", "govpn"}, &out, &errb)
	if errb.Len() > 0 || !strings.Contains(out.String(), "disabled govpn") {
		t.Fatalf("disable: out=%s err=%s", out.String(), errb.String())
	}
	out.Reset(); errb.Reset()
	Run([]string{db, "service", "list"}, &out, &errb)
	if !strings.Contains(out.String(), "disabled") {
		t.Fatalf("list after disable: %s", out.String())
	}
	out.Reset(); errb.Reset()
	Run([]string{db, "service", "enable", "govpn"}, &out, &errb)
	if errb.Len() > 0 || !strings.Contains(out.String(), "enabled govpn") {
		t.Fatalf("enable: out=%s err=%s", out.String(), errb.String())
	}
	out.Reset(); errb.Reset()
	Run([]string{db, "service", "rotate", "govpn"}, &out, &errb)
	if errb.Len() > 0 || !strings.Contains(out.String(), "rotated") || !strings.Contains(out.String(), "RAW TOKEN") {
		t.Fatalf("rotate: out=%s err=%s", out.String(), errb.String())
	}
}

func TestAdminLinkUnlinkUserEvents(t *testing.T) {
	db := filepath.Join(t.TempDir(), "b.db")
	var out, errb bytes.Buffer
	Run([]string{db, "service", "create", "gomail", "--name", "Mail"}, &out, &errb)
	st, err := store.New(db)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpsertUser(context.Background(), 555, "alice", "Alice"); err != nil {
		t.Fatal(err)
	}
	gc := int64(9001)
	if err := st.SetDeliveryMode(context.Background(), 555, "group", &gc); err != nil {
		t.Fatal(err)
	}
	st.Close()
	out.Reset(); errb.Reset()
	Run([]string{db, "link", "gomail", "17", "555"}, &out, &errb)
	if errb.Len() > 0 || !strings.Contains(out.String(), "linked gomail") {
		t.Fatalf("link: out=%s err=%s", out.String(), errb.String())
	}
	out.Reset(); errb.Reset()
	Run([]string{db, "user"}, &out, &errb)
	if !strings.Contains(out.String(), "user=555") || !strings.Contains(out.String(), "group=9001") {
		t.Fatalf("user list: %s", out.String())
	}
	out.Reset(); errb.Reset()
	Run([]string{db, "events", "-n", "5"}, &out, &errb)
	if errb.Len() > 0 {
		t.Fatalf("events: %s", errb.String())
	}
	out.Reset(); errb.Reset()
	Run([]string{db, "unlink", "gomail", "17"}, &out, &errb)
	if errb.Len() > 0 || !strings.Contains(out.String(), "unlinked gomail") {
		t.Fatalf("unlink: out=%s err=%s", out.String(), errb.String())
	}
}

func TestAdminEventsDefaultLimit(t *testing.T) {
	db := filepath.Join(t.TempDir(), "c.db")
	var out, errb bytes.Buffer
	Run([]string{db, "events"}, &out, &errb)
	if errb.Len() > 0 {
		t.Fatalf("events default stderr: %s", errb.String())
	}
}
