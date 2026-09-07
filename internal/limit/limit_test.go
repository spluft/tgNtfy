package limit

import (
	"testing"
	"time"
)

func TestBurstCap30(t *testing.T) {
	l := New()
	now := time.Now()
	for i := 0; i < 30; i++ {
		if !l.Allow(now) {
			t.Fatalf("allow %d failed within first 30", i)
		}
	}
	if l.Allow(now) {
		t.Fatal("31st in same second should be rejected (burst cap 30)")
	}
	// After sliding past 1s window, burst tokens free.
	if !l.Allow(now.Add(1100 * time.Millisecond)) {
		t.Fatal("after 1s burst should reset")
	}
}

func TestSustainedCap100(t *testing.T) {
	l := New()
	base := time.Now()
	for i := 0; i < 100; i++ {
		l.Allow(base.Add(time.Duration(i) * time.Millisecond))
	}
	if l.Allow(base.Add(101 * time.Millisecond)) {
		t.Fatal("101st in minute should be rejected")
	}
	// After 60s the early ones expire.
	if !l.Allow(base.Add(61 * time.Second)) {
		t.Fatal("after 60s should reset rate window")
	}
}

func TestRegistryPerServiceIndependent(t *testing.T) {
	r := NewRegistry()
	now := time.Now()
	for i := 0; i < 30; i++ {
		r.AllowForService("a", now)
	}
	if r.AllowForService("a", now) {
		t.Fatal("service a should be burst-limited")
	}
	if !r.AllowForService("b", now) {
		t.Fatal("service b must be unaffected by a")
	}
}
