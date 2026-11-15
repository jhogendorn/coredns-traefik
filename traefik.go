package traefik

import (
	"context"
	"net"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/coredns/coredns/plugin"
	"github.com/coredns/coredns/plugin/metrics"
	"github.com/coredns/coredns/plugin/pkg/fall"
	clog "github.com/coredns/coredns/plugin/pkg/log"
	"github.com/coredns/coredns/request"

	"github.com/miekg/dns"
)

var log = clog.NewWithPlugin("traefik")

//goland:noinspection GoNameStartsWithPackageName
type TraefikConfigEntry struct {
	cname *string
	a     *[]net.IP
	aaaa  *[]net.IP
	ttl   uint32
}

//goland:noinspection GoNameStartsWithPackageName
type TraefikConfigEntryMap map[string]*TraefikConfigEntry

//goland:noinspection GoNameStartsWithPackageName
type TraefikConfig struct {
	baseUrl            *url.URL
	cname              *string
	a                  *[]net.IP
	aaaa               *[]net.IP
	ttl                uint32
	refreshInterval    uint32
	// retention is how many seconds a host keeps being served after it
	// disappears from Traefik. 0 means evict on the first refresh that no
	// longer sees it.
	retention          uint32
	insecureSkipVerify bool
	hostMatcher        *regexp.Regexp
	apiHostname        string
	apiHostnameIsIp    bool
	apiIp              *net.IP
	resolveApiHost     bool
	tcpRouters         bool
	// filter is the set of FQDNs (lowercase, trailing dot) a discovered host
	// must fall under to be served. Defaults to the server block zone.
	filter []string
}

// matchesFilter reports whether a discovered host (lowercase, no trailing dot)
// falls under one of the configured filter FQDNs.
func (cfg *TraefikConfig) matchesFilter(host string) bool {
	fqdn := dns.Fqdn(host)
	for _, f := range cfg.filter {
		if dns.IsSubDomain(f, fqdn) {
			return true
		}
	}
	return false
}

// traefikInstance is one configured `traefik` block: a Traefik API to poll and
// the records to return for the hosts discovered there. Each instance runs its
// own poller and keeps its own mappings.
type traefikInstance struct {
	config   *TraefikConfig
	client   ITraefikClient
	mappings TraefikConfigEntryMap
	// evictAt holds the deadline for hosts that vanished from Traefik but are
	// still served during their retention window. Empty when retention is 0.
	evictAt map[string]time.Time
	ready   bool
	mutex   sync.RWMutex
}

type Traefik struct {
	Next      plugin.Handler
	Instances []*traefikInstance
	Fall      fall.F
}

func (t *Traefik) Name() string { return "traefik" }

func (t *Traefik) ServeDNS(ctx context.Context, w dns.ResponseWriter, r *dns.Msg) (int, error) {
	state := request.Request{W: w, Req: r}

	if state.QClass() != dns.ClassINET {
		return plugin.NextOrFailure(t.Name(), t.Next, ctx, w, r)
	}

	requestCount.WithLabelValues(metrics.WithServer(ctx)).Inc()

	qname := state.QName()
	answers := []dns.RR{}
	found := false
	for _, q := range state.Req.Question {
		find := strings.ToLower(q.Name[:len(q.Name)-1])

		result := t.getEntry(find)
		if result != nil {
			found = true
			switch {
			case result.cname != nil:
				answers = cname(qname, result.ttl, result.cname)
			case state.QType() == dns.TypeA && result.a != nil:
				answers = a(qname, result.ttl, *result.a)
			case state.QType() == dns.TypeAAAA && result.aaaa != nil:
				answers = aaaa(qname, result.ttl, *result.aaaa)
			}
		}
	}

	m := new(dns.Msg)
	m.SetReply(r)
	m.Authoritative = true

	if len(answers) == 0 {
		switch {
		case found:
			log.Debug("Returning NODATA")
		case t.Fall.Through(qname) && t.Next != nil:
			log.Debug("Falling through. 0 answers")
			return plugin.NextOrFailure(t.Name(), t.Next, ctx, w, r)
		default:
			log.Debug("Returning NXDOMAIN")
			m.Rcode = dns.RcodeNameError
		}
	}

	m.Answer = answers

	//goland:noinspection ALL
	w.WriteMsg(m)
	return dns.RcodeSuccess, nil
}

func (t *Traefik) start() {
	for _, inst := range t.Instances {
		go inst.start()
	}
}

func (t *Traefik) getEntry(host string) *TraefikConfigEntry {
	for _, inst := range t.Instances {
		if entry := inst.getEntry(host); entry != nil {
			return entry
		}
	}
	return nil
}

func (i *traefikInstance) start() {
	src := i.config.baseUrl
	log.Infof("[%s] Starting!", src)
	if err := i.refresh(true); err != nil {
		log.Warningf("[%s] Failed to load Traefik HTTP routers, will retry: %s", src, err)
	}

	uptimeTicker := time.NewTicker(time.Duration(i.config.refreshInterval) * time.Second)
	for range uptimeTicker.C {
		log.Debugf("[%s] Refreshing sites", src)
		if err := i.refresh(false); err != nil {
			log.Warningf("[%s] Error loading Traefik HTTP routers: %v", src, err)
		}
	}
}

func (i *traefikInstance) getEntry(host string) *TraefikConfigEntry {
	i.mutex.RLock()
	defer i.mutex.RUnlock()

	if i.config.resolveApiHost && !i.config.apiHostnameIsIp && i.config.apiHostname == host {
		return &TraefikConfigEntry{
			a:    i.config.a,
			aaaa: i.config.aaaa,
			ttl:  i.config.ttl,
		}
	}

	value, foundIt := i.mappings[host]
	if !foundIt {
		return nil
	}

	return value
}

func (i *traefikInstance) refresh(first bool) error {
	src := i.config.baseUrl
	if first {
		log.Infof("[%s] Checking for Traefik routers...", src)
	}
	routers, err := i.client.GetHttpRouters()
	if err != nil {
		log.Errorf("[%s] Error retrieving Traefik HTTP routers: %s", src, err)
		return err
	}

	rules := make([]string, 0, len(*routers))
	for _, r := range *routers {
		rules = append(rules, r.Rule)
	}

	if i.config.tcpRouters {
		tcpRouters, err := i.client.GetTcpRouters()
		if err != nil {
			log.Errorf("[%s] Error retrieving Traefik TCP routers: %s", src, err)
			return err
		}
		for _, r := range *tcpRouters {
			rules = append(rules, r.Rule)
		}
	}

	i.mutex.Lock()
	defer i.mutex.Unlock()

	adds, deletes := 0, 0
	fromTraefik := map[string]struct{}{}
	for _, rule := range rules {
		//goland:noinspection ALL
		strs := i.config.hostMatcher.FindAllStringSubmatch(rule, -1)
		for _, s := range strs {
			if len(s) == 3 {
				host := strings.ToLower(s[2])
				if strings.Contains(host, "*") {
					continue
				}
				if !i.config.matchesFilter(host) {
					continue
				}
				fromTraefik[host] = struct{}{}

				if i.config.cname != nil {
					_, exists := i.mappings[host]
					if !exists {
						log.Infof("[%s] + CNAME %s -> %s", src, host, *i.config.cname)
						i.mappings[host] = &TraefikConfigEntry{
							cname: i.config.cname,
							ttl:   i.config.ttl,
						}
						adds += 1
					}
				} else {
					_, exists := i.mappings[host]

					if !exists {
						log.Infof("[%s] + %s -> a=%s aaaa=%s", src, host, ipsLog(i.config.a), ipsLog(i.config.aaaa))
						i.mappings[host] = &TraefikConfigEntry{
							a:    i.config.a,
							aaaa: i.config.aaaa,
							ttl:  i.config.ttl,
						}
						adds += 1
					}
				}
			}
		}
	}

	now := time.Now()
	toDelete := map[string]struct{}{}
	for cachedHost := range i.mappings {
		if _, stillExists := fromTraefik[cachedHost]; stillExists {
			delete(i.evictAt, cachedHost) // back (or never gone) — cancel any pending eviction
			continue
		}
		// Host is gone from Traefik. Keep serving it until its retention window
		// elapses; retention 0 evicts immediately.
		if i.config.retention > 0 {
			deadline, pending := i.evictAt[cachedHost]
			if !pending {
				i.evictAt[cachedHost] = now.Add(time.Duration(i.config.retention) * time.Second)
				continue
			}
			if now.Before(deadline) {
				continue
			}
		}
		if i.config.cname != nil {
			log.Infof("[%s] - CNAME %s -> %s", src, cachedHost, *i.config.cname)
		} else {
			log.Infof("[%s] - %s -> a=%s aaaa=%s", src, cachedHost, ipsLog(i.config.a), ipsLog(i.config.aaaa))
		}
		delete(i.evictAt, cachedHost)
		toDelete[cachedHost] = struct{}{}
		deletes += 1
	}

	for del := range toDelete {
		delete(i.mappings, del)
	}

	if adds > 0 && deletes > 0 {
		log.Infof("[%s] Added %d, deleted %d entries", src, adds, deletes)
	} else if adds > 0 {
		log.Infof("[%s] Added %d entries", src, adds)
	} else if deletes > 0 {
		log.Infof("[%s] Deleted %d entries", src, deletes)
	} else {
		if first {
			log.Warningf("[%s] Failed to load Traefik HTTP routes... Will try again", src)
		} else {
			log.Debugf("[%s] No changes detected", src)
		}
	}

	i.ready = true
	return nil
}

func a(zone string, ttl uint32, ips []net.IP) []dns.RR {
	answers := make([]dns.RR, len(ips))
	for i, ip := range ips {
		r := new(dns.A)
		r.Hdr = dns.RR_Header{Name: zone, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: ttl}
		r.A = ip
		answers[i] = r
	}
	return answers
}

func aaaa(zone string, ttl uint32, ips []net.IP) []dns.RR {
	answers := make([]dns.RR, len(ips))
	for i, ip := range ips {
		r := new(dns.AAAA)
		r.Hdr = dns.RR_Header{Name: zone, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: ttl}
		r.AAAA = ip
		answers[i] = r
	}
	return answers
}

func cname(zone string, ttl uint32, target *string) []dns.RR {
	answers := make([]dns.RR, 1)
	r := new(dns.CNAME)
	r.Hdr = dns.RR_Header{Name: zone, Rrtype: dns.TypeCNAME, Class: dns.ClassINET, Ttl: ttl}
	r.Target = *target
	answers[0] = r
	return answers
}

func ipsLog(ips *[]net.IP) string {
	if ips == nil {
		return "-"
	}
	return ConvertIPsToString(*ips)
}

func ConvertIPsToString(ips []net.IP) string {
	var ipStrings []string
	for _, ip := range ips {
		ipStrings = append(ipStrings, ip.String())
	}
	return strings.Join(ipStrings, ",")
}
