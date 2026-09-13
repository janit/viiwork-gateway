# viiwork-gateway — secure deployment checklist

The gateway's code has been adversarially reviewed and hardened. But a gateway
is only as safe as the environment it runs in. These are the controls that live
**outside** the Go code — and for the mesh-trust threats, they are the highest-
leverage protection you have. Do not skip them because "the code passed review."

## The threat model in one paragraph

The **internet side** is defended by API keys, and that boundary held under
direct attack. The **mesh side** is where residual risk concentrates, and in
viiwork 2 the gateway is itself a mesh member rather than a poller with a seed
list: it routes to whoever is a member.

What that means depends on which mesh you run.

- **A secured mesh** (`VIIWORK_MESH_SECRET`) authenticates membership. Only a
  holder of the shared secret can join, be routed to, or serve a dashboard.
  **This is what to run behind a gateway.**
- **An open mesh** (`VIIWORK_GW_MESH_OPEN=true`) admits any viiwork node that
  can reach the gossip port. Inference routing then trusts whatever joins. The
  code does not pretend otherwise: the network is the boundary, and the tailnet
  must be a real one. `VIIWORK_GW_VIEW_NODES` is required in this mode, so at
  least the browser-facing node is named rather than elected from strangers.

Either way, a node that is genuinely a member and genuinely serves a model can
receive prompts routed to that model. That is the trust model, stated plainly
rather than half-defended in code.

## Must-do before internet exposure

- [ ] **Caddy terminates TLS in front of the gateway.** The gateway binds
      loopback only — and since the container runs with host networking there
      is no port mapping enforcing that any more, so `VIIWORK_GW_LISTEN` is
      what does it. Keep it at `127.0.0.1:8090`. A value of `0.0.0.0:8090`
      under host networking puts the gateway straight onto every interface,
      including the public one, with no Caddy and no TLS in front. This is the
      single most dangerous line in the configuration. Caddy is also what makes the
      `X-Forwarded-For` in the access log trustworthy and the `Secure` session
      cookie work — without Caddy in front, both break.
- [ ] **Use the shipped Caddyfile, and check its timeouts actually applied.**
      `caddy/viiwork-gateway.Caddyfile` carries read/idle timeouts and a body
      size backstop that kill a stalled request at the proxy before it costs
      the gateway a goroutine, an FD and a concurrency token. They live in a
      **global options block, which Caddy requires to be first in the
      Caddyfile** — an `import` placed after an existing site block fails with
      "server block without any key is global configuration, and if used, it
      must be first". Confirm they landed rather than assuming:

      ```
      caddy validate --config /etc/caddy/Caddyfile
      caddy adapt --config /etc/caddy/Caddyfile | grep -E 'read_timeout|idle_timeout'
      ```

      Never add a `write` timeout: responses here are token streams and SSE
      that run as long as a generation takes, and a write deadline truncates
      real inference mid-answer. (Same reason the gateway sets
      `WriteTimeout: 0`.)

      The shipped Caddyfile also sends `Strict-Transport-Security`
      (`max-age=31536000; includeSubDomains`). The gateway is HTTPS-only —
      its session cookie is `Secure` — and HSTS is what closes the
      first-visit downgrade gap the HTTP->HTTPS redirect leaves open. It is
      deliberately not `preload`d: preload is a domain-wide, hard-to-reverse
      commitment, not one service's to make. Confirm the header actually
      lands (`curl -sI https://<host>/ | grep -i strict-transport`); if you
      merged this Caddyfile's directives into an existing site block rather
      than importing it whole, it is easy to drop.
- [ ] **Generate one strong key per client.** `openssl rand -base64 32`, one
      `VIIWORK_KEY_<label>` per consumer. The label lands in the access log, so
      you can attribute and revoke per client. Minimum length is enforced (24
      chars), but longer is free.
- [ ] **`.env` is never committed and is not readable by other users.**
      `chmod 600 .env`. It holds every key to the fleet — and, in a secured
      mesh, `VIIWORK_MESH_SECRET` as well. That secret is not another API key:
      it is the credential that lets a process *join the mesh* as a member.
      Leaking it is worse than leaking an API key, because a member can be
      routed inference and can serve dashboards, not merely call them.
- [ ] **Do NOT run `docker compose up` (which builds) with a live `.env`
      present unless `.dockerignore` excludes it** — it does, but confirm
      `.dockerignore` lists `.env` before the first build on a host, so keys
      never enter the Docker build cache.

## Tailnet ACLs — the highest-leverage control

Membership is what the gateway trusts, so who can reach the gossip port is the
control that matters. In your tailnet policy:

- [ ] **Open 7946 tcp *and* udp between the gateway host and the fleet nodes**,
      and nothing else. Memberlist needs both. Gossip binds only the advertise
      address, so it is never on a public interface — but an ACL is what stops
      another tailnet device from joining.
- [ ] **Let the gateway host reach nodes on 8086** (their API) **and 7946**
      (gossip). Those two are the whole outbound requirement.
- [ ] **Prefer a secured mesh.** A shared secret authenticates membership, which
      is a stronger guarantee than any address rule, and it is the difference
      between "the network is the boundary" and "the secret is".
- [ ] **Name the view nodes.** `VIIWORK_GW_VIEW_NODES` is the allowlist for the
      nodes whose HTML runs at the gateway's cookie-authenticated origin. It is
      required in an open mesh and worth setting in a secured one.
- [ ] **Keep BMC/IPMI on a separate network the gateway cannot reach.** The
      SSRF fix blocks the gateway from dialing link-local/loopback/private, but
      network isolation of the IPMI plane is the real guarantee that chassis
      power stays off the API surface.

## Configuration knobs added by the hardening

- `VIIWORK_GW_MAX_INFLIGHT` (default 256) — concurrent proxied requests. Worst-
  case memory ≈ this × `VIIWORK_GW_MAX_BODY`. Lower it if the host has little
  RAM (256 × 16MB ≈ 4GB worst case).

  One budget covers everything the gateway forwards, and that includes the
  dashboards: `/` and `/v1/mesh/stream` hold a token for the whole life of
  their (often long-lived, SSE) connection, released when the browser
  disconnects. So open dashboard tabs and inference draw from the same 256.
  For a handful of clients this is invisibly generous; if you expect many
  simultaneous dashboard viewers, or you lower this for a small host, size it
  with both in mind. A key holder can also hold connections open
  deliberately — but a key holder is already trusted to read all traffic, so
  this changes nothing about the trust model, only the capacity arithmetic.
- `VIIWORK_GW_BODY_READ_TIMEOUT` (default 30s) — how long a request body may
  take to arrive before a `408`. Long enough for a real prompt on a slow link;
  short enough to stop a slow-body hang. Does not affect long generations.
- `VIIWORK_GW_RATE_PER_MIN` (default 120) — sustained requests per minute
  **per API key**, on the inference path. This is what stops one key from
  driving the whole fleet: `MAX_INFLIGHT` bounds how many requests run at
  once, but on its own leaves a single key free to issue unlimited sequential
  ones. Set it from your busiest legitimate client's real rate, with headroom;
  the access log's `key=` label tells you who is actually hitting it.
  `0` switches rate limiting off entirely.
- `VIIWORK_GW_RATE_BURST` (default 240) — token-bucket capacity: the most a
  key may spend at once after an idle spell. Keep it comfortably above the
  per-second rate or normal bursty clients (an SDK firing several parallel
  calls) will trip the limit. A rejected request gets `429` in the OpenAI
  error shape with `Retry-After`, and is logged as `rate limit exceeded` with
  the key's label.

  Not metered, by design: the dashboard paths (a page load fires many
  requests, and metering them on the same budget lets an operator lock their
  own browser out — they stay bounded by `MAX_INFLIGHT` and the body
  deadline), and `/v1/models`, which is forwarded to the view node. It does
  take an in-flight token, since it is a real upstream request now rather than
  an answer the gateway assembles.
- `VIIWORK_GW_CAPACITY_POLL` (default 1s) — how often each member's free-slot
  report is read. This is the resolution of the routing decision: a burst
  arriving inside one interval is spread by local reservations rather than by
  fresher numbers.
- `VIIWORK_GW_STALE_AFTER` (default 3s) — how old a report may be before its
  node stops being a routing candidate. Keep it comfortably above the poll
  interval; too close and a single slow poll takes a healthy node out of
  rotation.
- `VIIWORK_GW_VIEW_NODES` (default empty) — the nodes allowed to serve the
  dashboards, `/v1/models` and `GET /v1/aliases`, in order of preference.
  **Required in an open mesh.** Empty in a secured mesh means any member,
  chosen stickily so a live SSE stream is not moved.

## What is still NOT defended in code (know these)

- **A member can still capture prompts for a model it legitimately serves.**
  Routing trusts membership. In a secured mesh that means a secret holder; in
  an open mesh it means anything that reached the gossip port. Mitigation: run
  a secured mesh, and keep the ACLs above.
- **Anyone with any valid key can read the fleet's prompt/response history**
  via the dashboards. This is inherent to exposing viiwork's surface. Hand out
  keys accordingly; treat every key holder as able to read all traffic. Note
  that per-key rate limiting does NOT contain this: the dashboard paths are
  deliberately unmetered, so the limit bounds how hard a key can drive
  *inference*, not how much history it can read.
- **An open mesh admits anything that can reach the gossip port.** There is no
  cap to exhaust and no lockout to trigger — memberlist membership replaced the
  v1 discovery list — but equally there is no authentication. A secured mesh
  admits only secret holders, which is the answer to this.
- **`clientip` trusts the right-most `X-Forwarded-For`**, which is correct only
  with Caddy in front. Do not expose the gateway without it.

## Operational

- [ ] Ship the JSON access log somewhere you'll actually read it. Watch for a
      key label doing far more than expected, for `authentication rejected`
      bursts (a brute-force attempt — now logged with source IP and reason),
      and for `status=429` lines (rate limiting; there is no separate warning
      line, deliberately — a key hammering its limit would double its own log
      volume). A steady trickle of 429s from one `key=` label is either a
      client that needs a higher limit or a key being abused; the label tells
      you which one to go ask.
- [ ] Consider fail2ban (or equivalent) on `authentication rejected` lines.
      Unauthenticated brute force is the one thing better dropped before it
      reaches the gateway at all, and the log already carries the source IP.
- [ ] Have a revocation drill: deleting a `VIIWORK_KEY_<label>` line and
      restarting invalidates that key and every cookie minted from it.

- **A graceful stop leaves the mesh.** `docker compose stop` (or any SIGTERM)
  drains in-flight HTTP for up to 15s and only then leaves, so the gateway
  disappears from every member at once instead of being declared dead a
  failure-detector interval later. Draining first is deliberate: a request
  still in flight may yet be forwarded to a node.
- **Host networking is required.** Gossip must be reachable on this host's
  tailnet address, which a bridged container cannot advertise. The API still
  binds loopback only, so Caddy remains the sole way in.
