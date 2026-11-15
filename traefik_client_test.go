package traefik

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
)

func TestClientHasTimeout(t *testing.T) {
	baseUrl, _ := url.Parse("http://traefik.example.org/api")

	for _, insecure := range []bool{false, true} {
		c, err := NewTraefikClient(&TraefikConfig{baseUrl: baseUrl, insecureSkipVerify: insecure})
		if err != nil {
			t.Fatal(err)
		}
		if c.client.Timeout <= 0 {
			t.Errorf("insecureSkipVerify=%v: no timeout, a blackholed connection would stall the refresh loop", insecure)
		}
	}
}

func pagedServer(t *testing.T, pages map[string][]string, failPage string) (*httptest.Server, *TraefikClient) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("per_page") != "100" {
			t.Errorf("%s: per_page = %q", r.URL, r.URL.Query().Get("per_page"))
		}
		names, ok := pages[r.URL.Path]
		page, _ := strconv.Atoi(r.URL.Query().Get("page"))
		if !ok || page < 1 || page > len(names) || r.URL.Query().Get("page") == failPage {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		next := page + 1
		if next > len(names) {
			next = 1
		}
		w.Header().Set("X-Next-Page", strconv.Itoa(next))
		fmt.Fprintf(w, `[{"name":%q,"rule":"Host(%s)"}]`, names[page-1], "`"+names[page-1]+".example.org`")
	}))
	baseUrl, _ := url.Parse(srv.URL + "/api")
	c, err := NewTraefikClient(&TraefikConfig{baseUrl: baseUrl})
	if err != nil {
		t.Fatal(err)
	}
	return srv, c
}

func TestHttpRoutersPaged(t *testing.T) {
	srv, c := pagedServer(t, map[string][]string{"/api/http/routers": {"a", "b", "c"}}, "")
	defer srv.Close()

	routers, err := c.GetHttpRouters()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range *routers {
		got = append(got, r.Name)
	}
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("routers = %v, want a,b,c", got)
	}
}

func TestHttpRoutersPageFailure(t *testing.T) {
	srv, c := pagedServer(t, map[string][]string{"/api/http/routers": {"a", "b", "c"}}, "2")
	defer srv.Close()

	if routers, err := c.GetHttpRouters(); err == nil {
		t.Errorf("expected an error when page 2 fails, got %d routers", len(*routers))
	}
}

func TestTcpRoutersPaged(t *testing.T) {
	srv, c := pagedServer(t, map[string][]string{"/api/tcp/routers": {"a", "b", "c"}}, "")
	defer srv.Close()

	routers, err := c.GetTcpRouters()
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, r := range *routers {
		got = append(got, r.Name)
	}
	if strings.Join(got, ",") != "a,b,c" {
		t.Errorf("routers = %v, want a,b,c", got)
	}
}

func TestTcpRoutersPageFailure(t *testing.T) {
	srv, c := pagedServer(t, map[string][]string{"/api/tcp/routers": {"a", "b", "c"}}, "2")
	defer srv.Close()

	if routers, err := c.GetTcpRouters(); err == nil {
		t.Errorf("expected an error when page 2 fails, got %d routers", len(*routers))
	}
}
