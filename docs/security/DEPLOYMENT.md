# viiwork-gateway — secure deployment checklist

The gateway's code has been adversarially reviewed and hardened. But a gateway
is only as safe as the environment it runs in. These are the controls that live
**outside** the Go code — and for the mesh-trust threats, they are the highest-
leverage protection you have. Do not skip them because "the code passed review."

## The threat model in one paragraph

The **internet side** is defended by API keys, and that boundary held under
direct attack. The **tailnet side** is where residual risk concentrates: the
gateway trusts the viiwork nodes it polls. The hardening shrinks the blast
radius of a rogue node (it can't SSRF the gateway, can't become the browser-
facing view node, can't forge itself into the sole route, can't OOM the poller),
but it does **not** authenticate nodes. So a node that is genuinely on your
tailnet and genuinely serves a model can still receive prompts routed to that
model. The tailnet must therefore remain a real trust boundary. That is what
these controls enforce.

## Must-do before internet exposure

- [ ] **Caddy terminates TLS in front of the gateway.** The gateway binds
      loopback only (`127.0.0.1:8090` via the compose port mapping). Never bind
      it to a public interface directly. Caddy is also what makes the
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
- [ ] **Generate one strong key per client.** `openssl rand -base64 32`, one
      `VIIWORK_KEY_<label>` per consumer. The label lands in the access log, so
      you can attribute and revoke per client. Minimum length is enforced (24
      chars), but longer is free.
- [ ] **`.env` is never committed and is not readable by other users.**
      `chmod 600 .env`. It holds every key to the fleet.
- [ ] **Do NOT run `docker compose up` (which builds) with a live `.env`
      present unless `.dockerignore` excludes it** — it does, but confirm
      `.dockerignore` lists `.env` before the first build on a host, so keys
      never enter the Docker build cache.

## Tailnet ACLs — the highest-leverage control

The mesh-trust findings (SSRF, prompt capture, view capture) all require an
attacker to control a node the gateway polls. Tailscale ACLs are what keep that
from happening. In your tailnet policy:

- [ ] **Restrict who can reach the gateway host** on the mesh port to only the
      real viiwork nodes. A random tailnet device should not be able to answer
      `/v1/status` on an address a real node advertises.
- [ ] **Restrict what the gateway host may dial outbound.** The gateway only
      needs to reach viiwork nodes on their API port. If your tailnet/host
      firewall can limit the gateway's egress to just those nodes, do it — it
      is defense-in-depth behind the code's peer allow-list.
- [ ] **Keep BMC/IPMI on a separate network the gateway cannot reach.** The
      SSRF fix blocks the gateway from dialing link-local/loopback/private, but
      network isolation of the IPMI plane is the real guarantee that chassis
      power stays off the API surface.
- [ ] **Seed only nodes you control.** `VIIWORK_GW_SEEDS` and any
      `VIIWORK_GW_VIEW_PREFER` node are trusted more than discovered nodes
      (seeds are exempt from peer validation; only seeds can be the view node).
      Point them at hosts you administer.

## Configuration knobs added by the hardening

- `VIIWORK_GW_MAX_INFLIGHT` (default 256) — concurrent proxied requests. Worst-
  case memory ≈ this × `VIIWORK_GW_MAX_BODY`. Lower it if the host has little
  RAM (256 × 16MB ≈ 4GB worst case).
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

  Not metered, by design: the sticky/dashboard paths (a page load fires many
  requests, and metering them on the same budget lets an operator lock their
  own browser out — they stay bounded by `MAX_INFLIGHT` and the body
  deadline), and `/v1/models`, which is answered locally from the registry.
- `VIIWORK_GW_ALLOW_PRIVATE_PEERS` (default false) — set true ONLY if your mesh
  peers over plain RFC1918 LAN instead of (or in addition to) tailscale. The
  tailnet range `100.64.0.0/10` is always allowed regardless.

## What is still NOT defended in code (know these)

- **A compromised tailnet node can still capture prompts for a model it
  legitimately serves.** Routing trusts that a node advertising a model serves
  it. Mitigation: tailnet ACLs (above); node authentication is a deferred
  design change.
- **Anyone with any valid key can read the fleet's prompt/response history**
  via the dashboards. This is inherent to exposing viiwork's surface. Hand out
  keys accordingly; treat every key holder as able to read all traffic. Note
  that per-key rate limiting does NOT contain this: the dashboard paths are
  deliberately unmetered, so the limit bounds how hard a key can drive
  *inference*, not how much history it can read.
- **Discovery lockout (deferred).** A rogue node can flood the discovery cap
  (500) so new legitimate nodes aren't discovered until restart. Bounded and
  logged; not yet self-healing.
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
