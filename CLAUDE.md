# Recon

Personal attack surface management for bug bounty: one user, one instance. Recon keeps an inventory of
every program hunted on, keeps it fresh, and makes it queryable through a filter language.

## Status

Phases 0 to 2 are done: foundations, programs and the ScopesExtractor sync, and recon with ingestion.
A recon is queued on a wildcard, sent to `fastrecon serve`, and its report becomes assets, normalized
and deduplicated observations, and changes (a repeated recon on a real wildcard writes no new
observation). Phase 3 (verification and lifecycle) is next. The design in `docs/` is the source of
truth, and code follows it.

## Read before working

The design lives in `docs/src/content/docs/architecture/`. Read in this order:

1. `principles.md`: the rules every decision answers to
2. `overview.md`: the components and their edges
3. `roadmap.md`: the build order, and which phase is current
4. The page for the area you touch: `programs.md`, `data-model.md`, `discovery.md`, `lifecycle.md`,
   `fingerprinting.md`, `search.md`, `notifications.md`, `interface.md`, `api.md`, `stack.md`
5. `glossary.md` when a term is unclear

The homepage (`docs/src/content/docs/index.mdx`) holds the goals and what is in and out of scope.

## The system in one paragraph

Scope comes from ScopesExtractor (polled) or from manual programs. Discovery is never periodic: a
FastRecon recon runs on a wildcard on demand or, if configured, when a new wildcard appears; Certificate
Transparency, BBOT imports and manual entries add candidates. Verification is periodic: FastRecon probes
explicit lists of known hosts on due dates, and failures are qualified before they count. The
Fingerprinter renders web services and is the only source of technologies, versions and screenshots.
Everything is searchable through a text DSL; every change is recorded and shown in the UI, and only a
few events notify.

## External services

| Service | Contract |
|---|---|
| ScopesExtractor | REST, `GET /` and `GET /wildcards`, `X-API-KEY` header |
| FastRecon | started by a runner with CLI flags, fetches its targets, posts one JSON report |
| Fingerprinter | `POST /scan`, `GET /health`, `GET /detections`, answers `429` when saturated |
| certstream-server-go | websocket feed of certificates |

They are separate projects. Recon never embeds them and they never reach its database.

## Stack

Go single binary (API, embedded UI, loops), SvelteKit with the static adapter, PostgreSQL 18, sqlc and
pgx/v5, goose, koanf. Deployed with Docker Compose on one VPS. Details in `stack.md`.

## Rules

- A structural decision not in the design goes into the docs first, then into code.
- Keep the docs short: a diagram (Mermaid is supported) or a table first, a few lines of rationale at
  most. No incident stories in the design.
- Docs and UI copy are in English, sentence case headings, no em dashes.
- A test that does not contain the discriminating case measures nothing: check it fails without the fix.

## Commands

```sh
cp .env.example .env        # then fill it in
make up                     # build and start the stack, wait until healthy
make check                  # vet, lint, unit tests, sqlc diff
make test-integration       # needs Docker, finds the socket from the Docker context
make verify-isolation       # network isolation of the running stack
make help                   # everything else
```

Configuration is read only in `internal/config` (enforced by the linter). `config.example.yml` must
list every setting with its default, which a test checks. SQL lives in `db/queries`, run `make sqlc`
after changing it or a migration.

## Docs site

```sh
cd docs && pnpm install && pnpm dev    # http://localhost:4321
pnpm build                             # must pass before committing doc changes
```

The sidebar is generated from `architecture/`, ordered by each page's `sidebar.order`.
