<p align="center">
  <img src="https://img.shields.io/badge/status-design-lightgrey">
  <img src="https://img.shields.io/badge/golang-1.26-blue?logo=go">
  <img src="https://img.shields.io/badge/postgresql-18-blue?logo=postgresql">
</p>

> [!WARNING]
> Design stage, nothing is implemented yet. The design lives in `docs/`.

Personal attack surface management for bug bounty. Find assets the others miss, find them first, and
query them precisely.

Start with the [principles](docs/src/content/docs/architecture/principles.md), then the
[roadmap](docs/src/content/docs/architecture/roadmap.md).

## What it is

Recon owns an inventory and drives the services around it, each in its own repository:

| Service | Role |
|---|---|
| [ScopesExtractor](https://github.com/JoshuaMart/ScopesExtractor) | Program scopes from YesWeHack, HackerOne, Intigriti and Bugcrowd |
| [FastRecon](https://github.com/JoshuaMart/FastRecon) | Enumeration, DNS resolution, port scan, HTTP probe. One run, one JSON report |
| Fingerprinter | Browser rendering: technologies, favicon, scripts, cookies, headers, screenshot |

## Reading the design

```sh
cd docs && pnpm install && pnpm dev
```
