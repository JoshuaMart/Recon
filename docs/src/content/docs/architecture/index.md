---
title: Architecture
description: The design record of the platform. What is built and why, not how it is coded.
sidebar:
  label: How to read this
  order: 0
---

This is the design record. It states what the platform does and why, along with the constraints that
forced each decision. Implementation belongs in the code, with one exception: the
[technical base](/architecture/stack/) carries the tooling choices that are too structural to be
settled inside a pull request.

:::caution[Status]
Working draft. Expect it to move.
:::

## The sections

| Section | Contents |
|---|---|
| [Vision](/architecture/vision/) | Coverage and freshness, and why they are decoupled |
| [Principles](/architecture/principles/) | The six invariants every decision answers to |
| [Overview](/architecture/overview/) | The components and the direction of every edge |
| [Data model](/architecture/data-model/) | Assets, observations, current state, canonical keys, lineage |
| [Scope](/architecture/scope/) | A persistent, versioned perimeter owned by the control plane |
| [Asset lifecycle](/architecture/lifecycle/) | Death by layer, the state machine, scheduling and backoff |
| [Discovery](/architecture/discovery/) | FastRecon as the engine, and the Certificate Transparency stream |
| [Verification](/architecture/verification/) | Probing the inventory, the fingerprinter, reachability |
| [Runs and deployment](/architecture/deployment/) | The run contract, report ingestion, roles, bootstrap |
| [Search and filters](/architecture/search/) | The query AST, facets, pivots, and what a screen must distinguish |
| [Security and multi-tenancy](/architecture/security/) | The irreversible decisions, taken now |
| [Notifications and diff](/architecture/notifications/) | What turns an inventory into a product |
| [Technical base](/architecture/stack/) | Tooling, infrastructure, and what each choice commits us to |
| [Console](/architecture/console/) | The interface: framework, credential, art direction |
| [Roadmap](/architecture/roadmap/) | Seven phases, each closed by a verification milestone |
| [Glossary](/architecture/glossary/) | The project vocabulary |

## Two external services

Recon does not scan by itself. It owns the inventory and drives two tools that do the scanning, each
in its own repository:

| Service | Role |
|---|---|
| [FastRecon](https://github.com/JoshuaMart/FastRecon) | Enumeration, DNS resolution, port scan, HTTP probe. One run, one JSON report |
| Fingerprinter | Browser rendering: technologies, favicon, scripts, cookies, headers |

Both are replaceable capabilities behind a contract ([P4](/architecture/principles/)). The value is
in the inventory, not in the scanners.
