package traefik

import (
	"reflect"
	"testing"

	"github.com/coredns/caddy"
	"github.com/coredns/coredns/core/dnsserver"
)

// TestSetup tests the various things that should be parsed by setup, including
// multiple traefik blocks in one server block.
func TestSetup(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		shouldErr    bool
		numInstances int
	}{
		{"no records", `traefik`, true, 0},
		{"too many args", `traefik https://traefik.example.org/api extra`, true, 0},
		{"cname", `traefik https://traefik.example.org/api {
			cname traefik.example.org
		}`, false, 1},
		{"a record", `traefik https://traefik.example.org/api {
			a 10.0.0.2
		}`, false, 1},
		{"cname and a are mutually exclusive", `traefik https://traefik.example.org/api {
			cname traefik.example.org
			a 10.0.0.2
		}`, true, 0},
		{"unknown property", `traefik https://traefik.example.org/api {
			bogus value
		}`, true, 0},
		{"two blocks", `traefik https://a.example.org/api {
			cname a.example.org
		}
		traefik https://b.example.org/api {
			a 10.0.0.3
		}`, false, 2},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := caddy.NewTestController("dns", tc.input)
			traefik, err := createPlugin(c)
			if tc.shouldErr && err == nil {
				t.Fatalf("Expected error, but got none")
			}
			if !tc.shouldErr && err != nil {
				t.Fatalf("Expected no error, but got: %v", err)
			}
			if !tc.shouldErr && len(traefik.Instances) != tc.numInstances {
				t.Fatalf("Expected %d instances, but got %d", tc.numInstances, len(traefik.Instances))
			}
		})
	}
}

// TestFilterSetup covers the record filter: it defaults to the server block
// zone, only accepts FQDNs at or below that zone, and accepts any FQDN when the
// server zone is root.
func TestFilterSetup(t *testing.T) {
	tests := []struct {
		name       string
		zone       string
		input      string
		shouldErr  bool
		wantFilter []string
	}{
		{
			name:       "defaults to server zone",
			zone:       "example.org.",
			input:      `traefik https://traefik.example.org/api { cname traefik.example.org }`,
			wantFilter: []string{"example.org."},
		},
		{
			name: "explicit subdomain filters",
			zone: "example.org.",
			input: `traefik https://traefik.example.org/api {
				cname traefik.example.org
				filter a.example.org b.example.org
			}`,
			wantFilter: []string{"a.example.org.", "b.example.org."},
		},
		{
			name: "the zone itself is a valid filter",
			zone: "example.org.",
			input: `traefik https://traefik.example.org/api {
				cname traefik.example.org
				filter example.org
			}`,
			wantFilter: []string{"example.org."},
		},
		{
			name: "filter outside the zone is rejected",
			zone: "example.org.",
			input: `traefik https://traefik.example.org/api {
				cname traefik.example.org
				filter example.net
			}`,
			shouldErr: true,
		},
		{
			name: "any filter allowed when server zone is root",
			zone: ".",
			input: `traefik https://traefik.example.org/api {
				cname traefik.example.org
				filter example.net
			}`,
			wantFilter: []string{"example.net."},
		},
		{
			name: "filter requires an argument",
			zone: "example.org.",
			input: `traefik https://traefik.example.org/api {
				cname traefik.example.org
				filter
			}`,
			shouldErr: true,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := caddy.NewTestController("dns", tc.input)
			dnsserver.GetConfig(c).Zone = tc.zone
			traefik, err := createPlugin(c)
			if tc.shouldErr {
				if err == nil {
					t.Fatalf("Expected error, but got none")
				}
				return
			}
			if err != nil {
				t.Fatalf("Expected no error, but got: %v", err)
			}
			got := traefik.Instances[0].config.filter
			if !reflect.DeepEqual(got, tc.wantFilter) {
				t.Fatalf("filter = %v, want %v", got, tc.wantFilter)
			}
		})
	}
}

func TestTcpRoutersSetup(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  bool
	}{
		{
			name:  "defaults to true",
			input: `traefik https://traefik.example.org/api { cname traefik.example.org }`,
			want:  true,
		},
		{
			name: "bare directive is true",
			input: `traefik https://traefik.example.org/api {
				cname traefik.example.org
				tcprouters
			}`,
			want: true,
		},
		{
			name: "explicit false disables it",
			input: `traefik https://traefik.example.org/api {
				cname traefik.example.org
				tcprouters false
			}`,
			want: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := caddy.NewTestController("dns", tc.input)
			traefik, err := createPlugin(c)
			if err != nil {
				t.Fatalf("Expected no error, but got: %v", err)
			}
			got := traefik.Instances[0].config.tcpRouters
			if got != tc.want {
				t.Fatalf("tcpRouters = %v, want %v", got, tc.want)
			}
		})
	}
}
