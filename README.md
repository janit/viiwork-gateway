# viiwork-gateway

An API-key-protected HTTPS gateway that exposes a [viiwork](https://github.com/janit/viiwork) mesh outside its tailnet, on one port, behind Caddy.

viiwork has no authentication of its own — by design, for trusted local networks. This gateway is what makes reaching it from the internet reasonable: every request needs a key, credentials never reach the mesh, and chassis power control is refused outright.

## What it does

- **One origin for the whole mesh.** Inference API and dashboards, same hostname.
- **Lands on the mesh view.** The bare hostname serves the fleet-wide `/mesh` page, not the single-node dashboard a viiwork node answers `/` with. The rewrite is upstream-only, so the browser keeps `/` in its address bar and the access log still records the path that was asked for. The per-node dashboard is not reachable through the gateway; it stays on the tailnet.
- **Discovers the fleet.** Seeds are entry points, not the world: every other node is found by polling, and a host that comes up is usable within one poll interval with no restart and no config edit.
- **Routes by model.** `/v1/chat/completions` goes straight to a node that serves the requested model, one hop instead of two.
- **Aggregates the catalogue.** `/v1/models` is the union across every discovered node, which is more complete than any single node's view.
- **Streams properly.** Token-by-token completions and all three SSE endpoints pass through unbuffered.

## Quick start

```bash
cp .env.example .env
# Edit .env: set VIIWORK_GW_SEEDS to one or more tailnet node addresses,
# and generate a key per client:
openssl rand -base64 32

docker compose up -d
```

Then point Caddy at it:

```bash
sudo cp caddy/viiwork-gateway.Caddyfile /etc/caddy/
# add `import /etc/caddy/viiwork-gateway.Caddyfile` to /etc/caddy/Caddyfile
sudo systemctl reload caddy
```

## Using it

From an OpenAI-compatible client:

```bash
curl https://gw.example.com/v1/models \
  -H "Authorization: Bearer $VIIWORK_KEY"

curl https://gw.example.com/v1/chat/completions \
  -H "Authorization: Bearer $VIIWORK_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"your-model","messages":[{"role":"user","content":"Hello"}]}'
```

From a browser, visit once with the key appended:

```
https://gw.example.com/?key=YOUR_API_KEY
```

The gateway sets a session cookie and redirects to the clean URL. The cookie is signed with that key and expires; deleting the key from `.env` invalidates it immediately.

## Configuration

All configuration is environment variables — see `.env.example`. Keys are named `VIIWORK_KEY_<label>`; the label appears in the access log, so you can see which client did what and revoke exactly one.

Keys must be at least 24 characters. The gateway refuses to start otherwise, and refuses to start with no keys at all.

## What is deliberately not exposed

`/v1/power` and `/v1/mesh/power` are refused with `403`. These are chassis power control — machine on and off, with a fallback to the BMC that reaches a host which is already powered down. Switching hardware off stays a tailnet-only capability. The power buttons on `/mesh` will error when the dashboard is reached through the gateway; that is intended.

## Security notes

- The gateway binds loopback only. Caddy is the sole way in.
- Credentials are stripped before forwarding, so no gateway key reaches a viiwork node's logs or prompt history.
- No CORS headers are emitted, and any arriving from upstream are removed. Everything is same-origin, so no third-party page can drive the fleet from a visitor's browser.
- **Prompt and response text is readable through the dashboards.** Anyone holding a key can read the fleet's prompt history. Hand out keys accordingly.
- **The browser session cookie is `Secure`, so it only works over HTTPS.** If you test `/mesh` directly against `127.0.0.1:8090` on plain HTTP — bypassing Caddy — the browser silently refuses to store or send that cookie, which shows up as an endless redirect loop or repeated `401`s that look like a broken gateway. It isn't: go through Caddy over HTTPS and it works as expected.

## Development

```bash
make test   # unit tests
make race   # with the race detector
make build  # bin/viiwork-gateway
```

`make integration` runs `go test -tags=integration -race ./...`: the `integration`-tagged end-to-end suite in `integration_test.go` (a discovered, heterogeneous fake mesh — auth, routing, aggregation, the sticky view node, chassis-power denial) plus the full unit suite, since the build tag is additive rather than a filter.

The mesh wire contract comes from `github.com/janit/viiwork/meshapi`, the package viiwork publishes for exactly this purpose. It is the only dependency. To develop against an unpublished `meshapi` change, use a gitignored `go.work` — never a `replace` directive.
