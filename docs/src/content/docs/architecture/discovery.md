---
title: Discovery
description: FastRecon as the scanning engine, its two inputs, the curated port list, and the Certificate Transparency stream.
sidebar:
  order: 7
---

## 7.1 FastRecon is the engine

[FastRecon](https://github.com/JoshuaMart/FastRecon) takes a perimeter and returns one JSON report:
the hosts that answer, the ports they expose, and the HTTP services behind them. Five stages on a
ladder, where each rung runs the ones below it:

```
 [1] ENUMERATE ──► raw subdomains, multi-source, deduplicated
 [2] EXCLUDE ────► in-scope subdomains
 [3] RESOLVE ────► live hosts with A/AAAA/CNAME, plus dead hosts with a reason
 [4] PORTSCAN ───► open ports per live host
 [5] HTTP PROBE ─► HTTP services with the right scheme, status, title, technologies, TLS
```

What Recon gets for free and would be unreasonable to rewrite: the passive source set with its per
source accounting, mandatory wildcard DNS detection, a validated resolver pool, unprivileged connect
scanning, CDN determination per address, and scheme detection that tries TLS first because the
handshake is the only reliable discriminator.

Three properties of the report shape the ingestion path:

- **Dead hosts stay in.** A dangling CNAME is a finding, not noise, and `nxdomain` and `no_answer` are
  distinct values.
- **Every source appears**, successful or not. A source that silently returns nothing is exactly what
  that accounting exists to expose, and it is what lets Recon apply [P2](/architecture/principles/)
  rather than guess.
- **A truncated run is still a valid report.** Hosts the run did not reach come back as `discovered`,
  so a gap is stated rather than mistaken for an absence.

### Two inputs, two mandates

The same engine answers both questions of [P1](/architecture/principles/), and the difference is the
input:

| | Discovery run | Verification run |
|---|---|---|
| Input | a program apex, plus the exclusion patterns | the hosts selected by [6.3](/architecture/lifecycle/#63-scheduling-and-backoff) |
| Stage 1 | queries the passive sources | replaced by the target list |
| Scope | `full`, or `enum` for a passive pass | `resolve` or `full`, per due date |
| May conclude | that an asset **exists** | that an asset **no longer answers** |

### Why there is no documentary observation

One tool produces the `dns`, `tcp` and `http` layers, with one normalization, whichever question the
run was asked. A discovery report and a verification report are the same measurement over different
hosts, so they deduplicate against each other correctly and neither poisons the other's chain.

That is worth stating because it is not free elsewhere. A second producer on the same layer would mean
a second normalization, and a baseline written by one followed by a probe from the other would produce
a false change on every asset at the second pass. The [fingerprinter](/architecture/verification/#82-the-fingerprinter)
is a different producer, which is why it is a different layer.

## 7.2 How a verification run gets its targets

The host list replaces stage 1 rather than skipping it, so exclusions still run on its output. That is
what Recon wants: a rule may have changed between the run being defined and the run starting, and the
patterns are a second safety net in front of the network.

| Setting | Use |
|---|---|
| `--targets` | inline hosts, for a single host run |
| `--targets-file` | one host per line, for local use |
| `--targets-url` | https, fetched at startup. The one a job uses |
| `--targets-header` | headers for that fetch, so the credential stays out of anything that logs a URL |

`--domain` becomes optional and purely informational in this mode: it labels the report so Recon can
correlate, and nothing else reads it. In particular the root domain filter does not apply, which is what
lets one list span several apexes of the same program.

**The report says which input was used.** `run.input` is `domain` or `targets`, and Recon reads it to
decide what a missing host means. Without it, an empty `sources` block would be the only clue, and a
clue is not a contract.

### What the list may contain, and why Recon satisfies it by construction

Bare hostnames, normalized, one per line. No port, because the port selection is a run level setting and
a target carrying one would make it ambiguous. No address literal with colons, so an IPv6 `ip` asset has
no place in a list.

That last point decides something in [6.3](/architecture/lifecycle/#scheduling-is-per-host): an `ip`
asset is **observed through the hosts that resolve to it**, never scheduled on its own. It is not a
restriction Recon has to work around, since an address only ever enters this inventory as the answer to a
name.

Three refusals matter more than they look, and all three protect the same property:

- **An oversized list fails the run** rather than being truncated. A silently shortened list turns hosts
  that were never queried into hosts that did not answer, which is the exact false death this whole
  input exists to prevent.
- **A malformed entry fails the run**, named, rather than being skipped. A host dropped for a formatting
  reason is a host that is never queried, and nothing downstream would say so.
- **An empty list refuses to start.** A run with nothing to scan is a configuration mistake, and an empty
  report hides it.

Recon meets all three without effort. What it serves are the canonical keys of `fqdn` assets, already
normalized by [its own canonicalization](/architecture/data-model/#43-canonical-keys), so they are well
formed by construction, and the other kinds never appear in a list.

## 7.3 Source credentials

Two of the default sources work without a key, which is what makes a first run return data. The keyed
ones report themselves as `skipped_no_key` rather than being silently dropped.

That distinction is the whole point, because the real failure mode does not look like a failure. Without
a credential a keyed source disables itself, the run starts, finishes correctly, and simply finds less.
Nobody notices while looking at an inventory they have never seen otherwise.

**Keys live in the run's environment, never in the run definition.** The control plane **names**
secrets, it does not carry them. A key is an infrastructure credential, the same for every program,
that says nothing about the target, unlike the perimeter, the budget and the port list, which are
properties of the program being scanned. Sending it down through the run definition would put it in the
database and in an HTTP response, for nothing.

Three consequences:

- **An empty variable is an error**, not "no key". The most likely mistake, a variable declared in the
  deployment and never filled, otherwise reproduces exactly the silent run this rule exists to remove,
  while giving the impression that the key is configured.
- **A run with no key at all is valid.** It is the default configuration, and the run says so in one
  line at startup.
- **Names are logged, values never are.** Startup emits which sources have a key and which do not, which
  is the observable counterpart of a source disarming itself.

## 7.4 The port list

The port list is data, which is what [P5](/architecture/principles/) requires of anything of this kind.
It lives in Recon's configuration and travels in the run definition, so discovery and verification scan
the same ports. A second list configured on the scanner would be a second list to keep in agreement,
and nothing would raise an error when the two diverged.

It is not nmap's top 100. That ranking orders ports by how often they are open across the whole
internet, so it leads with telnet, FTP, SMTP, POP3, IMAP, NetBIOS and printing. Open everywhere,
interesting almost nowhere. A mail server answering on 25 is a mail server, and it will still be one
next month.

The criterion here is different: **a port earns its place if it can carry a finding.** Four families:

| Family | Examples |
|---|---|
| Web entry points | 80, 443, 8080, 8443, 8000, 8090, 9000, 9443, 3000, 5000 |
| Application servers and admin consoles | 2375 and 2376, 6443 and 10250, 15672, 5601, 8983, 9990, 2083 and 2087 |
| Databases and stores | 3306, 5432, 27017, 6379, 9200, 11211, 9042, 2379 |
| Remote access and open proxies | 3389, 5900, 1080, 3128 |

A forgotten Jenkins on 8090, a Docker API on 2375, an Elasticsearch on 9200. Each of the last three
families is a finding by the sole fact of being reachable from the internet.

## 7.5 Certificate Transparency

A separate service based on `certstream-server-go` listens to the CT stream and filters on the apexes
present in the database.

**Matching is O(1), never a regex.** CT carries several million certificates a day. For each SAN, walk
the labels up through an in-memory set:

```
san = "staging.api.target.com"
→ test "staging.api.target.com"
→ test "api.target.com"
→ test "target.com"          ← match
```

Four lookups at most, whatever the number of programs. The set is reloaded periodically when the scope
changes. One core absorbs the full stream.

Every matching SAN creates an `fqdn` asset in **CANDIDATE** state and enters the verification loop with
the [aggressive backoff](/architecture/lifecycle/#backoff-curves). Its first check is a single host run,
which is the second thing the target list input buys: no enumeration, no API quota, an answer in
seconds.

**A structural blind spot: wildcard certificates.** A certificate issued for `*.target.com` reveals
**no** subdomain, and mature organizations use them for exactly that. Add internal names, non TLS
services and private CAs. CT and periodic enumeration are therefore not redundant. They are
complementary by construction.

:::tip[Product idea]
A wildcard certificate passing for an apex in the database is a signal in itself. It says CT will not
help on this program and that more effort belongs in DNS brute force. A per program **coverage
confidence** metric follows from it.
:::

## 7.6 Future sources

Reverse WHOIS and ASN walking for organization discovery, archives such as Wayback for historical
URLs, public code repositories for secrets and endpoints.
