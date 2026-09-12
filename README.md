# viiwork-gateway

An API-key-protected HTTPS gateway that exposes a [viiwork 2](https://github.com/janit/viiwork) mesh outside its tailnet, on one port, behind Caddy.

A viiwork node's HTTP API has no authentication of its own — by design, for trusted local networks. (viiwork 2 can authenticate *membership* with a shared mesh secret, which is a different thing: it decides who may join the mesh, not who may call a node's API. Anyone who can reach the port can still call it.) This gateway is what makes reaching the mesh from the internet reasonable: every request needs a key, credentials never reach the nodes, and chassis power control and alias writes are refused outright.

## What it does

- **One origin for the whole mesh.** Inference API and dashboards, same hostname.
- **Lands on the mesh view.** The bare hostname serves the fleet-wide `/mesh` page, not the single-node dashboard a viiwork node answers `/` with. The rewrite is upstream-only, so the browser keeps `/` in its address bar and the access log still records the path that was asked for. The per-node dashboard is not reachable through the gateway; it stays on the tailnet.
- **Joins the mesh.** The gateway is a member that serves no models. It has no node list to maintain: membership is the fleet, so a host that comes up is usable within a capacity poll — seconds, with no restart and no config edit. On a tailnet it needs no seeds at all.
- **Routes by free slots.** `/v1/chat/completions` goes in one hop to the alive node with the most free slots for the requested model, counting this gateway's own in-flight forwards so a burst spreads instead of piling onto one reported slot. From there the node queues and spills with knowledge the gateway does not have.
- **The catalogue comes from the mesh.** `/v1/models` is answered by a node, whose own catalogue is already the union across alive members and includes mesh-wide aliases.
- **Streams properly.** Token-by-token completions and all three SSE endpoints pass through unbuffered.

## Quick start

```bash
cp .env.example .env
# Edit .env: set VIIWORK_MESH_SECRET to the fleet's mesh secret, and
# generate a key per client:
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

All configuration is environment variables — every one of them is documented in `.env.example`.

Two rules the gateway refuses to start without:

- **A mesh is secured or open, and you say which.** Either `VIIWORK_MESH_SECRET` (32 bytes, base64, matching the nodes) or `VIIWORK_GW_MESH_OPEN=true` — never both, never neither. A secured mesh is what to run behind a gateway: membership is authenticated, so only secret holders can join and be routed to.
- **An open mesh must name its view nodes.** `VIIWORK_GW_VIEW_NODES` lists the nodes allowed to serve the dashboards. Their HTML runs at this gateway's authenticated origin, and in an open mesh any viiwork node that can reach the gossip port is a member.

Keys are named `VIIWORK_KEY_<label>`; the label appears in the access log, so you can see which client did what and revoke exactly one. Keys must be at least 24 characters, and the gateway refuses to start with none.

The five v1 discovery variables are startup errors now rather than ignored settings, each naming its replacement — `VIIWORK_GW_SEEDS` above all, which held API addresses and would fail quietly if reused as gossip seeds.

## What is deliberately not exposed

**Alias writes.** Any method but `GET` or `HEAD` on `/v1/aliases` or below is refused with `403`. The alias table is mesh-wide state: a write repoints every node's traffic for a model name, and that should not be reachable from the internet with an API key. Reading it is fine, and the dashboards do. Use `viiwork alias` on the tailnet.

**Chassis power.** `/v1/power` and `/v1/mesh/power` are refused with `403`. These are chassis power control — machine on and off, with a fallback to the BMC that reaches a host which is already powered down. Switching hardware off stays a tailnet-only capability. The power buttons on `/mesh` will error when the dashboard is reached through the gateway; that is intended.

## Security notes

- The gateway binds loopback only, and `VIIWORK_GW_LISTEN` is what enforces that. The container runs with host networking — mesh gossip has to be reachable on the host's tailnet address — so there is no published-port mapping acting as a safety net any more. Setting it to `0.0.0.0:8090` would put the gateway on every interface, public one included, with no TLS in front. Caddy is the sole way in.
- **`.env` now holds a membership credential, not just API keys.** In a secured mesh `VIIWORK_MESH_SECRET` is what lets a process join the mesh as a member — able to be routed inference and to serve dashboards, not merely to call the API. `chmod 600` it and treat it as the most valuable file on the host.
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

`make integration` runs `go test -tags=integration -race ./...`: the end-to-end suite in `integration_test.go` and the membership suite in `internal/fleet`, plus the full unit suite, since the build tag is additive rather than a filter. Both run on viiwork's `mesh/meshtest` in-process network, so a test can take a node away and watch the gateway route around it. No test touches a real network or a real fleet.

The gateway builds on viiwork 2's public packages: `meshapi` for the wire contract, `mesh` for membership, and `mesh/capacity` for the free-slot reports — the same code the nodes run, rather than a gateway-only copy of it. They bring hashicorp/memberlist and mdns, which is most of the module list.

To develop against an unpublished viiwork 2 change, put a checkout beside this one and use a gitignored `go.work` — never a `replace` directive.
