// Package cloudflare is a minimal client for the parts of the Cloudflare API the
// operator needs: Cloudflare Tunnels (cfd_tunnel) and DNS records.
package cloudflare

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is the Cloudflare v4 API.
const DefaultBaseURL = "https://api.cloudflare.com/client/v4"

// Tunnel is a Cloudflare tunnel as returned by the API.
type Tunnel struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Status string `json:"status"`
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

// TunnelConfig is a remotely-managed tunnel configuration.
type TunnelConfig struct {
	Ingress []IngressRule `json:"ingress"`
}

// DNSRecord is a Cloudflare DNS record.
type DNSRecord struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Name    string `json:"name"`
	Content string `json:"content"`
	Proxied bool   `json:"proxied"`
	TTL     int    `json:"ttl,omitempty"`
	Comment string `json:"comment,omitempty"`
}

// API is everything the controllers use; the HTTP client implements it and
// tests substitute a fake.
type API interface {
	FindTunnelByName(ctx context.Context, accountID, name string) (*Tunnel, error)
	GetTunnel(ctx context.Context, accountID, tunnelID string) (*Tunnel, error)
	CreateTunnel(ctx context.Context, accountID, name string) (*Tunnel, error)
	DeleteTunnel(ctx context.Context, accountID, tunnelID string) error
	CleanupTunnelConnections(ctx context.Context, accountID, tunnelID string) error
	GetTunnelToken(ctx context.Context, accountID, tunnelID string) (string, error)
	UpdateTunnelConfig(ctx context.Context, accountID, tunnelID string, cfg TunnelConfig) error

	FindDNSRecord(ctx context.Context, zoneID, name string) (*DNSRecord, error)
	CreateDNSRecord(ctx context.Context, zoneID string, rec DNSRecord) (*DNSRecord, error)
	UpdateDNSRecord(ctx context.Context, zoneID, recordID string, rec DNSRecord) (*DNSRecord, error)
	DeleteDNSRecord(ctx context.Context, zoneID, recordID string) error
}

// Factory builds an API client from an API token.
type Factory func(apiToken string) API

// NewFactory returns a Factory producing HTTP clients against baseURL.
func NewFactory(baseURL string) Factory {
	return func(apiToken string) API { return NewClient(baseURL, apiToken) }
}

// Client talks to the Cloudflare REST API.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewClient returns a Client. An empty baseURL uses DefaultBaseURL.
func NewClient(baseURL, apiToken string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{
		baseURL: strings.TrimRight(baseURL, "/"),
		token:   apiToken,
		http:    &http.Client{Timeout: 30 * time.Second},
	}
}

// APIError is a non-success response from Cloudflare.
type APIError struct {
	StatusCode int
	Errors     []struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
}

func (e *APIError) Error() string {
	msgs := make([]string, 0, len(e.Errors))
	for _, m := range e.Errors {
		msgs = append(msgs, fmt.Sprintf("%d: %s", m.Code, m.Message))
	}
	return fmt.Sprintf("cloudflare API returned %d: %s", e.StatusCode, strings.Join(msgs, "; "))
}

// IsNotFound reports whether err is a 404 from Cloudflare.
func IsNotFound(err error) bool {
	var apiErr *APIError
	return errors.As(err, &apiErr) && apiErr.StatusCode == http.StatusNotFound
}

type envelope struct {
	Success bool            `json:"success"`
	Errors  json.RawMessage `json:"errors"`
	Result  json.RawMessage `json:"result"`
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	var reader io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("encoding request: %w", err)
		}
		reader = bytes.NewReader(b)
	}

	req, err := http.NewRequestWithContext(ctx, method, u, reader)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return fmt.Errorf("reading response: %w", err)
	}

	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("%s %s: unexpected response (%d): %w", method, path, resp.StatusCode, err)
	}
	if resp.StatusCode >= 300 || !env.Success {
		apiErr := &APIError{StatusCode: resp.StatusCode}
		_ = json.Unmarshal(env.Errors, &apiErr.Errors)
		return apiErr
	}
	if out != nil && len(env.Result) > 0 {
		if err := json.Unmarshal(env.Result, out); err != nil {
			return fmt.Errorf("decoding result: %w", err)
		}
	}
	return nil
}

// escapedSuffix renders path segments as "/a/b", escaping each one.
func escapedSuffix(parts []string) string {
	var b strings.Builder
	for _, part := range parts {
		b.WriteString("/")
		b.WriteString(url.PathEscape(part))
	}
	return b.String()
}

func tunnelPath(accountID string, parts ...string) string {
	return "/accounts/" + url.PathEscape(accountID) + "/cfd_tunnel" + escapedSuffix(parts)
}

// FindTunnelByName returns the non-deleted tunnel with that name, or nil.
func (c *Client) FindTunnelByName(ctx context.Context, accountID, name string) (*Tunnel, error) {
	var tunnels []Tunnel
	q := url.Values{"name": {name}, "is_deleted": {"false"}}
	if err := c.do(ctx, http.MethodGet, tunnelPath(accountID), q, nil, &tunnels); err != nil {
		return nil, err
	}
	for i := range tunnels {
		if tunnels[i].Name == name {
			return &tunnels[i], nil
		}
	}
	return nil, nil
}

// GetTunnel fetches a tunnel by ID.
func (c *Client) GetTunnel(ctx context.Context, accountID, tunnelID string) (*Tunnel, error) {
	var t Tunnel
	if err := c.do(ctx, http.MethodGet, tunnelPath(accountID, tunnelID), nil, nil, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// CreateTunnel creates a remotely-managed tunnel (config lives in Cloudflare).
func (c *Client) CreateTunnel(ctx context.Context, accountID, name string) (*Tunnel, error) {
	var t Tunnel
	body := map[string]string{"name": name, "config_src": "cloudflare"}
	if err := c.do(ctx, http.MethodPost, tunnelPath(accountID), nil, body, &t); err != nil {
		return nil, err
	}
	return &t, nil
}

// DeleteTunnel deletes a tunnel. It must have no active connections.
func (c *Client) DeleteTunnel(ctx context.Context, accountID, tunnelID string) error {
	return c.do(ctx, http.MethodDelete, tunnelPath(accountID, tunnelID), nil, nil, nil)
}

// CleanupTunnelConnections removes stale connector registrations.
func (c *Client) CleanupTunnelConnections(ctx context.Context, accountID, tunnelID string) error {
	return c.do(ctx, http.MethodDelete, tunnelPath(accountID, tunnelID, "connections"), nil, nil, nil)
}

// GetTunnelToken returns the token cloudflared uses to run the tunnel.
func (c *Client) GetTunnelToken(ctx context.Context, accountID, tunnelID string) (string, error) {
	var token string
	if err := c.do(ctx, http.MethodGet, tunnelPath(accountID, tunnelID, "token"), nil, nil, &token); err != nil {
		return "", err
	}
	return token, nil
}

// UpdateTunnelConfig replaces the tunnel's ingress configuration.
func (c *Client) UpdateTunnelConfig(ctx context.Context, accountID, tunnelID string, cfg TunnelConfig) error {
	body := map[string]TunnelConfig{"config": cfg}
	return c.do(ctx, http.MethodPut, tunnelPath(accountID, tunnelID, "configurations"), nil, body, nil)
}

func dnsPath(zoneID string, parts ...string) string {
	return "/zones/" + url.PathEscape(zoneID) + "/dns_records" + escapedSuffix(parts)
}

// FindDNSRecord returns the CNAME/A/AAAA record for name, or nil.
func (c *Client) FindDNSRecord(ctx context.Context, zoneID, name string) (*DNSRecord, error) {
	var recs []DNSRecord
	if err := c.do(ctx, http.MethodGet, dnsPath(zoneID), url.Values{"name": {name}}, nil, &recs); err != nil {
		return nil, err
	}
	for i := range recs {
		switch recs[i].Type {
		case "CNAME", "A", "AAAA":
			return &recs[i], nil
		}
	}
	return nil, nil
}

// CreateDNSRecord creates a record.
func (c *Client) CreateDNSRecord(ctx context.Context, zoneID string, rec DNSRecord) (*DNSRecord, error) {
	var out DNSRecord
	if err := c.do(ctx, http.MethodPost, dnsPath(zoneID), nil, rec, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// UpdateDNSRecord overwrites a record.
func (c *Client) UpdateDNSRecord(ctx context.Context, zoneID, recordID string, rec DNSRecord) (*DNSRecord, error) {
	var out DNSRecord
	if err := c.do(ctx, http.MethodPut, dnsPath(zoneID, recordID), nil, rec, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// DeleteDNSRecord deletes a record.
func (c *Client) DeleteDNSRecord(ctx context.Context, zoneID, recordID string) error {
	return c.do(ctx, http.MethodDelete, dnsPath(zoneID, recordID), nil, nil, nil)
}
