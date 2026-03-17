# API Reference

Base URL: `http://localhost:3002`

---

## Authentication

### POST /api/auth/login

Login with password, returns JWT + refresh token.

**Request body:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `password` | string | Yes | User password |

**Response (200):**

```json
{
  "token": "eyJhbG...",
  "refresh_token": "c2f1a8...",
  "expires_at": "2026-03-17T12:00:00Z"
}
```

**Errors:** `400` invalid body, `401` invalid password

---

### POST /api/auth/refresh

Refresh an access token using a refresh token.

**Request body:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `refresh_token` | string | Yes | Previously issued refresh token |

**Response (200):**

```json
{
  "token": "eyJhbG...",
  "refresh_token": "d3e2b9...",
  "expires_at": "2026-03-17T12:00:00Z"
}
```

**Errors:** `400` invalid body, `401` invalid or expired refresh token

---

## Wildcards

All endpoints require `Authorization: Bearer <token>`.

### GET /api/wildcards

List all wildcards with stats.

**Response (200):**

```json
[
  {
    "id": "uuid",
    "value": "*.example.com",
    "active": true,
    "last_recon_at": "2026-03-16T10:00:00Z",
    "last_revalidated_at": "2026-03-16T10:00:00Z",
    "stats": {
      "hostnames_total": 150,
      "hostnames_alive": 120,
      "hostnames_dead": 20,
      "hostnames_unreachable": 10,
      "web_services": 85
    }
  }
]
```

---

### POST /api/wildcards

Create a new wildcard.

**Request body:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `value` | string | Yes | Wildcard pattern (format: `*.domain.tld`) |

**Response (201):**

```json
{
  "id": "uuid",
  "value": "*.example.com",
  "active": true
}
```

**Errors:** `400` invalid format, `409` already exists

---

### DELETE /api/wildcards/{id}

Delete a wildcard and cascade delete all related hostnames, web results, and jobs.

**Path parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `id` | UUID | Wildcard ID |

**Response:** `204 No Content`

**Errors:** `400` invalid ID

---

### POST /api/wildcards/{id}/recon

Launch a reconnaissance job for a wildcard.

**Path parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `id` | UUID | Wildcard ID |

**Request body (optional):**

| Field | Type | Required | Default | Description |
|-------|------|----------|---------|-------------|
| `mode` | string | No | `normal` | Recon mode: `normal` or `intensive` |

**Response (202):**

```json
{
  "job_id": "uuid",
  "scaleway_job_id": "scw-xxx",
  "status": "pending"
}
```

**Errors:** `400` invalid ID or mode, `404` wildcard not found, `409` job already running or max concurrent jobs reached (10)

---

### PUT /api/wildcards/{id}/revalidate

Trigger on-demand revalidation for all hostnames of a wildcard.

**Path parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `id` | UUID | Wildcard ID |

**Response:** `202 Accepted`

**Errors:** `400` invalid ID, `404` wildcard not found

---

## Hostnames

All endpoints require `Authorization: Bearer <token>`.

### GET /api/hostnames

List hostnames with optional filters and pagination.

**Query parameters:**

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `wildcard_id` | UUID | — | Filter by wildcard |
| `status` | string | — | Filter by status: `alive`, `dead`, `unreachable` |
| `type` | string | — | Filter by type: `web`, `other`, `unknown` |
| `page` | int | `1` | Page number |
| `per_page` | int | `50` | Results per page (max `200`) |

**Response (200):**

```json
{
  "data": [
    {
      "id": "uuid",
      "wildcard_id": "uuid",
      "fqdn": "sub.example.com",
      "ip": "1.2.3.4",
      "cdn": "cloudflare",
      "status": "alive",
      "type": "web",
      "last_seen_at": "2026-03-16T10:00:00Z",
      "created_at": "2026-03-15T08:00:00Z",
      "updated_at": "2026-03-16T10:00:00Z"
    }
  ],
  "total": 150,
  "page": 1,
  "per_page": 50
}
```

**Errors:** `400` invalid wildcard_id

---

### GET /api/hostnames/{id}

Get a single hostname with full DNS and ports data.

**Path parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `id` | UUID | Hostname ID |

**Response (200):**

```json
{
  "id": "uuid",
  "wildcard_id": "uuid",
  "fqdn": "sub.example.com",
  "ip": "1.2.3.4",
  "cdn": "cloudflare",
  "status": "alive",
  "type": "web",
  "last_seen_at": "2026-03-16T10:00:00Z",
  "created_at": "2026-03-15T08:00:00Z",
  "updated_at": "2026-03-16T10:00:00Z",
  "dns": {
    "A": ["1.2.3.4"],
    "CNAME": ["cdn.example.com"]
  },
  "ports": {
    "tcp": {
      "80": { "web": "http://sub.example.com" },
      "443": { "web": "https://sub.example.com" }
    },
    "udp": {}
  }
}
```

**Errors:** `400` invalid ID, `404` not found

---

## URLs / Fingerprints

All endpoints require `Authorization: Bearer <token>`.

### GET /api/urls

List URLs (lightweight, no full fingerprint payload).

**Query parameters:**

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `wildcard_id` | UUID | — | Filter by wildcard |
| `hostname_id` | UUID | — | Filter by hostname |
| `page` | int | `1` | Page number |
| `per_page` | int | `50` | Results per page (max `200`) |

**Response (200):**

```json
{
  "data": [
    {
      "id": "uuid",
      "hostname_id": "uuid",
      "url": "https://sub.example.com",
      "status_code": "200",
      "title": "Example Site",
      "scanned_at": "2026-03-16T10:00:00Z"
    }
  ],
  "total": 85,
  "page": 1,
  "per_page": 50
}
```

**Errors:** `400` invalid wildcard_id or hostname_id

---

### GET /api/urls/{id}

Get full fingerprint data for a URL.

**Path parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `id` | UUID | Web result ID |

**Response (200):**

```json
{
  "id": "uuid",
  "hostname_id": "uuid",
  "url": "https://sub.example.com",
  "chain": [ ... ],
  "technologies": [ ... ],
  "cookies": [ ... ],
  "metadata": { ... },
  "external_hosts": [ ... ],
  "scanned_at": "2026-03-16T10:00:00Z",
  "created_at": "2026-03-15T08:00:00Z",
  "updated_at": "2026-03-16T10:00:00Z"
}
```

**Errors:** `400` invalid ID, `404` not found

---

### POST /api/urls/fingerprint

Manually enqueue a URL for fingerprinting.

**Request body:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `url` | string | Yes | URL to fingerprint (must start with `http://` or `https://`) |

**Response:** `202 Accepted`

**Errors:** `400` invalid body or URL format, `404` hostname not found for this URL

---

## Jobs

All endpoints require `Authorization: Bearer <token>`.

### GET /api/jobs

List recon jobs with optional filters and pagination.

**Query parameters:**

| Parameter | Type | Default | Description |
|-----------|------|---------|-------------|
| `wildcard_id` | UUID | — | Filter by wildcard |
| `status` | string | — | Filter by status: `pending`, `running`, `completed`, `failed` |
| `page` | int | `1` | Page number |
| `per_page` | int | `50` | Results per page (max `200`) |

**Response (200):**

```json
{
  "data": [
    {
      "id": "uuid",
      "wildcard_id": "uuid",
      "mode": "normal",
      "status": "completed",
      "scaleway_job_id": "scw-xxx",
      "started_at": "2026-03-16T09:00:00Z",
      "completed_at": "2026-03-16T09:30:00Z",
      "created_at": "2026-03-16T09:00:00Z"
    }
  ],
  "total": 25,
  "page": 1,
  "per_page": 50
}
```

**Errors:** `400` invalid wildcard_id

---

### GET /api/jobs/{id}

Get a single job record.

**Path parameters:**

| Parameter | Type | Description |
|-----------|------|-------------|
| `id` | UUID | Job ID |

**Response (200):**

```json
{
  "id": "uuid",
  "wildcard_id": "uuid",
  "mode": "normal",
  "status": "completed",
  "scaleway_job_id": "scw-xxx",
  "started_at": "2026-03-16T09:00:00Z",
  "completed_at": "2026-03-16T09:30:00Z",
  "created_at": "2026-03-16T09:00:00Z"
}
```

**Errors:** `400` invalid ID, `404` not found

---

## Ingest (service-to-service)

These endpoints are used by recon jobs and Certstream to push data into the API. They require the `X-API-Key` header.

### POST /api/ingest/recon

Receive hostname data from a recon job.

**Headers:**

| Header | Required | Description |
|--------|----------|-------------|
| `X-API-Key` | Yes | Ingest API key |

**Request body:**

```json
{
  "job_id": "uuid",
  "host": {
    "fqdn": "sub.example.com",
    "ip": "1.2.3.4",
    "cdn": "cloudflare",
    "dns": {
      "A": ["1.2.3.4"],
      "CNAME": ["cdn.example.com"]
    },
    "ports": {
      "tcp": {
        "80": { "web": "http://sub.example.com" },
        "443": { "web": "https://sub.example.com" },
        "22": { "web": null }
      },
      "udp": {}
    }
  }
}
```

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `job_id` | UUID | Yes | Job ID (wildcard is resolved from the job) |
| `host.fqdn` | string | Yes | Fully qualified domain name |
| `host.ip` | string | No | IP address |
| `host.cdn` | string | No | CDN provider |
| `host.dns` | object | Yes | DNS resolution data |
| `host.ports` | object | Yes | Port scan results |
| `host.ports.tcp.<port>.web` | string/null | No | URL if web service detected on port |

**Response:** `200 OK`

**Behavior:**
- Hostname status: `alive` if IP or ports present, `unreachable` if both null
- Hostname type: `web` if any port has a web URL, `other` if ports but no web, `unknown` if no data
- All web URLs from ports are enqueued for fingerprinting (`source = recon`)

**Errors:** `400` invalid body or missing fqdn, `401` invalid API key

---

### POST /api/ingest/certstream

Receive a URL discovered via certificate transparency logs.

**Headers:**

| Header | Required | Description |
|--------|----------|-------------|
| `X-API-Key` | Yes | Ingest API key |

**Request body:**

| Field | Type | Required | Description |
|-------|------|----------|-------------|
| `url` | string | Yes | URL from certificate stream |

**Response:**
- `200 OK` — URL matched a wildcard and was processed
- `204 No Content` — No matching wildcard found

**Behavior:**
- Extracts hostname from URL and matches against all active wildcards
- If hostname is new: creates record with `status = unreachable`, `type = unknown`
- Enqueues URL for fingerprinting (`source = certstream`)

**Errors:** `400` invalid body or URL, `401` invalid API key

---

## Health

### GET /health

**Response:** `200 OK` — plain text `ok`

---

## Common patterns

### Authentication headers

```
# JWT (protected endpoints)
Authorization: Bearer <token>

# API Key (ingest endpoints)
X-API-Key: <key>
```

### Pagination

All list endpoints support pagination:

| Parameter | Type | Default | Max | Description |
|-----------|------|---------|-----|-------------|
| `page` | int | `1` | — | Page number (1-indexed) |
| `per_page` | int | `50` | `200` | Results per page |

Paginated responses include:

```json
{
  "data": [ ... ],
  "total": 150,
  "page": 1,
  "per_page": 50
}
```

### Error format

All errors return JSON:

```json
{
  "error": "description of the error"
}
```

### Status codes

| Code | Meaning |
|------|---------|
| `200` | Success |
| `201` | Created |
| `202` | Accepted (async processing) |
| `204` | No Content |
| `400` | Bad Request |
| `401` | Unauthorized |
| `404` | Not Found |
| `409` | Conflict |
| `500` | Internal Server Error |

### Global limits

- Max concurrent recon jobs: **10**
- Refresh token validity: **7 days**
- Wildcard format: `*.domain.tld` (single-level subdomain matching only)
