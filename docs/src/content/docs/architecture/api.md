---
title: HTTP API
description: One surface, two kinds of caller, and a machine readable description of it. What the answers are careful about, and where the description can fall behind.
sidebar:
  order: 15
---

The control plane's whole surface is described in [`openapi.yaml`](/openapi.yaml), an OpenAPI 3.1 document
covering twenty four operations. It is the file to hand to anything that has to call this without reading the
Go.

This chapter is not a copy of it. It says what the surface is shaped by, which a schema cannot carry.

## 15.1 Two kinds of caller, told apart by the credential

There is no route that decides who is asking by reading a body.

A **console token** belongs to an organization and carries a set of actions. Every route it reaches goes
through one authorization layer, which turns the token into a principal before the handler sees anything.
That layer exists so that the day a second role appears there is one place to audit, rather than a check
inlined at every endpoint and an audit nobody performs.

The three actions are `read_assets`, `manage_scope` and `manage_jobs`, and the split is not tidiness.
Entering an asset by hand is an assertion about the perimeter, which is a different privilege from reading
what a scanner found. And both render triggers hold `manage_jobs` rather than `ingest`: something holding
`ingest` could otherwise schedule renders of its choosing and spend a programme's budget on targets it
picked.

A **run token** is signed, short lived, and belongs to one run. It reaches two routes, the target list that
run is allowed to read and the report it posts back, and nothing else. It cannot be recalled, so revocation
is the run reaching a terminal state and the routes going quiet ([11.4](/architecture/security/#114-what-a-run-holds)).

The tenant is never a parameter. It is discovered from the credential, once, and
[the query language has no field to express it](/architecture/search/#103-the-compiler-and-what-it-does-not-delegate).

## 15.2 What the answers are careful about

Four behaviours in the document look like quirks until the reason is written down.

**A `404` says nothing about existence.** An identifier that does not exist and one belonging to another
organization answer identically, and a malformed identifier answers `404` rather than `400` for the same
reason. Telling them apart would leak one bit, and one bit is enough to enumerate a tenant.

**A `401` and a `403` are deliberately different.** A credential that is missing, wrong or expired gets one
answer for all three cases, because which one it was belongs in a log. A credential that authenticated and
lacks the action gets the other. That difference is what makes the separation of actions usable rather than
mysterious.

**A `200` is not always a yes.** Asking for a render answers `200` with `queued: false` and a reason when the
asset has left the scheduler, sits outside the perimeter, or listens on a port a browser refuses to open.
Answering otherwise would tell a caller to wait for a render nobody will make. Starting a run answers `200`
with `started: false` when nothing was due, which is the normal state of a healthy inventory rather than a
failure.

**A refusal names itself.** Every error body carries a machine readable `error` beside a sentence in
`detail`. A query naming a field the registry does not describe is refused by name rather than answered with
an empty result set, because an empty page reads as an empty inventory, which is a different thing.

The one answer worth reading twice is `202` with `started: false` on
[a run](/architecture/deployment/#98-starting-runs). The row exists and the platform did not start it. The
deadline sweeper owns that run and nothing has to be repaired by hand, so retrying is the action that
creates a second one.

## 15.3 The description is written, not generated

There is no annotation pass and no generator. The routes are a plain `http.ServeMux` and the document is
maintained beside it.

That is a real cost and it is worth naming rather than discovering. Nothing fails to compile when a handler
changes shape and the document does not, so the two can drift in silence, which is the same failure mode
[the wire contract of a summary](/architecture/deployment/#93-ingesting-a-report) is written to avoid.

Two things bound it.

The route list is checked mechanically: the operations in the document and the `mux.Handle` calls in
`cmd/controlplane/main.go` are two lists of the same thing, and a test compares them in both directions. An
endpoint served and undescribed is the one somebody calling this without reading the Go finds out about the
hard way; an operation described and unserved is the one they trust and get a 404 from.

The check was written when it was one line of work and it found one immediately, which is the argument for
it: the coverage panel of phase 8 had been served for a while with nothing in the document naming it, and
nothing anywhere had said so.

The search vocabulary is not, and that is the part most likely to fall behind. The field list appears twice,
once in the document and once served live by `GET /assets/fields`. The served one is authoritative and the
document says so. A field added to the registry is two edits, in that order.

Serving the vocabulary rather than freezing it is the same decision as
[serving the enrichment state](/architecture/console/#143-what-the-console-does-not-decide): a client that
learns what a deployment accepts by collecting `400`s learns it wrong.

## 15.4 What the surface does not have yet

**No textual query language.** The filter is a tree because
[the query is a document](/architecture/search/#101-three-principles). A textual language comes later and
produces the same tree, which is what avoids freezing a syntax before anybody knows what actually gets
filtered.

**No pagination that survives a schema change.** Cursors encode the columns a walk is ordered by, so the one
from the flat list and the one from [the folded list](/architecture/search/#107-the-list-is-a-list-of-hosts)
are not interchangeable and the wrong one is refused. That refusal is the feature: behind a single route, a
client that kept a cursor and flipped a flag would get a walk that restarts or skips, and neither says
anything.

**No MCP server.** An agent calling this today reads the OpenAPI document and speaks HTTP. A server in front
of the search API is [post-v1](/architecture/roadmap/#post-v1), and it would be a façade over three routes
rather than a second surface.
