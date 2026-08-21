<p align="center">
  <img src="https://img.shields.io/badge/status-design-lightgrey">
  <img src="https://img.shields.io/badge/golang-1.26-blue?logo=go">
  <img src="https://img.shields.io/badge/postgresql-18-blue?logo=postgresql">
</p>

> [!WARNING]
> Work in progress. Foundations are in; the inventory is not.

Attack surface management for bug bounty. Find assets the others miss, and find them first.

The design lives in `docs/`. Start with the
[architecture](docs/src/content/docs/architecture/index.md), then the
[roadmap](docs/src/content/docs/architecture/roadmap.md).

## What it is

Recon owns an inventory and drives two scanners, each in its own repository:

| Service | Role |
|---|---|
| [FastRecon](https://github.com/JoshuaMart/FastRecon) | Enumeration, DNS resolution, port scan, HTTP probe. One run, one JSON report |
| Fingerprinter | Browser rendering: technologies, favicon, scripts, cookies, headers |

Neither holds a database credential. The scope, the scheduling, the correlation, the diff and the
authorization live in the control plane, which is where the value is.

## Running the stack

```sh
cp .env.example .env    # local credentials, never committed
make up                 # postgres, migrations, roles, control plane, renderer
make help               # everything else
```

What a pull request has to pass, which is also what CI runs:

```sh
make check              # vet, lint, unit tests
make test-integration   # needs Docker
```

## Reading the design

```sh
cd docs && pnpm install && pnpm dev
```

## Layout

| Directory | Contents |
|---|---|
| `cmd/` | The binaries: `controlplane`, `migrate` |
| `internal/` | The control plane's own code |
| `db/` | Migrations, embedded into the binary |
| `deploy/compose/` | The local stack, and the checks that guard its topology |
| `docs/` | The design record (Astro and Starlight) |
