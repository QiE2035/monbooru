# Authentication and REST API

## Password login

Off by default. Enable via **Settings → Authentication** or set
`auth.password_hash` in TOML and `auth.enable_password = true`. To generate the hash :

```bash
# On the host:
./monbooru -hash-password 'your-password'
# Or via Docker:
docker exec -it monbooru monbooru -hash-password 'your-password'
```

The login rate-limits per IP with exponential backoff.

## REST API

Disabled until you create at least one API token in
**Settings → Authentication**. Tokens are named, individually revocable, and each carries a privilege set chosen in its
**Config** dialog:

- `read` - all `GET` endpoints (search, fetch, tags, ...)
- `write` - `POST`/`PATCH` (upload, edit, tags, relations, ...)
- `delete` - `DELETE` endpoints

A request needs a token whose scopes cover the method, or it gets
`403 insufficient_scope`; a missing or invalid token gets
`401 unauthorized`, and before any token exists the whole API returns
`503 api_disabled`. The secret is shown once at creation and stored
only as a hash.

```bash
curl -H "Authorization: Bearer <token>" \
     "http://localhost:8080/api/v1/images/search?q=1girl"
```

Requests target the active gallery by default; add `?gallery=<name>` or
an `X-Monbooru-Gallery` header to address another one.

- HTML reference: `/api/v1/docs` (also linked in the footer).
- OpenAPI spec: `/api/v1/openapi.json`.

The HTML reference and the OpenAPI document are the source of truth
for the wire shape.

## Pairing with monloader

To connect monloader without copying keys, open monloader and click
**connect to monbooru**; the request appears under **Settings → Monloader** for you to approve. monbooru
then issues monloader a token (`read`+`write` by default) and stores the token monloader offers for the
return direction, which drives the footer's "connected to monloader" light.
Remove the pairing on either side; re-pairing requires removing the existing
one first.