# Cloudflare in front of the fallback path

Some networks — hotel and conference WiFi, corporate guest networks — drop UDP
or allow only web traffic. On those uplinks the bond falls back to a WebSocket
path. Publishing the aggregator's TCP port through a Cloudflare Tunnel makes
that path look like ordinary HTTPS on port 443 to a Cloudflare address, which
gets through almost anything.

What Cloudflare can and cannot do here:

- **Can:** carry the WebSocket fallback (`/v1/ws`). WebSockets work on every
  plan, including free.
- **Cannot:** carry the bonded UDP traffic itself. Proxying arbitrary UDP
  (Spectrum) is an Enterprise product, and Zero Trust private networking needs
  Cloudflare's own client on every device. The aggregator stays a VM with a
  public address and UDP port 7777 open; Cloudflare is the way in when UDP is
  not available, not a replacement for it.

## Setup

On the aggregator:

```bash
cloudflared tunnel login
```

```bash
cloudflared tunnel create bond
```

```bash
cloudflared tunnel route dns bond bond.example.com
```

Copy `config.example.yml` to `/etc/cloudflared/config.yml`, fill in the tunnel
id and hostname, and start it:

```bash
sudo cloudflared service install
```

Tell the aggregator to hand that URL to devices, by adding `--ws-url` to its
flags (with a service: `sudo bondd service install server --public <address> --ws-url wss://bond.example.com/v1/ws`):

```bash
bondd server --public 203.0.113.5 --ws-url wss://bond.example.com/v1/ws
```

Devices learn the URL when they join. Ones that joined before the change keep
the old URL until they join again (`bond join <code>` — they keep their
identity and address).

## Checking it

On a hub, force the fallback and look at the paths:

```bash
bondd hub --transport ws
```

```bash
bond status
```

Each uplink should show a `ws:<name>` path that is up. With the default
`--transport auto` a `ws:` path appears by itself about eight seconds after an
uplink's UDP path stops answering, and goes away once UDP has been back for
fifteen.

A WebSocket path is TCP underneath, so one lost packet stalls everything
behind it. The scheduler measures that like any other path and leans on it
only as much as it deserves — expect it to keep you online, not to be fast.
