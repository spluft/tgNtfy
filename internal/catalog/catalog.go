// Package catalog loads and serves the events.yaml catalog describing, per service, the
// event types it may emit and their default severity. The catalog is runtime config:
// loaded at startup and re-read on SIGHUP, so services/event types can be added
// without a re-release.
package catalog

import (
	"errors"
	"fmt"
	"os"
	"sync"

	"gopkg.in/yaml.v3"
)

// Severity values allowed in catalog entries.
const (
	SeverityInfo    = "info"
	SeverityWarn    = "warn"
	SeverityError   = "error"
	SeveritySuccess = "success"
)

// EventType describes one event type a service may emit.
type EventType struct {
	// Severity is the catalog default severity (used for /menu grouping and docs only);
	// the event's own severity field always drives rendering.
	Severity string `yaml:"severity"`
	// Drop, when true, means events of this (service,type) are accepted at ingest
	// but not delivered (counted, logged). No v1 catalog entry sets it.
	Drop bool `yaml:"drop"`
}

// Service describes one registered service in the catalog.
type Service struct {
	DisplayName string               `yaml:"display_name"`
	Events      map[string]EventType `yaml:"events"`
}

// Catalog is a validated snapshot of an events.yaml file.
type Catalog struct {
	Version  int                `yaml:"version"`
	Services map[string]Service `yaml:"services"`
}

// Lookup is a concurrency-safe holder of the current catalog snapshot. Reload on SIGHUP
// swaps the snapshot only when the new file parses and validates; else the old is kept.
type Lookup struct {
	mu   sync.RWMutex
	cat  *Catalog
	path string
}

// NewLookup returns a Lookup seeded with cat.
func NewLookup(cat *Catalog) *Lookup { return &Lookup{cat: cat} }

// Get returns the current catalog snapshot.
func (l *Lookup) Get() *Catalog {
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.cat
}

// Load reads, parses and validates the catalog file at path.
func Load(path string) (*Catalog, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read catalog %s: %w", path, err)
	}
	var c Catalog
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("parse catalog %s: %w", path, err)
	}
	if err := c.Validate(); err != nil {
		return nil, fmt.Errorf("catalog %s: %w", path, err)
	}
	return &c, nil
}

// Reload re-reads the catalog file at path and swaps the snapshot on success.
func (l *Lookup) Reload() error {
	c, err := Load(l.path)
	if err != nil {
		return err
	}
	l.mu.Lock()
	l.cat = c
	l.mu.Unlock()
	return nil
}

// SetPath records the catalog path for later Reload calls.
func (l *Lookup) SetPath(path string) { l.path = path }

// Validate checks structural invariants of the catalog.
func (c *Catalog) Validate() error {
	if c.Version != 1 {
		return fmt.Errorf("unsupported catalog version %d (want 1)", c.Version)
	}
	for id, svc := range c.Services {
		if id == "" {
			return errors.New("catalog: empty service id")
		}
		if svc.DisplayName == "" {
			return fmt.Errorf("catalog service %q: empty display_name", id)
		}
		for t, ev := range svc.Events {
			switch ev.Severity {
			case SeverityInfo, SeverityWarn, SeverityError, SeveritySuccess:
			default:
				return fmt.Errorf("catalog service %q type %q: invalid default severity %q", id, t, ev.Severity)
			}
		}
	}
	return nil
}

// DisplayName returns the display name for a service, falling back to the service id.
func (c *Catalog) DisplayName(service string) string {
	for id, svc := range c.Services {
		if id == service {
			return svc.DisplayName
		}
	}
	return service
}

// TypeFlag returns the catalog entry for (service,type), if present.
func (c *Catalog) TypeFlag(service, typ string) (EventType, bool) {
	svc, ok := c.Services[service]
	if !ok {
		return EventType{}, false
	}
	et, ok := svc.Events[typ]
	return et, ok
}

// IsKnown reports whether the given (service,type) combination exists in the catalog.
// Unknown combinations are accepted at ingest (A-4) but not by POST /v1/link.
func (c *Catalog) IsKnown(service, typ string) bool {
	_, ok := c.TypeFlag(service, typ)
	return ok
}

// ServiceTypes returns the event type ids declared for a service (unordered).
func (c *Catalog) ServiceTypes(service string) []string {
	svc, ok := c.Services[service]
	if !ok {
		return nil
	}
	out := make([]string, 0, len(svc.Events))
	for t := range svc.Events {
		out = append(out, t)
	}
	return out
}
