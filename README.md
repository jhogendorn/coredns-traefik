# traefik

## Name

*traefik* - returns records for hosts discovered in the routers of a Traefik instance.

## Description

*traefik* polls a Traefik instance's [API](https://doc.traefik.io/traefik/operations/api/)
(`/api/http/routers` and, unless disabled, `/api/tcp/routers`) and extracts the FQDNs from the
`Host()` and `HostSNI()` matchers in each router's rule. ``HostSNI(`*`)`` catch-all TCP routers are
skipped; they name no concrete host. For every discovered host it answers queries with either a
`CNAME` pointing at the Traefik instance, or `A`/`AAAA` records for the Traefik instance's addresses.
Records are refreshed periodically, so containers exposed through Traefik become resolvable without
any manual DNS entries.

Queries for hosts that are not currently advertised by a Traefik router are answered with NXDOMAIN,
or passed to the next plugin when `fallthrough` is set.

## Syntax

~~~ txt
traefik [ENDPOINT] {
    cname CNAME
    a ADDRESS...
    aaaa ADDRESS...
    refreshinterval SECONDS
    ttl SECONDS
    retention SECONDS
    insecureskipverify [BOOL]
    resolveapihost [BOOL]
    tcprouters [BOOL]
    filter FQDN...
    fallthrough [ZONES...]
}
~~~

* **ENDPOINT** the base URL of the Traefik API, without a trailing slash, for example
  `https://traefik.example.org/api`. The `/api/http/routers` endpoint is derived from it. Defaults to
  `https://traefik.example.com/api`.
* `cname` **CNAME** the fully qualified name (with or without a trailing `.`) returned as a `CNAME`
  for every discovered host, usually the host name of **ENDPOINT**. Mutually exclusive with `a` and
  `aaaa`.
* `a` **ADDRESS...** one or more IPv4 addresses of the Traefik instance, returned as `A` records for
  every discovered host. Mutually exclusive with `cname`.
* `aaaa` **ADDRESS...** one or more IPv6 addresses of the Traefik instance, returned as `AAAA`
  records for every discovered host. Mutually exclusive with `cname`.
* `refreshinterval` **SECONDS** how often the Traefik API is polled for changes, an integer number of
  seconds. Default `30`.
* `ttl` **SECONDS** the TTL of the returned records, an integer number of seconds. Default `30`.
* `retention` **SECONDS** how long a discovered host keeps being served after it disappears from
  Traefik (for example when its router or container label is removed), an integer number of seconds.
  The host is evicted on the first refresh after the window elapses; if it reappears within the
  window it is kept. Default `0`, which evicts as soon as Traefik stops advertising the host.
* `insecureskipverify` **[BOOL]** skip TLS certificate verification when the API endpoint uses HTTPS
  with an untrusted certificate. The argument is optional; the bare directive means `true`. Default
  `false`.
* `resolveapihost` **[BOOL]** when set together with `a`/`aaaa`, also answer queries for the
  **ENDPOINT** host name itself with those addresses. The argument is optional; the bare directive
  means `true`. Default `false`.
* `tcprouters` **[BOOL]** also poll `/api/tcp/routers` and discover hosts from `HostSNI()` rules,
  merged into the same set as HTTP-discovered hosts. The argument is optional; the bare directive
  means `true`. Default `true`.
* `filter` **FQDN...** only serve discovered hosts that are equal to, or a subdomain of, one of the
  given **FQDN**s; other hosts advertised by Traefik are ignored. Defaults to the server block's
  zone, so out-of-zone hosts are dropped unless you widen or narrow the filter. Each **FQDN** must
  itself be the server zone or a subdomain of it; when the server block has no zone (`.`) any **FQDN**
  is accepted.
* `fallthrough` **[ZONES...]** if a query for a host is not advertised by Traefik, pass it to the next
  plugin instead of returning NXDOMAIN. If **ZONES** is given, only queries in those zones fall
  through. See also the *fallthrough* section of the CoreDNS plugin documentation.

Exactly one record type must be configured: either `cname`, or one or both of `a` and `aaaa`.

## Examples

Return a `CNAME` to the Traefik instance for every host it advertises, polling every 30 seconds:

~~~ txt
example.org:53 {
    hosts {
        10.0.0.2 traefik.example.org
        fallthrough
    }
    traefik https://traefik.example.org/api {
        cname traefik.example.org
        refreshinterval 30
        ttl 30
    }
    forward . 10.0.0.1
}
~~~

A container labelled with a Traefik router rule such as ``Host(`gitea.example.org`)`` is discovered
on the next poll and resolves via the `CNAME` to `traefik.example.org`, which the *hosts* plugin
answers with `10.0.0.2`.

Return `A` records directly, and also resolve the API host name itself:

~~~ txt
example.org:53 {
    traefik https://traefik.example.org/api {
        a 10.0.0.2
        resolveapihost
        ttl 30
    }
    forward . 10.0.0.1
}
~~~

Poll two Traefik instances from the same server block, each with its own records:

~~~ txt
example.org:53 {
    traefik https://traefik-a.example.org/api {
        cname traefik-a.example.org
    }
    traefik https://traefik-b.example.org/api {
        a 10.0.0.3
    }
    forward . 10.0.0.1
}
~~~

Each block is polled independently; a query is answered by the first instance that knows the host.

## Building

This plugin must be compiled into CoreDNS. Add it to
[plugin.cfg](https://github.com/coredns/coredns/blob/master/plugin.cfg) **above the backend plugins
that answer the zone** — `file`, `auto`, `secondary`, `etcd`, and `forward`. CoreDNS runs plugins in
plugin.cfg order, and those plugins answer authoritatively (or forward upstream) without falling
through, so if any of them runs before *traefik* the query for a discovered host is answered
(NXDOMAIN, or an upstream reply) before *traefik* ever sees it. Placing it just before `file` puts it
ahead of the whole cluster:

~~~ txt
traefik:github.com/BaseCrusher/coredns-traefik
~~~

Then rebuild with `go generate && go build`, or `make`.

A plugin ordered *before* *traefik* (such as `hosts`) will handle matching queries first; give it
`fallthrough` so unmatched names reach *traefik* (see the *hosts* example above).

## Metrics

If monitoring is enabled (via the *prometheus* directive) the following metric is exported:

* `coredns_traefik_request_count_total{server}` - query count to the *traefik* plugin.

The `server` label indicates which server handled the request, see the *metrics* plugin for details.

## Ready

This plugin reports readiness to the *ready* plugin. It becomes ready after the first successful
refresh of data from the Traefik API.

## Compared to the *template* plugin

Traefik already configures routes, services, and TLS from container labels, but the matching DNS
records still have to be created by hand: a single address for Traefik and a CNAME to it per service.
*traefik* removes that last manual step by polling the Traefik API and answering for the discovered
host names on the fly.

Most of this can also be achieved with the *template* plugin, which ships with CoreDNS and needs no
custom build — a wildcard `match` under the zone answering a `CNAME` to the Traefik instance:

~~~ txt
template IN ANY dyn.example.org {
    match "^.+\.dyn\.example\.org\.$"
    answer "{{ .Name }} 60 IN CNAME dyn.example.org."
    fallthrough
}
~~~

Be aware that a wildcard like this matches *every* name under the zone, including the ones served by
*file* — *template* runs first and shadows those records. Names not advertised by Traefik also
resolve, so a missing service returns a Traefik 404 instead of NXDOMAIN.

Extending the `match` keeps *file* reachable: scope the wildcard to a dedicated label so everything
else falls through to the zone file.

~~~ txt
dyn.example.org {
    template IN ANY dyn.example.org {
        match "^.+\.apps\.dyn\.example\.org\.$"
        answer "{{ .Name }} 60 IN CNAME dyn.example.org."
        fallthrough
    }

    file /etc/coredns/dyn.example.org.db
}
~~~

`whoami.apps.dyn.example.org` is answered by the template, while `nas.dyn.example.org` misses the
regex and is served from the zone file. Note that Go's regexp engine has no negative lookahead, so
excluding individual static names from a broad wildcard is not practical — separate the dynamic names
by label instead.

*traefik* answers only the host names Traefik currently advertises, so unknown names still return
NXDOMAIN and records served by other plugins are not shadowed. Use *template* if a static wildcard is
enough for your setup.

## Also See

See the [manual](https://coredns.io/manual) and the
[Traefik API documentation](https://doc.traefik.io/traefik/operations/api/).
