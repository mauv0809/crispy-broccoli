# Deployment

DeepValue runs as a single Docker container behind Coolify on a Hetzner VPS,
with a Coolify-managed Postgres on the same host.

## One-time setup (operator)

### 1. Postgres

In Coolify:
1. Project → New Resource → **PostgreSQL**, version 18.
2. Note the auto-generated credentials (username, password, database name).
3. Save.

### 2. App service

In Coolify:
1. Project → New Resource → **Public Repository** (or private with SSH key).
2. URL: `https://github.com/<org>/<repo>`.
3. Branch: `main`.
4. Build pack: **Dockerfile**.
5. Dockerfile path: `Dockerfile`.
6. Port: `8080`.
7. Healthcheck: `GET /health` expecting status 200.
8. **Link this app to the Postgres service** so Coolify injects `DATABASE_URL`
   automatically. (Alternative: copy the DSN into env vars manually.)

### 3. Environment variables

In the Coolify UI for the app service, add the variables from `.env.example`.
See that file for the canonical list and meanings.

Mark these as **secret** (toggle in the UI):
- `NASDAQ_API_KEY`
- `TIINGO_API_KEY`
- `SENTRY_DSN`
- `GOOGLE_CLIENT_SECRET` (Plan 2 — auth)
- `SESSION_KEY` (Plan 2 — auth)

Generate `SESSION_KEY` (Plan 2 dependency, but easy to set up now):
```bash
openssl rand -hex 32
```

### 4. DNS + TLS

1. Point an A record for `deepvalue.<your-domain>` at the VPS IP.
2. In Coolify, set the app's domain. Coolify auto-issues a Let's Encrypt cert.

### 5. GitHub branch protection

In GitHub repo settings → Branches → Add rule for `main`:
- ☑ Require a pull request before merging
- ☑ Require status checks to pass:
  - `lint`
  - `generated-go-fresh`
  - `generated-css-fresh`
  - `test`
  - `build`
- ☑ Require branches to be up to date before merging
- ☑ Require linear history (squash or rebase merges only)

### 6. Coolify auto-deploy

In the app service settings → Source → enable **Auto Deploy** with the webhook
secret Coolify provides. Coolify will redeploy on every push to `main`.

## Runtime

### Logs

In Coolify's UI, the app's "Logs" tab shows stdout. Lines are JSON when
`ENV=production`. Filter by `request_id`, `level`, or any other field.

### Healthcheck

`GET /health` returns:
- `200 {"status":"ok","build_sha":"…","build_time":"…"}` when DB is reachable
- `503 {"status":"unhealthy","error":"…"}` when the DB is down or pool is nil

Coolify's container healthcheck consumes this; an unhealthy container is
restarted automatically.

### Graceful shutdown

The app listens for SIGTERM (Coolify sends this on redeploy) and drains
in-flight requests with a 10s deadline before exiting.

### Error tracking

Errors flow to Sentry/GlitchTip when `SENTRY_DSN` is set. To enable later:
1. Either sign up for Sentry (cloud) or deploy GlitchTip as another Coolify
   service on the same VPS.
2. Copy the DSN into the app's `SENTRY_DSN` env var.
3. Redeploy. No code change required.

## Backups

Not configured at this time (accepted risk). Postgres data lives only on the
VPS volume. To enable later, configure Coolify's built-in S3 backup on the
Postgres service.

## Disaster recovery

If the VPS is lost:
1. Spin up a fresh VPS, install Coolify.
2. Re-create the Postgres and app services per "One-time setup" above.
3. Re-ingest fundamentals via the admin endpoints (slow, consumes API quota).
4. Strategies and portfolio rows are lost unless backups are configured.
