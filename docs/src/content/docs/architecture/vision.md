---
title: Vision
description: Coverage and freshness, two goals normally in tension, carried by mechanisms that do not compete.
sidebar:
  order: 1
---

An attack surface management platform aimed at bug bounty and offensive reconnaissance, with two
goals held at once:

- **Coverage.** Find assets the others do not.
- **Freshness.** Find them before the others do.

These pull against each other. Coverage wants heavy, infrequent sweeps; freshness wants light,
constant checks. The architecture separates them so they draw on different budgets:

| Goal | Mechanism | Cost | Frequency |
|---|---|---|---|
| Coverage | A full [FastRecon](/architecture/discovery/) run from the program's apexes | high, in bursts | daily to weekly |
| Freshness | [Certificate Transparency](/architecture/discovery/#75-certificate-transparency), then a single-host run | near zero, continuous | real time |

Personal use first, but built from the start so that becoming a multi-tenant service is a
configuration change rather than a rewrite. The decisions that make that true are listed in
[Security and multi-tenancy](/architecture/security/); they are the ones that cannot be retrofitted.

## 1.1 Non-goals for v1

- No automated vulnerability scanning. Nuclei and its family come later, downstream of the inventory.
- No deep crawl or spider.
- No organization discovery: ASN walking, acquisitions, reverse WHOIS.
- No collaborative multi-user interface. The backend is ready for it; the screens are not built.
