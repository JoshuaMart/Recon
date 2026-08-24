---
title: Technical base
description: The tooling and infrastructure choices, and what each one commits the project to.
sidebar:
  order: 13
---

This is the only chapter about implementation. It exists because the choices it carries are
**structural**: they decide what becomes expensive to change once the data model is frozen.

Every decision here is dated August 2026. The state of the ecosystem is an input, not a stable truth.

## 13.1 One language

Everything Recon runs is **Go**. The scanners are separate services with their own repositories, reached
over HTTP and JSON, and that contract is the only coupling surface.

One language is worth stating rather than assuming, because it is what a scanner reached over HTTP buys:
nothing here needs a second runtime, a second package manager, a second lockfile, a second lint
configuration or a second CI job. A scanner shipping as a static binary with an HTTP interface is a
dependency of the deployment, not of the build.

## 13.2 Repository structure

```
/
├── go.mod                  single module, Go 1.26
├── cmd/
│   ├── controlplane/       API, scheduler, notifier
│   ├── recon/              bootstrap and operational commands
│   └── migrate/
├── internal/
│   ├── config/             the only place in the repository that reads the environment
│   ├── store/              sqlc and pgx
│   ├── normalize/          the single function of 4.5
│   ├── scope/              in_scope / out_of_scope / unknown evaluation
│   ├── ct/                 the apex set, the label walk, the candidate writer
│   ├── run/                run definitions, signed URLs, report ingestion
│   ├── lifecycle/          transitions, qualification, backoff
│   ├── diff/               structured comparison, shared by the notifier and the timeline
│   ├── notify/             queue, aggregation, channels
│   ├── search/             AST compiler, facets, export
│   ├── fingerprint/        the Fingerprinter client
│   └── obs/                logs and metrics
├── db/
│   ├── migrations/         goose, versioned SQL
│   └── queries/            hand written SQL, the source for sqlc
├── api/                    the report ingestion contract, versioned
├── web/                    SvelteKit console, holds no database credential
├── deploy/compose/         the local stack
├── docs/
└── .github/workflows/
```

One `go.mod`, **no workspace**: workspaces justify themselves from several independently published modules.
No `pkg/` either. Nothing is meant to be imported from outside, and `internal/` gets the compiler to enforce
that boundary rather than a convention.

## 13.3 Self-hosted PostgreSQL

**PostgreSQL 18**, in a container, the same official image locally and in production.

### Partitioning without an extension

The [monthly partitioning](/architecture/data-model/#partitioning-from-the-first-migration) of `observation`
and `notification_event` is done by a repository SQL function, called daily by the maintenance loop.

`pg_partman` is the right tool as soon as several partitioned tables with different retentions are in play.
Here there are two, on a trivial monthly range, with a retention policy that reduces to a `DROP TABLE`. Thirty
lines of SQL against an extension to install, a non official image to follow, and one more component at
startup.

### Backups, the consequence of self-hosting

Choosing self-hosting transfers durability to the project. The database is the **only** non reproducible data
in the system: runs can be replayed, observation history cannot ([P3](/architecture/principles/)).

Continuous WAL archiving, with retention aligned on the observation retention policy. And **a restore that
has been exercised is worth a backup; an untested backup is worth nothing.** The restore test belongs to the
first milestone, not to an incident.

**The archive destination is configuration**, a local path in development and a bucket in production. That
is what makes the restore verifiable on a laptop instead of waiting for a production host to exist, and an
assertion that cannot go green until the end of the project is an assertion everyone learns to skip.

It has a second effect worth having: the restore path is exercised before it is needed, which is the only
moment anyone can afford to get it wrong.

In production this is the one thing that puts object storage back in the deployment, and it is a different
bucket, a different credential and a different lifecycle from the one
[13.7](#137-what-is-deliberately-not-a-dependency) declines.

## 13.4 Migrations: goose

The first milestone requires that a migration can be applied and then rolled back without loss. That
assertion rules out the declarative approach and ranks the rest.

| Tool | Verdict |
|---|---|
| **goose** | `Up` and `Down` in one file, a `NO TRANSACTION` directive (needed for `CREATE INDEX CONCURRENTLY` and attaching partitions), Go migrations for data transformations, `embed.FS`, a library API, MIT |
| golang-migrate | a "stable and frozen" v4 API, hundreds of open issues, `up` and `down` in separate files. The choice made by inertia |
| Atlas | an excellent declarative diff engine on a classic schema, but it fights range partitioning, and its interesting functions are commercial. A schema DSL is also the kind of layer [P5](/architecture/principles/) invites us to avoid |

Fixed conventions: **sequential** numbering rather than timestamps, validation in CI, and a **PostgreSQL
advisory lock** at startup so two control plane instances never migrate in parallel.

## 13.5 Data access: sqlc and pgx

SQL written by hand in `db/queries`, types generated by **sqlc** against `pgx/v5`, with `sqlc vet` in CI.

The schema sqlc reads is the goose migration directory itself, never a parallel `schema.sql`. Two truths about
the model at the exact moment it is frozen would be the worst debt available.

## 13.6 Configuration and secrets

`koanf`, with an explicit merge order: **defaults in code, then a YAML file, then environment variables
prefixed `RECON_`**. One typed structure, a `Validate()` at startup, immediate failure when a required field
is missing.

A strictly environment oriented library would do if everything were flat values. It will not be: port lists,
resolvers, [backoff thresholds](/architecture/lifecycle/#backoff-curves) and
[render cadences](/architecture/verification/#cadence-of-the-periodic-render) are structured data, which is
exactly what [P5](/architecture/principles/) requires them to stay.

Configuration is **passed explicitly**, never read from a package level global.

:::note[Enforced by the linter]
**No call to the environment outside `internal/config`.** Without an automated constraint, the "no hard coded
value" invariant holds for about three weeks.
:::

| Context | Mechanism |
|---|---|
| Local | `.env` ignored by git, `.env.example` committed |
| Durable | **SOPS and age**: an encrypted file committed, decrypted into the environment so the plaintext never touches disk |
| CI | repository secrets |
| Production | a systemd `EnvironmentFile` in 0600 |

**A secret scanner in CI and in a pre-commit hook.** The real failure mode is not a `.env` committed by
distraction. It is a scanner's API key ending up in a configuration file that legitimately carries a settings
section, which is why source keys travel through the run's environment and
[never through the run definition](/architecture/discovery/#73-source-credentials).

## 13.7 What is deliberately not a dependency

Three things the previous shape of this platform needed and this one does not. Each is listed with the
condition that brings it back, so nobody has to re-derive the reasoning.

| Not deployed | Why not | What brings it back |
|---|---|---|
| A shared token bucket store | FastRecon carries its own limiters, and the render budget is metered in one process | a second control plane process ([9.5](/architecture/deployment/#95-rate-limiting)) |
| Object storage for scan output | screenshots are not collected, and nothing else is large enough | collecting screenshots. Backups use their own bucket ([13.3](#133-self-hosted-postgresql)) |
| A message broker | the run queue is due dates in a table, read by one loop | genuine multi consumer fan out, or a throughput need |

Also out: Kubernetes, any ORM, and any declarative schema generation layer.

## 13.8 Tooling and CI

Go tooling is pinned by the `tool` directive in `go.mod`, so CI and a development machine run the same
generators, which removes an entire class of silent divergence.

| Job | Contents |
|---|---|
| `lint` | golangci-lint v2, the standard set plus `errcheck`, `gosec`, `bodyclose`, `rowserrcheck`, `sqlclosecheck`, `depguard`, `forbidigo`, with `gofumpt` and `goimports` |
| `test` | `go test ./... -race` for unit tests. Integration tests behind a build tag, on **testcontainers** |
| `build` | binaries and images |
| `console` | the SvelteKit lint, typecheck and build |

The [milestones become the integration suite](/architecture/roadmap/#project-rules), so the integration test
infrastructure is set up in the first phase, not when it is first needed.

:::caution[The trap in "CI passes on an empty pull request"]
Never put a path filter on a workflow declared as a required check. A filtered required check stays pending
forever on a pull request that does not touch those paths, and the pull request becomes unmergeable. It is the
most common way to make that assertion false.
:::

## 13.9 Minimal observability

Two things outside the usual first phase list, kept because retrofitting them is expensive:

- **Structured logs with a correlation id** carried end to end from ingestion. Grafting correlation onto an
  existing pipeline means touching every write path.
- **A metrics endpoint** on the control plane. Several milestone assertions *are* metrics: the deduplication
  rate above 90 %, the 10 % `unobservable` threshold, per program CT coverage. Without counters from the
  start, those are one off SQL queries instead of alerts.

Distributed tracing waits for a real need.

## 13.10 Summary

| Role | Choice |
|---|---|
| Control plane | Go 1.26 |
| Scanning | FastRecon, a container image, no privilege |
| Rendering | Fingerprinter, a long-running service with a Chrome sidecar |
| Certificate Transparency | `certstream-server-go` as the feed, an image. The matcher is a loop in the control plane |
| Console | SvelteKit, pnpm, Node adapter, interface in English |
| Database | PostgreSQL 18, self-hosted, partitioning without an extension |
| Migrations | goose |
| Data access | sqlc and pgx/v5 |
| Configuration | koanf, one typed structure validated at startup |
| Secrets | environment, SOPS and age, a secret scanner |
| Lint | golangci-lint v2 |
| Integration tests | testcontainers |
