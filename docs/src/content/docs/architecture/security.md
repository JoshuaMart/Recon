---
title: Security and multi-tenancy
description: Multi-tenant and multi-user are two axes with very different retrofit costs. What is irreversible, what is not, what stays out.
sidebar:
  order: 11
---

Two distinct axes:

- **Multi-tenant**, several isolated organizations. Irreversible, so it is handled in the first migration.
- **Multi-user**, several people inside one organization. Mostly retrofittable; only six points have to be
  frozen now.

The collaborative interface stays a [v1 non-goal](/architecture/vision/#11-non-goals-for-v1). The backend
has to be ready for it.

## 11.1 Irreversible decisions

1. **`org_id` on every table from the first migration.** The columns are what is urgent, not the policy.

   Row-Level Security lands once every query path is written and stable, and not in post-v1: it becomes
   necessary the moment a second tenant exists, which can happen before v1 if a personal space sits next
   to a client one. In the meantime the guarantee rests on `org_id` being present in every query, which is
   a convention, and a **static test** turns it into a build failure. See the box below.

2. **Identity through a join table, never `user.org_id`.** A user has to be able to belong to several
   organizations without a migration of the authentication layer.

   ```sql
   CREATE TABLE app_user (
     id         uuid PRIMARY KEY,
     email      text UNIQUE NOT NULL,
     created_at timestamptz NOT NULL DEFAULT now()
   );

   CREATE TABLE membership (
     user_id uuid NOT NULL REFERENCES app_user(id),
     org_id  uuid NOT NULL REFERENCES org(id),
     role    text NOT NULL DEFAULT 'owner',
     PRIMARY KEY (user_id, org_id)
   );
   ```

3. **Write attribution.** `created_by` and `updated_by`, nullable, on `program` and `scope_rule`. The actor
   can be a human or the system, and the two must be distinguishable. The column can be added later; the
   history cannot.

4. **One place for authorization.** Every request goes through a layer receiving a principal
   (`org_id`, `actor_id`, `role`), even while `role` has one value. Checks scattered inline would force an
   audit of every endpoint the day a second role appears.

5. **An optimistic lock on mutable configuration.** A `version` column on `program` and `scope_rule`. Even
   with one user, two concurrent writes lose each other silently, and a lost scope produces a scan outside
   the perimeter.

6. **Tokens belong to the organization.** `api_token(org_id, created_by, scopes[], expires_at)`, with only
   the hash stored. A token modelled as belonging to a person becomes unmanageable when that person leaves,
   or for machine access.

:::caution[The tenant guard declares, it does not deduce]
A guard that looks for `org_id` in the text of a query cannot decide isolation: a correlated subquery
carrying `a.org_id = b.org_id` satisfies it while returning every tenant. Each tightening would produce a
new bypass, and the failure mode is the worst available, a **false green** that removes vigilance without
supplying the property.

So the burden is inverted. Every query declares its intent:

```sql
-- @tenant: scoped      filtered on org_id at statement level
-- @tenant: cross-org   crosses tenants, justification required
-- @tenant: none        touches no table carrying org_id
```

The guard no longer interprets SQL. It checks that an annotation exists, that it is not contradicted by
something simple and exact, and that `cross-org` carries a reason. An unannotated query fails the build.

The one thing it still decides, it decides exactly: **a correlation is not a filter.** `org_id = $2` says
*which* tenant; `a.org_id = b.org_id` keeps two tables in step and isolates nothing. An `INSERT` naming
`org_id` in its column list is the second legitimate form, since a write supplies a tenant rather than
filtering one.

Two properties the deduction never had: adding a query forces a conscious decision at the moment the error
is visible, and grepping for `cross-org` gives the exhaustive list to audit. That list does not exist while
undetected cross-tenant queries pass in silence, and it is exactly what writing the RLS policies needs.
:::

### Row-Level Security: two roles rather than one variable

A **third role**, `asm_sys`, carrying `BYPASSRLS`, for the scheduler, the deadline sweeper and the Notifier.
`asm_app` stays subject to RLS for everything else. The roles are described in
[9.6](/architecture/deployment/#96-postgresql-roles).

**What is rejected**: relying only on a session variable read by uniform policies. A forgotten `SET LOCAL`
then produces not an error but **zero rows**, which is precisely the silent failure the tenant guard was
rewritten to eliminate. And a component that legitimately crosses tenants would have no way to say so
except by not setting the variable, which is an omission, indistinguishable from forgetting.

**With two roles the property is structural.** A component crosses tenants because it connected with the
role that allows it. That reads in the deployment configuration rather than in the body of each query, and
the inventoried `cross-org` queries become the exact specification of what `asm_sys` must cover.

**`SET LOCAL app.org_id` is still needed** for `asm_app`, since that is what the policies read. What changes
is the detectability of its absence: a scoped query with no organization set returns zero rows, so it breaks
the integration suite instead of passing unnoticed.

**The implementation trap, and it is central.** The variable is set at the start of **the transaction
carrying the query**, never when the connection is acquired. `SET LOCAL` is transaction scoped and
disappears at commit, which is exactly the property wanted. A plain `SET` at acquisition survives the
connection going back to the pool, and the next query from another tenant inherits the previous context.
That would be the cross-tenant leak RLS exists to prevent, introduced by the mechanism meant to prevent it.

**`BYPASSRLS` needs a superuser to grant**, which managed databases generally do not give. The fallback is a
`USING (true)` policy granted to `asm_sys` alone, which produces the same effect without the attribute. It
is less direct, and it moves the property from the role to the policy, therefore to something a migration
can forget to carry onto a new table.

**The fallback is built and exercised from the start, not deferred to migration day.** The reason is the
moment rather than the cost: on the day of a move to a managed database, discovering under pressure, with
data to shift, that the multi-tenant isolation mechanism cannot be installed is exactly the circumstance
that gets RLS disabled "temporarily". The migration therefore **detects the available path** instead of
failing, and the fallback is **tested like the main path**. A fallback nobody exercises is no better than an
absent one: it only adds the certainty of having one.

**A test suite connected as the owner verifies nothing.** A policy does not apply to a table's owner, so an
RLS suite run as `asm_owner` passes **entirely** without exercising anything. The connection identity is
therefore **asserted** in the suite's preamble rather than assumed from configuration:

```sql
SELECT rolsuper, rolbypassrls FROM pg_roles WHERE rolname = current_user;
```

The suite fails if either is true, or if the current role owns the tables being queried. Without that check,
a modified test connection string silently makes the whole RLS coverage inoperative while leaving the
milestone green, and a green milestone on an absent property is what teaches people to stop reading
milestones.

## 11.2 Explicitly out of scope

Each of these adds without a structural migration and must not be built for v1:

RBAC with role and permission tables, invitations, SSO and SAML, asset level permissions (organization
isolation is enough), an audit log interface, per user notification preferences, presence, comments,
assignment.

## 11.3 Other guardrails

- **No arbitrary code execution in configuration** ([P5](/architecture/principles/)).
- **Authorization to scan is first class data.** A run that cannot be traced to a `program` with a valid,
  unexpired authorization does not start. It is also the natural place for per organization quotas later.
- **Rate limiting per program** ([9.5](/architecture/deployment/#95-rate-limiting)).
- **Scanners are untrusted by assumption** ([P6](/architecture/principles/)): a frozen perimeter in the run
  definition, no database credential, and every conclusion re-derived on arrival.

### The perimeter in the run definition is defence in depth

Two mechanisms hide under "the run carries its scope", and they do not have the same status.

**Authenticating the report** is not optional. Without it, `/reports` is open.

**Freezing the perimeter in the run definition**, which stops a scanner widening its own scope, is defence
in depth. The real control is the [server side re-evaluation of the scope at every ingestion](/architecture/scope/#52-re-evaluated-at-ingestion):
observations from a scanner lying about its scope would be reclassified anyway, and out of list hosts on a
verification run are rejected outright.

## 11.4 What a run holds

| Path | Mechanism | Persisted |
|---|---|---|
| Console to API | `api_token` ([11.1](#111-irreversible-decisions)) | yes |
| Run to `POST /reports` | a signed token bound to the run | **no** |
| Run to its target list | a signed URL bound to the run | **no** |
| Control plane to Fingerprinter | network restriction, mTLS as a second choice | no |

**A run's credential is not an `api_token`.** An `api_token` is a long lived object attached to an
organization, meant for the public API. An ephemeral job carrying a long lived organization token is exactly
the failure mode to avoid: the job is disposable, the token is not.

Both of a run's credentials are HMACs over `(run_id, purpose, expiry)`
([9.1](/architecture/deployment/#91-the-run-contract)). Three properties follow with no extra machinery: the
binding to the run is in the claims, the scope is bounded by the run's frozen target list, and expiry is
intrinsic. No table, no revocation, no purge job.

**A run never reads the inventory.** Everything it needs is in its definition and its target list. The
report token authorizes writing one report and nothing else, so a compromised run cannot exfiltrate an
organization's perimeter.

**Revocation is by run state.** A run in a terminal state makes any later report bearing its id fail. A
signed token cannot be recalled; it simply stops being useful.

**Control plane to Fingerprinter.** The service is [isolated and renders hostile pages](/architecture/verification/#85-network-isolation),
and the goal is that only the control plane reaches it. In order of preference: a network restriction, then
mTLS if the provider offers nothing satisfactory. A shared secret in an environment variable is explicitly
rejected, since it gets duplicated everywhere and never rotated.
