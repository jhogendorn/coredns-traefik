package traefik

import (
	"context"
	"errors"
	"net"
	"net/url"
	"regexp"
	"testing"
	"time"

	"github.com/coredns/coredns/plugin/pkg/dnstest"
	"github.com/coredns/coredns/plugin/pkg/fall"
	"github.com/coredns/coredns/plugin/test"

	"github.com/miekg/dns"
)

// fakeClient returns a canned router list, standing in for a live Traefik API.
type fakeClient struct {
	routers    []HttpRouter
	tcpRouters []TcpRouter
	tcpErr     error
}

func (f *fakeClient) GetHttpRouters() (*[]HttpRouter, error) { return &f.routers, nil }

func (f *fakeClient) GetTcpRouters() (*[]TcpRouter, error) {
	if f.tcpErr != nil {
		return nil, f.tcpErr
	}
	return &f.tcpRouters, nil
}

// TestRetention proves a discovered host keeps being served for its retention
// window after it disappears from Traefik, is evicted once the window elapses,
// and that reappearing within the window cancels the pending eviction.
func TestRetention(t *testing.T) {
	baseURL, _ := url.Parse("http://traefik.example.org/api")
	ips := []net.IP{net.ParseIP("10.0.0.2")}
	fc := &fakeClient{}
	inst := &traefikInstance{
		config: &TraefikConfig{
			baseUrl:     baseURL,
			hostMatcher: regexp.MustCompile(`Host(SNI)?\(` + "`([^`]+)`" + `\)`),
			a:           &ips,
			ttl:         30,
			retention:   300,
			filter:      []string{"example.org."},
		},
		client:   fc,
		mappings: make(TraefikConfigEntryMap),
		evictAt:  make(map[string]time.Time),
	}
	const host = "whoami.example.org"
	router := HttpRouter{Rule: "Host(`" + host + "`)"}

	// Discovered while advertised.
	fc.routers = []HttpRouter{router}
	inst.refresh(false)
	if inst.getEntry(host) == nil {
		t.Fatal("host should be served once discovered")
	}

	// Gone from Traefik, but within the retention window it is still served.
	fc.routers = nil
	inst.refresh(false)
	if inst.getEntry(host) == nil {
		t.Fatal("host should linger during the retention window")
	}
	if _, pending := inst.evictAt[host]; !pending {
		t.Fatal("eviction should be pending")
	}

	// Reappears within the window: pending eviction is cancelled.
	fc.routers = []HttpRouter{router}
	inst.refresh(false)
	if _, pending := inst.evictAt[host]; pending {
		t.Fatal("eviction should be cancelled when the host reappears")
	}

	// Gone again, then the window elapses: evicted.
	fc.routers = nil
	inst.refresh(false)
	inst.evictAt[host] = time.Now().Add(-time.Second) // force the deadline into the past
	inst.refresh(false)
	if inst.getEntry(host) != nil {
		t.Fatal("host should be evicted after the retention window elapses")
	}

	// retention 0 evicts on the first refresh that no longer sees the host.
	inst.config.retention = 0
	fc.routers = []HttpRouter{router}
	inst.refresh(false)
	fc.routers = nil
	inst.refresh(false)
	if inst.getEntry(host) != nil {
		t.Fatal("with retention 0 the host should be evicted immediately")
	}
}

// TestMatchesFilter checks which discovered hosts a filter admits.
func TestMatchesFilter(t *testing.T) {
	cfg := &TraefikConfig{filter: []string{"example.org."}}
	cases := map[string]bool{
		"gitea.example.org":    true,
		"example.org":          true,
		"deep.sub.example.org": true,
		"example.net":          false,
		"notexample.org":       false,
	}
	for host, want := range cases {
		if got := cfg.matchesFilter(host); got != want {
			t.Errorf("matchesFilter(%q) = %v, want %v", host, got, want)
		}
	}
}

// TestServeDNSMultipleInstances proves a single Traefik handler answers for
// hosts discovered across more than one configured block.
func TestServeDNSMultipleInstances(t *testing.T) {
	cnameTarget := "traefik-a.example.org."
	instA := &traefikInstance{
		config: &TraefikConfig{ttl: 30},
		mappings: TraefikConfigEntryMap{
			"a.example.org": &TraefikConfigEntry{cname: &cnameTarget, ttl: 30},
		},
	}
	ips := []net.IP{net.ParseIP("10.0.0.5")}
	instB := &traefikInstance{
		config: &TraefikConfig{ttl: 30},
		mappings: TraefikConfigEntryMap{
			"b.example.org": &TraefikConfigEntry{a: &ips, ttl: 30},
		},
	}

	tk := &Traefik{
		Next:      test.ErrorHandler(),
		Instances: []*traefikInstance{instA, instB},
	}

	tests := []struct {
		qname  string
		qtype  uint16
		rcode  int
		answer int
	}{
		{"a.example.org.", dns.TypeA, dns.RcodeSuccess, 1}, // CNAME from instance A
		{"b.example.org.", dns.TypeA, dns.RcodeSuccess, 1}, // A from instance B
		{"unknown.example.org.", dns.TypeA, dns.RcodeNameError, 0},
	}

	for _, tc := range tests {
		r := new(dns.Msg)
		r.SetQuestion(tc.qname, tc.qtype)
		rec := dnstest.NewRecorder(&test.ResponseWriter{})

		if _, err := tk.ServeDNS(context.TODO(), rec, r); err != nil {
			t.Fatalf("%s: expected no error, got %v", tc.qname, err)
		}
		if rec.Msg.Rcode != tc.rcode {
			t.Errorf("%s: expected rcode %d, got %d", tc.qname, tc.rcode, rec.Msg.Rcode)
		}
		if len(rec.Msg.Answer) != tc.answer {
			t.Errorf("%s: expected %d answers, got %d", tc.qname, tc.answer, len(rec.Msg.Answer))
		}
	}
}

type nxdomainHandler struct{ called bool }

func (h *nxdomainHandler) Name() string { return "nxdomain" }

func (h *nxdomainHandler) ServeDNS(_ context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	h.called = true
	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true
	m.Rcode = dns.RcodeNameError
	w.WriteMsg(m)
	return dns.RcodeSuccess, nil
}

func TestServeDNSKnownHostNoDataWithFallthrough(t *testing.T) {
	ips := []net.IP{net.ParseIP("10.0.0.2")}
	inst := &traefikInstance{
		config: &TraefikConfig{ttl: 30},
		mappings: TraefikConfigEntryMap{
			"known.example.org": &TraefikConfigEntry{a: &ips, ttl: 30},
		},
	}

	tests := []struct {
		name       string
		qname      string
		qtype      uint16
		rcode      int
		answers    int
		nextCalled bool
	}{
		{"known host, A is answered", "known.example.org.", dns.TypeA, dns.RcodeSuccess, 1, false},
		{"known host, AAAA is NODATA not NXDOMAIN", "known.example.org.", dns.TypeAAAA, dns.RcodeSuccess, 0, false},
		{"known host, other qtype is NODATA", "known.example.org.", dns.TypeTXT, dns.RcodeSuccess, 0, false},
		{"unknown host still falls through", "other.example.org.", dns.TypeA, dns.RcodeNameError, 0, true},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			next := &nxdomainHandler{}
			tk := &Traefik{
				Next:      next,
				Instances: []*traefikInstance{inst},
				Fall:      fall.Root,
			}

			r := new(dns.Msg)
			r.SetQuestion(tc.qname, tc.qtype)
			rec := dnstest.NewRecorder(&test.ResponseWriter{})

			if _, err := tk.ServeDNS(context.TODO(), rec, r); err != nil {
				t.Fatalf("expected no error, got %v", err)
			}
			if rec.Msg.Rcode != tc.rcode {
				t.Errorf("expected rcode %s, got %s",
					dns.RcodeToString[tc.rcode], dns.RcodeToString[rec.Msg.Rcode])
			}
			if len(rec.Msg.Answer) != tc.answers {
				t.Errorf("expected %d answers, got %d", tc.answers, len(rec.Msg.Answer))
			}
			if next.called != tc.nextCalled {
				t.Errorf("expected next-plugin called=%v, got %v", tc.nextCalled, next.called)
			}
			if !tc.nextCalled && !rec.Msg.Authoritative {
				t.Error("expected an authoritative answer from the plugin itself")
			}
		})
	}
}

func TestTcpRouters(t *testing.T) {
	baseURL, _ := url.Parse("http://traefik.example.org/api")
	ips := []net.IP{net.ParseIP("10.0.0.2")}
	hostMatcher := regexp.MustCompile(`Host(SNI)?\(` + "`([^`]+)`" + `\)`)

	newInstance := func(fc *fakeClient) *traefikInstance {
		return &traefikInstance{
			config: &TraefikConfig{
				baseUrl:     baseURL,
				hostMatcher: hostMatcher,
				a:           &ips,
				ttl:         30,
				tcpRouters:  true,
				filter:      []string{"example.org."},
			},
			client:   fc,
			mappings: make(TraefikConfigEntryMap),
			evictAt:  make(map[string]time.Time),
		}
	}

	t.Run("concrete HostSNI is discovered", func(t *testing.T) {
		fc := &fakeClient{tcpRouters: []TcpRouter{{Rule: "HostSNI(`ssh.example.org`)"}}}
		inst := newInstance(fc)
		if err := inst.refresh(false); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if inst.getEntry("ssh.example.org") == nil {
			t.Fatal("expected ssh.example.org to be discovered from a TCP router")
		}
	})

	t.Run("wildcard HostSNI is skipped", func(t *testing.T) {
		fc := &fakeClient{tcpRouters: []TcpRouter{{Rule: "HostSNI(`*`)"}}}
		inst := newInstance(fc)
		if err := inst.refresh(false); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if inst.getEntry("*") != nil {
			t.Fatal("wildcard HostSNI must not create a record")
		}
		if len(inst.mappings) != 0 {
			t.Fatalf("expected no mappings from a wildcard-only TCP router, got %d", len(inst.mappings))
		}
	})

	t.Run("wildcard suffix HostSNI is skipped, sibling concrete host still registers", func(t *testing.T) {
		fc := &fakeClient{tcpRouters: []TcpRouter{{Rule: "HostSNI(`*.example.org`) || HostSNI(`concrete.example.org`)"}}}
		inst := newInstance(fc)
		if err := inst.refresh(false); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if inst.getEntry("*.example.org") != nil {
			t.Fatal("wildcard-suffix HostSNI must not create a record")
		}
		if inst.getEntry("concrete.example.org") == nil {
			t.Fatal("sibling concrete host in the same rule must still be discovered")
		}
		if len(inst.mappings) != 1 {
			t.Fatalf("expected only the concrete host to be mapped, got %d", len(inst.mappings))
		}
	})

	t.Run("multiple HostSNI values in one rule", func(t *testing.T) {
		fc := &fakeClient{tcpRouters: []TcpRouter{{Rule: "HostSNI(`a.example.org`) || HostSNI(`b.example.org`)"}}}
		inst := newInstance(fc)
		if err := inst.refresh(false); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if inst.getEntry("a.example.org") == nil || inst.getEntry("b.example.org") == nil {
			t.Fatal("expected both HostSNI values to be discovered")
		}
	})

	t.Run("host in both HTTP and TCP is not duplicated or flapped", func(t *testing.T) {
		const host = "shared.example.org"
		fc := &fakeClient{
			routers:    []HttpRouter{{Rule: "Host(`" + host + "`)"}},
			tcpRouters: []TcpRouter{{Rule: "HostSNI(`" + host + "`)"}},
		}
		inst := newInstance(fc)
		if err := inst.refresh(false); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if len(inst.mappings) != 1 {
			t.Fatalf("expected 1 mapping for a host advertised by both HTTP and TCP, got %d", len(inst.mappings))
		}

		fc.routers = nil
		if err := inst.refresh(false); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if inst.getEntry(host) == nil {
			t.Fatal("host still advertised via TCP must not be evicted when HTTP stops advertising it")
		}
	})

	t.Run("TCP fetch failure aborts the refresh and preserves prior state", func(t *testing.T) {
		fc := &fakeClient{
			routers:    []HttpRouter{{Rule: "Host(`http.example.org`)"}},
			tcpRouters: []TcpRouter{{Rule: "HostSNI(`tcp-only.example.org`)"}},
		}
		inst := newInstance(fc)
		if err := inst.refresh(false); err != nil {
			t.Fatalf("unexpected error on the initial refresh: %v", err)
		}
		if inst.getEntry("http.example.org") == nil || inst.getEntry("tcp-only.example.org") == nil {
			t.Fatal("expected both the HTTP and TCP-only hosts to be discovered on the initial refresh")
		}

		fc.tcpErr = errors.New("tcp api unreachable")
		if err := inst.refresh(false); err == nil {
			t.Fatal("expected refresh to return an error when the TCP fetch fails")
		}
		if inst.getEntry("http.example.org") == nil {
			t.Fatal("HTTP host must remain mapped after a refresh aborted by a TCP fetch failure")
		}
		if inst.getEntry("tcp-only.example.org") == nil {
			t.Fatal("TCP-only host must remain mapped after a refresh aborted by a TCP fetch failure")
		}
	})
}
