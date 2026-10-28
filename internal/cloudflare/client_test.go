package cloudflare

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"
)

// recordedRequest is what the stub server saw.
type recordedRequest struct {
	method, path, query, auth string
	body                      map[string]any
}

// stubServer answers every request with the next scripted response (the last one
// repeats) and records what it received. Like the real API, it answers a request
// for page 2 or later with an empty page: the SDK's auto-pager stops only on an
// empty page, so every list call makes one extra request, counted in pageFetches.
type stubServer struct {
	*httptest.Server
	mu          sync.Mutex
	requests    []recordedRequest
	responses   []stubResponse
	pageFetches int
}

type stubResponse struct {
	status int
	body   string
}

func newStubServer(t *testing.T, responses ...stubResponse) *stubServer {
	t.Helper()
	s := &stubServer{responses: responses}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		req := recordedRequest{method: r.Method, path: r.URL.Path, query: r.URL.RawQuery, auth: r.Header.Get("Authorization")}
		if len(raw) > 0 {
			if err := json.Unmarshal(raw, &req.body); err != nil {
				t.Errorf("request body is not JSON: %s", raw)
			}
		}

		s.mu.Lock()
		var resp stubResponse
		if page := r.URL.Query().Get("page"); page != "" && page != "1" {
			resp = ok(`[]`)
			s.pageFetches++
		} else {
			resp = s.responses[min(len(s.requests), len(s.responses)-1)]
			s.requests = append(s.requests, req)
		}
		s.mu.Unlock()

		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.status)
		_, _ = io.WriteString(w, resp.body)
	}))
	t.Cleanup(s.Close)
	return s
}

func ok(result string) stubResponse {
	return stubResponse{status: http.StatusOK, body: `{"success":true,"errors":[],"messages":[],"result":` + result + `}`}
}

const (
	tunnelJSON = `{"id":"abc","name":"home","status":"healthy"}`
	recordJSON = `{"id":"r1","type":"CNAME","name":"app.example.com","content":"abc.cfargotunnel.com",` +
		`"proxied":true,"ttl":1,"comment":"managed-by=cloudflare-tunnel-operator tunnel=home"}`
)

var wantRecord = &DNSRecord{
	ID: "r1", Type: "CNAME", Name: "app.example.com", Content: "abc.cfargotunnel.com",
	Proxied: true, TTL: 1, Comment: "managed-by=cloudflare-tunnel-operator tunnel=home",
}

// TestClientRequests checks, for every API method, the request the SDK-backed
// client sends and how it maps the response.
func TestClientRequests(t *testing.T) {
	ctx := context.Background()
	cname := DNSRecord{
		Type: "CNAME", Name: "app.example.com", Content: "abc.cfargotunnel.com",
		Proxied: true, TTL: 1, Comment: "managed-by=cloudflare-tunnel-operator tunnel=home",
	}
	wantCNAMEBody := map[string]any{
		"type": "CNAME", "name": "app.example.com", "content": "abc.cfargotunnel.com",
		"proxied": true, "ttl": 1.0, "comment": "managed-by=cloudflare-tunnel-operator tunnel=home",
	}

	tests := []struct {
		name       string
		response   stubResponse
		call       func(c *Client) (any, error)
		wantMethod string
		wantPath   string
		wantQuery  []string // substrings of the raw query
		wantBody   map[string]any
		want       any
	}{
		{
			name:       "FindTunnelByName",
			response:   ok(`[{"id":"other","name":"home-2"},` + tunnelJSON + `]`),
			call:       func(c *Client) (any, error) { return c.FindTunnelByName(ctx, "acct", "home") },
			wantMethod: http.MethodGet, wantPath: "/accounts/acct/cfd_tunnel",
			wantQuery: []string{"name=home", "is_deleted=false"},
			want:      &Tunnel{ID: "abc", Name: "home", Status: "healthy"},
		},
		{
			name:       "FindTunnelByName with no match",
			response:   ok(`[]`),
			call:       func(c *Client) (any, error) { return c.FindTunnelByName(ctx, "acct", "home") },
			wantMethod: http.MethodGet, wantPath: "/accounts/acct/cfd_tunnel",
			want: (*Tunnel)(nil),
		},
		{
			name:       "GetTunnel",
			response:   ok(tunnelJSON),
			call:       func(c *Client) (any, error) { return c.GetTunnel(ctx, "acct", "abc") },
			wantMethod: http.MethodGet, wantPath: "/accounts/acct/cfd_tunnel/abc",
			want: &Tunnel{ID: "abc", Name: "home", Status: "healthy"},
		},
		{
			name:       "CreateTunnel",
			response:   ok(`{"id":"abc","name":"home","status":"inactive"}`),
			call:       func(c *Client) (any, error) { return c.CreateTunnel(ctx, "acct", "home") },
			wantMethod: http.MethodPost, wantPath: "/accounts/acct/cfd_tunnel",
			wantBody: map[string]any{"name": "home", "config_src": "cloudflare"},
			want:     &Tunnel{ID: "abc", Name: "home", Status: "inactive"},
		},
		{
			name:       "RenameTunnel",
			response:   ok(`{"id":"abc","name":"latentlab","status":"healthy"}`),
			call:       func(c *Client) (any, error) { return c.RenameTunnel(ctx, "acct", "abc", "latentlab") },
			wantMethod: http.MethodPatch, wantPath: "/accounts/acct/cfd_tunnel/abc",
			wantBody: map[string]any{"name": "latentlab"},
			want:     &Tunnel{ID: "abc", Name: "latentlab", Status: "healthy"},
		},
		{
			name:       "DeleteTunnel",
			response:   ok(tunnelJSON),
			call:       func(c *Client) (any, error) { return nil, c.DeleteTunnel(ctx, "acct", "abc") },
			wantMethod: http.MethodDelete, wantPath: "/accounts/acct/cfd_tunnel/abc",
		},
		{
			name:       "CleanupTunnelConnections",
			response:   ok(`{}`),
			call:       func(c *Client) (any, error) { return nil, c.CleanupTunnelConnections(ctx, "acct", "abc") },
			wantMethod: http.MethodDelete, wantPath: "/accounts/acct/cfd_tunnel/abc/connections",
		},
		{
			name:       "GetTunnelToken",
			response:   ok(`"eyJhIjoi"`),
			call:       func(c *Client) (any, error) { return c.GetTunnelToken(ctx, "acct", "abc") },
			wantMethod: http.MethodGet, wantPath: "/accounts/acct/cfd_tunnel/abc/token",
			want: "eyJhIjoi",
		},
		{
			name:     "UpdateTunnelConfig",
			response: ok(`{}`),
			call: func(c *Client) (any, error) {
				return nil, c.UpdateTunnelConfig(ctx, "acct", "abc", TunnelConfig{Ingress: []IngressRule{
					{Hostname: "app.example.com", Path: "^/api/", Service: "https://api.ns.svc.cluster.local:443",
						OriginRequest: &OriginRequest{NoTLSVerify: true, HTTP2Origin: true}},
					{Hostname: "app.example.com", Service: "http://web.ns.svc.cluster.local:80"},
					{Service: "http_status:404"},
				}})
			},
			wantMethod: http.MethodPut, wantPath: "/accounts/acct/cfd_tunnel/abc/configurations",
			// Unset fields must be omitted: in particular the catch-all has no hostname.
			wantBody: map[string]any{"config": map[string]any{"ingress": []any{
				map[string]any{"hostname": "app.example.com", "path": "^/api/", "service": "https://api.ns.svc.cluster.local:443",
					"originRequest": map[string]any{"noTLSVerify": true, "http2Origin": true}},
				map[string]any{"hostname": "app.example.com", "service": "http://web.ns.svc.cluster.local:80"},
				map[string]any{"service": "http_status:404"},
			}}},
		},
		{
			name: "FindDNSRecords filters by exact name and type",
			response: ok(`[
				{"id":"t1","type":"TXT","name":"app.example.com","content":"x"},
				` + recordJSON + `,
				{"id":"o1","type":"CNAME","name":"other.example.com","content":"abc.cfargotunnel.com"}
			]`),
			call:       func(c *Client) (any, error) { return c.FindDNSRecords(ctx, "zone", "app.example.com") },
			wantMethod: http.MethodGet, wantPath: "/zones/zone/dns_records",
			wantQuery: []string{"name.exact=app.example.com"},
			want:      []DNSRecord{*wantRecord},
		},
		{
			name:       "GetDNSRecord",
			response:   ok(recordJSON),
			call:       func(c *Client) (any, error) { return c.GetDNSRecord(ctx, "zone", "r1") },
			wantMethod: http.MethodGet, wantPath: "/zones/zone/dns_records/r1",
			want: wantRecord,
		},
		{
			name:       "CreateDNSRecord",
			response:   ok(recordJSON),
			call:       func(c *Client) (any, error) { return c.CreateDNSRecord(ctx, "zone", cname) },
			wantMethod: http.MethodPost, wantPath: "/zones/zone/dns_records",
			wantBody: wantCNAMEBody,
			want:     wantRecord,
		},
		{
			name:       "UpdateDNSRecord",
			response:   ok(recordJSON),
			call:       func(c *Client) (any, error) { return c.UpdateDNSRecord(ctx, "zone", "r1", cname) },
			wantMethod: http.MethodPut, wantPath: "/zones/zone/dns_records/r1",
			wantBody: wantCNAMEBody,
			want:     wantRecord,
		},
		{
			name:       "DeleteDNSRecord",
			response:   ok(`{"id":"r1"}`),
			call:       func(c *Client) (any, error) { return nil, c.DeleteDNSRecord(ctx, "zone", "r1") },
			wantMethod: http.MethodDelete, wantPath: "/zones/zone/dns_records/r1",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv := newStubServer(t, tt.response)
			got, err := tt.call(NewClient(srv.URL, "secret"))
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}

			if len(srv.requests) != 1 {
				t.Fatalf("got %d requests, want 1", len(srv.requests))
			}
			req := srv.requests[0]
			if req.method != tt.wantMethod || req.path != tt.wantPath {
				t.Errorf("request = %s %s, want %s %s", req.method, req.path, tt.wantMethod, tt.wantPath)
			}
			if req.auth != "Bearer secret" {
				t.Errorf("Authorization = %q", req.auth)
			}
			for _, q := range tt.wantQuery {
				if !strings.Contains(req.query, q) {
					t.Errorf("query %q does not contain %q", req.query, q)
				}
			}
			if tt.wantBody != nil && !reflect.DeepEqual(req.body, tt.wantBody) {
				t.Errorf("body = %v\nwant  %v", req.body, tt.wantBody)
			}
			if tt.want != nil && !reflect.DeepEqual(got, tt.want) {
				t.Errorf("result = %#v\nwant    %#v", got, tt.want)
			}
		})
	}
}

func TestClientNotFound(t *testing.T) {
	srv := newStubServer(t, stubResponse{
		status: http.StatusNotFound,
		body:   `{"success":false,"errors":[{"code":1003,"message":"Tunnel not found"}],"messages":[],"result":null}`,
	})

	_, err := NewClient(srv.URL, "secret").GetTunnel(context.Background(), "acct", "missing")
	if !IsNotFound(err) {
		t.Fatalf("IsNotFound(%v) = false", err)
	}
	if !strings.Contains(err.Error(), "Tunnel not found") {
		t.Errorf("error %q should include Cloudflare's message", err)
	}
	if len(srv.requests) != 1 {
		t.Errorf("a 404 must not be retried; got %d requests", len(srv.requests))
	}
}

func TestClientRetriesRateLimits(t *testing.T) {
	srv := newStubServer(t,
		stubResponse{status: http.StatusTooManyRequests, body: `{"success":false,"errors":[{"code":971,"message":"rate limited"}]}`},
		ok(tunnelJSON),
	)

	got, err := NewClient(srv.URL, "secret").GetTunnel(context.Background(), "acct", "abc")
	if err != nil {
		t.Fatalf("expected the retry to succeed: %v", err)
	}
	if got.ID != "abc" || len(srv.requests) != 2 {
		t.Fatalf("got %+v after %d requests, want tunnel abc after 2", got, len(srv.requests))
	}
}

func TestClientRejectsNonCNAMEWrites(t *testing.T) {
	srv := newStubServer(t, ok(recordJSON))
	_, err := NewClient(srv.URL, "secret").CreateDNSRecord(context.Background(), "zone", DNSRecord{Type: "A", Name: "x", Content: "1.2.3.4"})
	if err == nil || len(srv.requests) != 0 {
		t.Fatalf("want an error and no request; got err=%v, %d requests", err, len(srv.requests))
	}
}

func TestIsNotFoundMatchesFakeErrors(t *testing.T) {
	if !IsNotFound(notFound()) {
		t.Fatal("the Fake's not-found error must satisfy IsNotFound")
	}
	if IsNotFound(&APIError{StatusCode: http.StatusInternalServerError}) {
		t.Fatal("a 500 is not a not-found")
	}
}
