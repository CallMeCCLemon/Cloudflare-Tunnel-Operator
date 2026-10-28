// Package cloudflare wraps the parts of the Cloudflare API the operator needs,
// Cloudflare Tunnels and DNS records, behind the small API interface the
// controllers use. Client implements it with the official cloudflare-go SDK;
// Fake implements it in memory for tests.
package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	cfsdk "github.com/cloudflare/cloudflare-go/v7"
	"github.com/cloudflare/cloudflare-go/v7/dns"
	"github.com/cloudflare/cloudflare-go/v7/option"
	"github.com/cloudflare/cloudflare-go/v7/zero_trust"
)

// DefaultBaseURL is the Cloudflare v4 API.
const DefaultBaseURL = "https://api.cloudflare.com/client/v4"

const (
	// maxRetries applies to 408, 409, 429 and 5xx responses and connection errors;
	// the SDK backs off between attempts and honours Retry-After.
	maxRetries     = 3
	requestTimeout = 30 * time.Second
)

// Tunnel is a Cloudflare tunnel.
type Tunnel struct {
	ID     string
	Name   string
	Status string
}

// IngressRule is one entry of a tunnel's ingress configuration.
type IngressRule struct {
	Hostname      string         `json:"hostname,omitempty"`
	Path          string         `json:"path,omitempty"`
	Service       string         `json:"service"`
	OriginRequest *OriginRequest `json:"originRequest,omitempty"`
}

// OriginRequest is the per-rule origin configuration.
type OriginRequest struct {
	NoTLSVerify      bool   `json:"noTLSVerify,omitempty"`
	HTTP2Origin      bool   `json:"http2Origin,omitempty"`
	HTTPHostHeader   string `json:"httpHostHeader,omitempty"`
	OriginServerName string `json:"originServerName,omitempty"`
}

// TunnelConfig is a remotely-managed tunnel configuration. The JSON tags make it
// hashable for change detection; they are not what goes over the wire.
type TunnelConfig struct {
	Ingress []IngressRule `json:"ingress"`
}

// DNSRecord is a Cloudflare DNS record. The operator only ever writes CNAMEs, but
// reads any address record so it can spot conflicts.
type DNSRecord struct {
	ID      string
	Type    string
	Name    string
	Content string
	Proxied bool
	TTL     int
	Comment string
}

// API is everything the controllers use; Client implements it and tests
// substitute Fake.
type API interface {
	FindTunnelByName(ctx context.Context, accountID, name string) (*Tunnel, error)
	GetTunnel(ctx context.Context, accountID, tunnelID string) (*Tunnel, error)
	CreateTunnel(ctx context.Context, accountID, name string) (*Tunnel, error)
	RenameTunnel(ctx context.Context, accountID, tunnelID, name string) (*Tunnel, error)
	DeleteTunnel(ctx context.Context, accountID, tunnelID string) error
	CleanupTunnelConnections(ctx context.Context, accountID, tunnelID string) error
	GetTunnelToken(ctx context.Context, accountID, tunnelID string) (string, error)
	UpdateTunnelConfig(ctx context.Context, accountID, tunnelID string, cfg TunnelConfig) error

	FindDNSRecords(ctx context.Context, zoneID, name string) ([]DNSRecord, error)
	GetDNSRecord(ctx context.Context, zoneID, recordID string) (*DNSRecord, error)
	CreateDNSRecord(ctx context.Context, zoneID string, rec DNSRecord) (*DNSRecord, error)
	UpdateDNSRecord(ctx context.Context, zoneID, recordID string, rec DNSRecord) (*DNSRecord, error)
	DeleteDNSRecord(ctx context.Context, zoneID, recordID string) error
}

// Factory builds an API client from an API token.
type Factory func(apiToken string) API

// NewFactory returns a Factory producing SDK-backed clients against baseURL.
func NewFactory(baseURL string) Factory {
	return func(apiToken string) API { return NewClient(baseURL, apiToken) }
}

// Client implements API with the cloudflare-go SDK.
type Client struct {
	tunnels *zero_trust.TunnelCloudflaredService
	records *dns.RecordService
}

// NewClient returns a Client. An empty baseURL uses DefaultBaseURL.
//
// It builds only the two services it needs, and deliberately not via
// cfsdk.NewClient: that also reads CLOUDFLARE_API_TOKEN,
// CLOUDFLARE_API_KEY and CLOUDFLARE_EMAIL from the environment, which could add a
// second set of credentials. The token always comes from the ClusterTunnel's Secret.
func NewClient(baseURL, apiToken string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	opts := []option.RequestOption{
		option.WithHTTPClient(&http.Client{}),
		option.WithBaseURL(strings.TrimRight(baseURL, "/") + "/"),
		option.WithAPIToken(apiToken),
		option.WithMaxRetries(maxRetries),
		option.WithRequestTimeout(requestTimeout),
	}
	return &Client{
		tunnels: zero_trust.NewTunnelCloudflaredService(opts...),
		records: dns.NewRecordService(opts...),
	}
}

// APIError is a non-success response. The SDK returns *cfsdk.Error; the Fake
// returns this, so IsNotFound treats both alike.
type APIError struct {
	StatusCode int
	Message    string
}

func (e *APIError) Error() string {
	return fmt.Sprintf("cloudflare API returned %d: %s", e.StatusCode, e.Message)
}

// IsNotFound reports whether err is a 404 from Cloudflare.
func IsNotFound(err error) bool {
	var sdkErr *cfsdk.Error
	if errors.As(err, &sdkErr) {
		return sdkErr.StatusCode == http.StatusNotFound
	}
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

func tunnelFrom(t *zero_trust.CloudflareTunnel) *Tunnel {
	return &Tunnel{ID: t.ID, Name: t.Name, Status: string(t.Status)}
}

// FindTunnelByName returns the non-deleted tunnel with that name, or nil.
func (c *Client) FindTunnelByName(ctx context.Context, accountID, name string) (*Tunnel, error) {
	iter := c.tunnels.ListAutoPaging(ctx, zero_trust.TunnelCloudflaredListParams{
		AccountID: cfsdk.F(accountID),
		Name:      cfsdk.F(name),
		IsDeleted: cfsdk.F(false),
	})
	for iter.Next() {
		if t := iter.Current(); t.Name == name {
			return tunnelFrom(&t), nil
		}
	}
	return nil, iter.Err()
}

// GetTunnel fetches a tunnel by ID.
func (c *Client) GetTunnel(ctx context.Context, accountID, tunnelID string) (*Tunnel, error) {
	t, err := c.tunnels.Get(ctx, tunnelID, zero_trust.TunnelCloudflaredGetParams{AccountID: cfsdk.F(accountID)})
	if err != nil {
		return nil, err
	}
	return tunnelFrom(t), nil
}

// CreateTunnel creates a remotely-managed tunnel (config lives in Cloudflare).
func (c *Client) CreateTunnel(ctx context.Context, accountID, name string) (*Tunnel, error) {
	t, err := c.tunnels.New(ctx, zero_trust.TunnelCloudflaredNewParams{
		AccountID: cfsdk.F(accountID),
		Name:      cfsdk.F(name),
		ConfigSrc: cfsdk.F(zero_trust.TunnelCloudflaredNewParamsConfigSrcCloudflare),
	})
	if err != nil {
		return nil, err
	}
	return tunnelFrom(t), nil
}

// RenameTunnel changes a tunnel's name. The ID, token, connectors and DNS
// records are unaffected; the name is only a label.
func (c *Client) RenameTunnel(ctx context.Context, accountID, tunnelID, name string) (*Tunnel, error) {
	t, err := c.tunnels.Edit(ctx, tunnelID, zero_trust.TunnelCloudflaredEditParams{
		AccountID: cfsdk.F(accountID),
		Name:      cfsdk.F(name),
	})
	if err != nil {
		return nil, err
	}
	return tunnelFrom(t), nil
}

// DeleteTunnel deletes a tunnel. It must have no active connections.
func (c *Client) DeleteTunnel(ctx context.Context, accountID, tunnelID string) error {
	_, err := c.tunnels.Delete(ctx, tunnelID, zero_trust.TunnelCloudflaredDeleteParams{AccountID: cfsdk.F(accountID)})
	return err
}

// CleanupTunnelConnections removes stale connector registrations.
func (c *Client) CleanupTunnelConnections(ctx context.Context, accountID, tunnelID string) error {
	_, err := c.tunnels.Connections.Delete(ctx, tunnelID,
		zero_trust.TunnelCloudflaredConnectionDeleteParams{AccountID: cfsdk.F(accountID)})
	return err
}

// GetTunnelToken returns the token cloudflared uses to run the tunnel.
func (c *Client) GetTunnelToken(ctx context.Context, accountID, tunnelID string) (string, error) {
	token, err := c.tunnels.Token.Get(ctx, tunnelID, zero_trust.TunnelCloudflaredTokenGetParams{AccountID: cfsdk.F(accountID)})
	if err != nil {
		return "", err
	}
	return *token, nil
}

// UpdateTunnelConfig replaces the tunnel's ingress configuration. Unset fields
// are omitted, so the catch-all rule goes out as just {"service": "http_status:404"}.
func (c *Client) UpdateTunnelConfig(ctx context.Context, accountID, tunnelID string, cfg TunnelConfig) error {
	rules := make([]zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress, 0, len(cfg.Ingress))
	for _, in := range cfg.Ingress {
		rule := zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngress{Service: cfsdk.F(in.Service)}
		if in.Hostname != "" {
			rule.Hostname = cfsdk.F(in.Hostname)
		}
		if in.Path != "" {
			rule.Path = cfsdk.F(in.Path)
		}
		if o := in.OriginRequest; o != nil {
			var or zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfigIngressOriginRequest
			if o.NoTLSVerify {
				or.NoTLSVerify = cfsdk.F(true)
			}
			if o.HTTP2Origin {
				or.HTTP2Origin = cfsdk.F(true)
			}
			if o.HTTPHostHeader != "" {
				or.HTTPHostHeader = cfsdk.F(o.HTTPHostHeader)
			}
			if o.OriginServerName != "" {
				or.OriginServerName = cfsdk.F(o.OriginServerName)
			}
			rule.OriginRequest = cfsdk.F(or)
		}
		rules = append(rules, rule)
	}
	_, err := c.tunnels.Configurations.Update(ctx, tunnelID, zero_trust.TunnelCloudflaredConfigurationUpdateParams{
		AccountID: cfsdk.F(accountID),
		Config: cfsdk.F(zero_trust.TunnelCloudflaredConfigurationUpdateParamsConfig{
			Ingress: cfsdk.F(rules),
		}),
	})
	return err
}

func recordFrom(r *dns.RecordResponse) *DNSRecord {
	return &DNSRecord{
		ID:      r.ID,
		Type:    string(r.Type),
		Name:    r.Name,
		Content: r.Content,
		Proxied: r.Proxied,
		TTL:     int(r.TTL),
		Comment: r.Comment,
	}
}

// FindDNSRecords returns every CNAME/A/AAAA record named exactly name. Normally
// there is at most one, but duplicates can exist (for example after a create that
// the list endpoint hadn't caught up with), and callers must handle them.
func (c *Client) FindDNSRecords(ctx context.Context, zoneID, name string) ([]DNSRecord, error) {
	iter := c.records.ListAutoPaging(ctx, dns.RecordListParams{
		ZoneID: cfsdk.F(zoneID),
		Name:   cfsdk.F(dns.RecordListParamsName{Exact: cfsdk.F(name)}),
	})
	var recs []DNSRecord
	for iter.Next() {
		r := iter.Current()
		recs = append(recs, *recordFrom(&r))
	}
	if err := iter.Err(); err != nil {
		return nil, err
	}
	return filterAddressRecords(recs, name), nil
}

// filterAddressRecords keeps CNAME/A/AAAA records for name. The name check guards
// against the API's filter semantics changing; never trust it to scope a delete.
func filterAddressRecords(recs []DNSRecord, name string) []DNSRecord {
	out := recs[:0]
	for _, r := range recs {
		if !strings.EqualFold(r.Name, name) {
			continue
		}
		switch r.Type {
		case "CNAME", "A", "AAAA":
			out = append(out, r)
		}
	}
	return out
}

// GetDNSRecord fetches a record by ID. Unlike listing, this sees a record
// immediately after it is created.
func (c *Client) GetDNSRecord(ctx context.Context, zoneID, recordID string) (*DNSRecord, error) {
	r, err := c.records.Get(ctx, recordID, dns.RecordGetParams{ZoneID: cfsdk.F(zoneID)})
	if err != nil {
		return nil, err
	}
	return recordFrom(r), nil
}

func cnameParam(rec DNSRecord) (dns.CNAMERecordParam, error) {
	if rec.Type != "CNAME" {
		return dns.CNAMERecordParam{}, fmt.Errorf("only CNAME records can be written, got %q", rec.Type)
	}
	ttl := rec.TTL
	if ttl == 0 {
		ttl = 1 // automatic
	}
	return dns.CNAMERecordParam{
		Type:    cfsdk.F(dns.CNAMERecordTypeCNAME),
		Name:    cfsdk.F(rec.Name),
		Content: cfsdk.F(rec.Content),
		Proxied: cfsdk.F(rec.Proxied),
		TTL:     cfsdk.F(dns.TTL(ttl)),
		Comment: cfsdk.F(rec.Comment),
	}, nil
}

// CreateDNSRecord creates a CNAME record.
func (c *Client) CreateDNSRecord(ctx context.Context, zoneID string, rec DNSRecord) (*DNSRecord, error) {
	body, err := cnameParam(rec)
	if err != nil {
		return nil, err
	}
	r, err := c.records.New(ctx, dns.RecordNewParams{ZoneID: cfsdk.F(zoneID), Body: body})
	if err != nil {
		return nil, err
	}
	return recordFrom(r), nil
}

// UpdateDNSRecord overwrites a record with a CNAME. Cloudflare allows changing the
// type this way, which is how an adopted A record becomes the tunnel's CNAME.
func (c *Client) UpdateDNSRecord(ctx context.Context, zoneID, recordID string, rec DNSRecord) (*DNSRecord, error) {
	body, err := cnameParam(rec)
	if err != nil {
		return nil, err
	}
	r, err := c.records.Update(ctx, recordID, dns.RecordUpdateParams{ZoneID: cfsdk.F(zoneID), Body: body})
	if err != nil {
		return nil, err
	}
	return recordFrom(r), nil
}

// DeleteDNSRecord deletes a record.
func (c *Client) DeleteDNSRecord(ctx context.Context, zoneID, recordID string) error {
	_, err := c.records.Delete(ctx, recordID, dns.RecordDeleteParams{ZoneID: cfsdk.F(zoneID)})
	return err
}
