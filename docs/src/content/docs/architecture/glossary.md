---
title: Glossary
description: The project vocabulary.
sidebar:
  order: 15
---

| Term | Definition |
|---|---|
| **Program** | One bug bounty program or contract, synced from ScopesExtractor or entered by hand |
| **Scope rule** | An include or exclude rule on a program, opened and closed, never deleted |
| **Wildcard** | An apex rule, `*.example.com`, with its own recon history |
| **Asset** | The stable identity of a host, an address, a service or a declared URL |
| **Observation** | What one scanner saw on one layer, over a time span. Append only. Only FastRecon and the Fingerprinter write them |
| **Layer** | `dns`, `tcp`, `http` or `fingerprint`. One producer per layer |
| **Change** | One recorded event on an asset: created, an observation that differs, a lifecycle or scope move. What the feed shows |
| **Stage** | How far a FastRecon run goes: `enum`, `resolve`, `ports` or `full`, each including the ones before |
| **Recon run** | FastRecon enumerating a wildcard. On demand, never periodic |
| **Verification run** | FastRecon probing an explicit list of known hosts. Periodic |
| **Render** | One Fingerprinter scan of a service: technologies, headers, screenshot |
| **Candidate** | An asset known but never confirmed alive |
| **Unobservable** | An asset nothing gets through to. Neither alive nor dead |
| **Observer** | What can get a result on an asset: FastRecon for a host, FastRecon's HTTP probe and the Fingerprinter for a web service |
| **Informative failure** | A failure where the target answered with an absence. The only kind that can lead to death |
| **Triage** | Your own state on an asset: new, seen, interesting, ignored |
| **Saved search** | A named query, optionally alerting on new matches |
| **Facet** | A count over the current search result, clickable into a filter |
| **Pivot** | A value shared by a few assets that links them: favicon, script, cookie name |
