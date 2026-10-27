package cloudflare

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientCreateTunnel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/accounts/acct/cfd_tunnel" {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if got := r.Header.Get("Authorization"); got != "Bearer secret" {
			t.Errorf("Authorization = %q", got)
		}
		body, _ := io.ReadAll(r.Body)
		var req map[string]string
		_ = json.Unmarshal(body, &req)
		if req["name"] != "home" || req["config_src"] != "cloudflare" {
			t.Errorf("unexpected body %s", body)
		}
		_, _ = w.Write([]byte(`{"success":true,"errors":[],"result":{"id":"abc","name":"home","status":"inactive"}}`))
	}))
	defer srv.Close()

	tun, err := NewClient(srv.URL, "secret").CreateTunnel(context.Background(), "acct", "home")
	if err != nil {
		t.Fatal(err)
	}
	if tun.ID != "abc" || tun.Name != "home" {
		t.Fatalf("got %+v", tun)
	}
}

func TestClientTokenAndConfig(t *testing.T) {
	var pushed map[string]TunnelConfig
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/accounts/acct/cfd_tunnel/abc/token":
			_, _ = w.Write([]byte(`{"success":true,"result":"tok"}`))
		case "/accounts/acct/cfd_tunnel/abc/configurations":
			if r.Method != http.MethodPut {
				t.Errorf("method = %s", r.Method)
			}
			_ = json.NewDecoder(r.Body).Decode(&pushed)
			_, _ = w.Write([]byte(`{"success":true,"result":{}}`))
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
		}
	}))
	defer srv.Close()
	c := NewClient(srv.URL, "secret")

	tok, err := c.GetTunnelToken(context.Background(), "acct", "abc")
	if err != nil || tok != "tok" {
		t.Fatalf("token = %q, %v", tok, err)
	}

	cfg := TunnelConfig{Ingress: []IngressRule{
		{Hostname: "a.example.com", Service: "http://a.ns.svc.cluster.local:80"},
		{Service: "http_status:404"},
	}}
	if err := c.UpdateTunnelConfig(context.Background(), "acct", "abc", cfg); err != nil {
		t.Fatal(err)
	}
	if got := pushed["config"].Ingress; len(got) != 2 || got[0].Hostname != "a.example.com" {
		t.Fatalf("pushed %+v", pushed)
	}
}

func TestClientErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotFound)
		_, _ = w.Write([]byte(`{"success":false,"errors":[{"code":1003,"message":"Tunnel not found"}],"result":null}`))
	}))
	defer srv.Close()

	_, err := NewClient(srv.URL, "secret").GetTunnel(context.Background(), "acct", "missing")
	if !IsNotFound(err) {
		t.Fatalf("expected not found, got %v", err)
	}
	if want := "cloudflare API returned 404: 1003: Tunnel not found"; err.Error() != want {
		t.Fatalf("error = %q, want %q", err, want)
	}
}

func TestClientFindDNSRecordSkipsOtherTypes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("name") != "app.example.com" {
			t.Errorf("query = %s", r.URL.RawQuery)
		}
		_, _ = w.Write([]byte(`{"success":true,"result":[
			{"id":"1","type":"TXT","name":"app.example.com","content":"x"},
			{"id":"2","type":"CNAME","name":"app.example.com","content":"t.cfargotunnel.com","proxied":true}
		]}`))
	}))
	defer srv.Close()

	rec, err := NewClient(srv.URL, "secret").FindDNSRecord(context.Background(), "zone", "app.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if rec == nil || rec.ID != "2" {
		t.Fatalf("got %+v", rec)
	}
}
