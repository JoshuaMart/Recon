---
title: Glossary
description: The project vocabulary.
sidebar:
  order: 17
---

| Term | Definition |
|---|---|
| **Asset** | The stable, deduplicated identity of a piece of attack surface |
| **Observation** | A timestamped, immutable finding produced by one producer on one layer |
| **Layer** | What an observation describes: `dns`, `tcp`, `http` or `fingerprint`. One producer per layer |
| **Run** | One execution of a scanner over one perimeter, producing one report |
| **Discovery run** | A run that starts from a domain and asks what exists. Authoritative on presence |
| **Verification run** | A run over an explicit target list, asking what still answers. Authoritative on absence |
| **Candidate** | An asset discovered but never confirmed alive |
| **Unobservable** | An asset no observer gets a result on. Neither alive nor dead |
| **Facet** | An aggregation computed over a filtered search result |
| **Pivot** | A shared value (favicon, certificate key, internal script, cookie name) that links assets nothing else connects. It has to discriminate: a value that is everywhere is not a pivot, and neither is one that is nowhere twice |
| **Promoted column** | A value lifted out of the JSON payload into a typed column, because it is filtered often |
| **Informative failure** | A failure where an observer reached the target and reported an absence. The only kind that can lead to death |
