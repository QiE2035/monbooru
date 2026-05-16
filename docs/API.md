# Authentication and REST API

## Password login

Off by default. To enable:

```bash
# On the host:
./monbooru -hash-password 'your-password'
# Or via Docker:
docker exec -it monbooru monbooru -hash-password 'your-password'
```

The flag prints the bcrypt hash of the supplied password and exits.
Paste the generated hash into **Settings → Authentication**, or set
`auth.password_hash` in TOML and `auth.enable_password = true`. The
login rate-limits per IP with exponential backoff.

## REST API

Disabled by default. Enable it by generating a token in
**Settings → Authentication**. Then:

```bash
curl -H "Authorization: Bearer <token>" \
     "http://localhost:8080/api/v1/images/search?q=1girl"
```

- HTML reference: `/api/v1/docs` (also linked in the footer).
- OpenAPI spec: `/api/v1/openapi.json`.

The HTML reference and the OpenAPI document are the source of truth
for the wire shape. 