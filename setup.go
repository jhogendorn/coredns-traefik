package traefik

import (
	"fmt"
	"net"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/coredns/coredns/core/dnsserver"
	"github.com/coredns/coredns/plugin"

	"github.com/coredns/caddy"
	"github.com/miekg/dns"
)

const defaultTraefikApiEndpoint = "https://traefik.example.com/api"
const defaultTtl uint32 = 30
const defaultRefreshInterval uint32 = 30

func init() {
	caddy.RegisterPlugin("traefik", caddy.Plugin{
		ServerType: "dns",
		Action:     setup,
	})
}

func createPlugin(c *caddy.Controller) (*Traefik, error) {
	hostMatcher := regexp.MustCompile(`Host(SNI)?\(["'` + "`" + `]([^"'` + "`" + `]+)["'` + "`" + `]\)`)

	traefik := &Traefik{}

	// Server block zone (FQDN, lowercase); "." when no domain is set. Filters
	// default to this and must fall under it.
	zone := dns.Fqdn(strings.ToLower(dnsserver.GetConfig(c).Zone))

	for c.Next() {
		cfg := &TraefikConfig{
			hostMatcher:     hostMatcher,
			ttl:             defaultTtl,
			refreshInterval: defaultRefreshInterval,
			tcpRouters:      true,
		}

		defaultBaseUrl, err := url.Parse(defaultTraefikApiEndpoint)
		if err != nil {
			return traefik, err
		}
		cfg.baseUrl = defaultBaseUrl

		args := c.RemainingArgs()
		if len(args) > 1 {
			return traefik, c.ArgErr()
		}
		if len(args) == 1 {
			baseUrl, err := url.Parse(args[0])
			if err != nil {
				return traefik, err
			}

			cfg.baseUrl = baseUrl
		}

		apiHostname := cfg.baseUrl.Hostname()
		cfg.apiHostname = apiHostname

		apiIp := net.ParseIP(apiHostname)
		cfg.apiHostnameIsIp = apiIp != nil
		if apiIp != nil {
			cfg.apiIp = &apiIp
		}

		for c.NextBlock() {
			var value = c.Val()
			//goland:noinspection SpellCheckingInspection
			switch value {
			case "cname":
				if !c.NextArg() {
					return traefik, c.ArgErr()
				}
				val := c.Val()
				cfg.cname = &val
				if cfg.a != nil || cfg.aaaa != nil {
					return traefik, c.Err("traefik cname is mutually exclusive with a/aaaa")
				}
			case "a":
				val := c.RemainingArgs()
				ips, err := convertStringsToIPs(val)
				if err != nil {
					return traefik, c.Errf("traefik config failed to parse ip addresses: %s", err)
				}
				if len(ips) == 0 {
					return traefik, c.ArgErr()
				}
				cfg.a = &ips
				if cfg.cname != nil {
					return traefik, c.Err("traefik cname is mutually exclusive with a/aaaa")
				}
			case "aaaa":
				val := c.RemainingArgs()
				ips, err := convertStringsToIPv6s(val)
				if err != nil {
					return traefik, c.Errf("traefik config failed to parse ipv6 addresses: %s", err)
				}
				if len(ips) == 0 {
					return traefik, c.ArgErr()
				}
				cfg.aaaa = &ips
				if cfg.cname != nil {
					return traefik, c.Err("traefik cname is mutually exclusive with a/aaaa")
				}
			case "refreshinterval":
				if !c.NextArg() {
					return traefik, c.ArgErr()
				}
				refreshInterval, err := strconv.ParseUint(c.Val(), 10, 32)
				if err != nil {
					return traefik, err
				}
				if refreshInterval > 0 {
					cfg.refreshInterval = uint32(refreshInterval)
				}
			case "ttl":
				if !c.NextArg() {
					return traefik, c.ArgErr()
				}
				ttl, err := strconv.ParseUint(c.Val(), 10, 32)
				if err != nil {
					return traefik, err
				}
				if ttl > 0 {
					cfg.ttl = uint32(ttl)
				}
			case "retention":
				if !c.NextArg() {
					return traefik, c.ArgErr()
				}
				retention, err := strconv.ParseUint(c.Val(), 10, 32)
				if err != nil {
					return traefik, err
				}
				cfg.retention = uint32(retention)
			case "insecureskipverify":
				if c.NextArg() {
					cfg.insecureSkipVerify = parseBoolWithDefault(c.Val(), false)
				} else {
					cfg.insecureSkipVerify = true
				}
			case "fallthrough":
				traefik.Fall.SetZonesFromArgs(c.RemainingArgs())
			case "resolveapihost":
				if c.NextArg() {
					cfg.resolveApiHost = parseBoolWithDefault(c.Val(), false)
				} else {
					cfg.resolveApiHost = true
				}
			case "tcprouters":
				if c.NextArg() {
					cfg.tcpRouters = parseBoolWithDefault(c.Val(), true)
				} else {
					cfg.tcpRouters = true
				}
			case "filter":
				filters := c.RemainingArgs()
				if len(filters) == 0 {
					return traefik, c.ArgErr()
				}
				for _, f := range filters {
					fqdn := dns.Fqdn(strings.ToLower(f))
					if !dns.IsSubDomain(zone, fqdn) {
						return traefik, c.Errf("filter %q must be the server zone %q or a subdomain of it", f, zone)
					}
					cfg.filter = append(cfg.filter, fqdn)
				}
			default:
				return traefik, c.Errf("unknown property: '%s'", c.Val())
			}
		}

		if cfg.cname == nil && cfg.a == nil && cfg.aaaa == nil {
			return traefik, c.Errf("traefik config requires a cname, a, or aaaa")
		}

		// Default the filter to the server block zone.
		if len(cfg.filter) == 0 {
			cfg.filter = []string{zone}
		}

		client, err := NewTraefikClient(cfg)
		if err != nil {
			return nil, err
		}

		traefik.Instances = append(traefik.Instances, &traefikInstance{
			config:   cfg,
			client:   client,
			mappings: make(TraefikConfigEntryMap),
			evictAt:  make(map[string]time.Time),
		})

		log.Infof("base url ............ %s", cfg.baseUrl)
		log.Infof("cname ............... %v", cfg.cname)
		log.Infof("a ................... %v", cfg.a)
		log.Infof("aaaa ................ %v", cfg.aaaa)
		log.Infof("ttl ................. %v", cfg.ttl)
		log.Infof("refreshInterval ..... %v", cfg.refreshInterval)
		log.Infof("retention ........... %v", cfg.retention)
		log.Infof("insecureSkipVerify .. %v", cfg.insecureSkipVerify)
		log.Infof("apiHostname ......... %v", cfg.apiHostname)
		log.Infof("apiIp ............... %v", cfg.apiIp)
		log.Infof("resolveApiHost ...... %v", cfg.resolveApiHost)
		log.Infof("tcpRouters .......... %v", cfg.tcpRouters)
		log.Infof("filter .............. %v", cfg.filter)
	}

	return traefik, nil
}

func convertStringsToIPs(ips []string) ([]net.IP, error) {
	var result []net.IP
	for _, ipStr := range ips {
		ip := net.ParseIP(ipStr)
		if ip == nil || ip.To4() == nil {
			return nil, fmt.Errorf("invalid IPv4 address: %s", ipStr)
		}
		result = append(result, ip.To4())
	}
	return result, nil
}

func convertStringsToIPv6s(ips []string) ([]net.IP, error) {
	var result []net.IP
	for _, ipStr := range ips {
		ip := net.ParseIP(ipStr)
		if ip == nil || ip.To4() != nil {
			return nil, fmt.Errorf("invalid IPv6 address: %s", ipStr)
		}
		result = append(result, ip.To16())
	}
	return result, nil
}

func parseBoolWithDefault(str string, defaultValue bool) bool {
	parsedValue, err := strconv.ParseBool(str)
	if err != nil {
		log.Warningf("Failed to parse %s as bool. Defaulting to %v", str, defaultValue)
		return defaultValue
	}
	return parsedValue
}

func setup(c *caddy.Controller) error {
	traefik, err := createPlugin(c)
	if err != nil {
		return err
	}

	traefik.start()

	dnsserver.GetConfig(c).AddPlugin(func(next plugin.Handler) plugin.Handler {
		traefik.Next = next
		return traefik
	})

	return nil
}
