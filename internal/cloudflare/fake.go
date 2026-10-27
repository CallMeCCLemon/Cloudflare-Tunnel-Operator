package cloudflare

import (
	"context"
	"fmt"
	"net/http"
	"sync"
)

// Fake is an in-memory API for tests.
type Fake struct {
	mu      sync.Mutex
	nextID  int
	Tunnels map[string]*Tunnel
	Configs map[string]TunnelConfig
	Records map[string]*DNSRecord
	// ConfigPushes counts UpdateTunnelConfig calls.
	ConfigPushes int
}

var _ API = (*Fake)(nil)

// NewFake returns an empty Fake.
func NewFake() *Fake {
	return &Fake{
		Tunnels: map[string]*Tunnel{},
		Configs: map[string]TunnelConfig{},
		Records: map[string]*DNSRecord{},
	}
}

// Factory returns a Factory that always hands out this Fake.
func (f *Fake) Factory() Factory { return func(string) API { return f } }

func (f *Fake) id(prefix string) string {
	f.nextID++
	return fmt.Sprintf("%s-%d", prefix, f.nextID)
}

func notFound() error { return &APIError{StatusCode: http.StatusNotFound} }

func (f *Fake) FindTunnelByName(_ context.Context, _, name string) (*Tunnel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, t := range f.Tunnels {
		if t.Name == name {
			c := *t
			return &c, nil
		}
	}
	return nil, nil
}

func (f *Fake) GetTunnel(_ context.Context, _, id string) (*Tunnel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t, ok := f.Tunnels[id]
	if !ok {
		return nil, notFound()
	}
	c := *t
	return &c, nil
}

func (f *Fake) CreateTunnel(_ context.Context, _, name string) (*Tunnel, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &Tunnel{ID: f.id("tunnel"), Name: name, Status: "inactive"}
	f.Tunnels[t.ID] = t
	c := *t
	return &c, nil
}

func (f *Fake) DeleteTunnel(_ context.Context, _, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.Tunnels[id]; !ok {
		return notFound()
	}
	delete(f.Tunnels, id)
	delete(f.Configs, id)
	return nil
}

func (f *Fake) CleanupTunnelConnections(context.Context, string, string) error { return nil }

func (f *Fake) GetTunnelToken(_ context.Context, _, id string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.Tunnels[id]; !ok {
		return "", notFound()
	}
	return "token-for-" + id, nil
}

func (f *Fake) UpdateTunnelConfig(_ context.Context, _, id string, cfg TunnelConfig) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.Tunnels[id]; !ok {
		return notFound()
	}
	f.Configs[id] = cfg
	f.ConfigPushes++
	return nil
}

func (f *Fake) FindDNSRecord(_ context.Context, _, name string) (*DNSRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.Records {
		if r.Name == name {
			c := *r
			return &c, nil
		}
	}
	return nil, nil
}

func (f *Fake) CreateDNSRecord(_ context.Context, _ string, rec DNSRecord) (*DNSRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	rec.ID = f.id("dns")
	f.Records[rec.ID] = &rec
	c := rec
	return &c, nil
}

func (f *Fake) UpdateDNSRecord(_ context.Context, _, id string, rec DNSRecord) (*DNSRecord, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.Records[id]; !ok {
		return nil, notFound()
	}
	rec.ID = id
	f.Records[id] = &rec
	c := rec
	return &c, nil
}

func (f *Fake) DeleteDNSRecord(_ context.Context, _, id string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, ok := f.Records[id]; !ok {
		return notFound()
	}
	delete(f.Records, id)
	return nil
}
