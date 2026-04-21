# Hysteria Tuning & Client Knobs

Practical settings for the Kotodian fork. Covers server-side kernel tuning
and the three client-side toggles people most often ask about: `fastOpen`,
`udp`, and `block-quic`.

## Server: UDP socket buffers (sysctl)

quic-go wants at least ~7.5 MiB of SO_RCVBUF/SO_SNDBUF per UDP socket.
Hitting that ceiling produces the well-known log line:

```
failed to sufficiently increase receive buffer size
```

The fork's Debian package ships `/etc/sysctl.d/99-hysteria.conf`:

```
net.core.rmem_max = 16777216
net.core.wmem_max = 16777216
```

This is the value recommended by upstream Hysteria. Apply out-of-band
(e.g. on non-Debian hosts) with:

```sh
sudo tee /etc/sysctl.d/99-hysteria.conf >/dev/null <<'EOF'
net.core.rmem_max = 16777216
net.core.wmem_max = 16777216
EOF
sudo sysctl --system
```

Memory cost is bounded by `rmem_max` × number of concurrently active QUIC
sockets. On a 2 GiB VPS this is safe for test and single-tenant production
workloads.

## Client: `fastOpen: true`

Hysteria's `fastOpen` is a **protocol-level** optimisation, not kernel TFO.
When enabled, the client returns a newly opened proxy stream to the caller
**before** it has read the server's `TCPResponse` frame. Caller payload
(TLS ClientHello, HTTP request bytes, etc.) can be packed into the same
QUIC packet as the address frame, saving one client↔server RTT per new
TCP target.

- Source: `core/client/client.go` (the `if c.config.FastOpen` branch after
  `protocol.WriteTCPRequest`).
- Savings: ~1 RTT per new TCP stream. Noticeable on any real-world link
  (100 ms+ RTT) and cumulative when a page opens dozens of connections.
- Cost: connection errors become deferred. The local app believes it
  connected; the actual failure surfaces on the first `Read()` call.
  Modern browsers, `curl`, and mainstream apps handle this correctly.

**Recommendation: enable it.** The only reason to leave it off is if
something in your pipeline interprets "connect succeeded" as a liveness
probe and never reads.

## Client: `udp: true` (a.k.a. `disableUDP: false`)

Hysteria does not expose a `udp` key; instead it has the inverse toggle
`disableUDP` (default `false`). Upstream wrapper apps (sing-box, NekoBox,
Karing, Clash.Meta) often surface a positive `udp: true` flag on their
Hysteria outbound — the translation is `udp=true` ≡ `disableUDP=false`.

UDP proxying runs over QUIC unreliable datagrams (`core/server/server.go`
`udpIOImpl.ReceiveMessage`). Disabling it turns Hysteria into a
TCP-only proxy: DNS, WebRTC, online games, QUIC-native apps all break.

**Recommendation: keep UDP enabled** unless you have a specific reason
not to (e.g. you terminate UDP elsewhere and want Hysteria to handle TCP
only).

## Client-side App: `block-quic: true`

This is **not** a Hysteria config key. It lives in the upstream wrapper
app's route rules (sing-box, NekoBox, Karing, Clash.Meta, etc.). The
rule drops local `UDP port 443` traffic so browsers give up on HTTP/3
and fall back to HTTPS (TCP 443).

### Why you want it

Browsers default to HTTP/3 (QUIC over UDP 443). Without blocking,
browser traffic looks like:

```
HTTP/3 (QUIC) over UDP
   └── tunnelled into Hysteria datagrams
         └── Hysteria's own QUIC handshake + stream
               └── raw UDP on the wire
```

That is QUIC-in-QUIC. Concretely:

- Extra CPU for double encryption.
- Two congestion controllers fighting each other (inner HTTP/3 BBR vs.
  outer Hysteria Brutal).
- 5–15 % bandwidth loss in practice.
- CDNs sometimes flag it (unusual UDP flow characteristics).

`block-quic` in the wrapper app forces the browser to use HTTPS/TCP,
which Hysteria carries over a single QUIC stream — no double wrapping.

### When to leave it off

- Testing or debugging HTTP/3 masquerade.
- Benchmarking raw QUIC-in-QUIC for research.
- Apps that rely on UDP-443 to endpoints you deliberately want tunneled
  as QUIC (rare outside HTTP/3).

Non-443 UDP (DNS, WireGuard-in-app, game traffic, WebRTC) is unaffected
by this rule.

## Quick reference

| Knob | Where it lives | Default | Typical value |
| --- | --- | --- | --- |
| `net.core.rmem_max` / `wmem_max` | server kernel sysctl | distro default (often 2 MiB) | 16 MiB |
| `fastOpen` | Hysteria client YAML | `false` | `true` |
| `disableUDP` | Hysteria client & server YAML | `false` | leave `false` |
| `block-quic` (rule) | Wrapper app route rules | off | on for browser-heavy clients |
