# Raspberry Pi → XRouter WiFi Hotspot (transparent Xray proxy router)

Turn a Raspberry Pi (running the `xray-manager` container) into a WiFi access point
that transparently routes **all client traffic through the Xray socks5 proxy**.

- **SSID:** `XRouter`  •  **Password:** `<your-wifi-password>` (8–63 characters; set in `hostapd.conf`)
- **Uplink:** `eth0` (DHCP, untouched)  •  **AP:** `wlan0` = `10.42.0.1/24`
- **Proxy target:** socks5 `127.0.0.1:10808` (exposed by the `xray-manager` Docker container)

> Verified on a Raspberry Pi 5, Ubuntu Server 24.04 (Noble), kernel 6.8 `raspi`,
> onboard Broadcom WiFi (`brcmfmac`). Network stack: **systemd-networkd / netplan**
> (NetworkManager not used).

---

## 1. How it works

```
                      ┌─────────────────────── Raspberry Pi ───────────────────────┐
 WiFi client          │  wlan0 = 10.42.0.1/24 (hostapd AP + dnsmasq DHCP/DNS)       │
 (10.42.0.50-200) ───►│                                                             │   eth0
                      │  • TCP   ─iptables REDIRECT─► redsocks :12345 ─► socks5 ─┐   │  (uplink)
                      │  • DNS   ─iptables REDIRECT─► dnsmasq :53 ─► dns-over-   │   │ ──────►
                      │                                socks :5354 ─► socks5 ───┤   │  internet
                      │  • other UDP / QUIC / IPv6 fwd ─► DROP (no leak)        │   │
                      │                                                         ▼   │
                      │                              127.0.0.1:10808  ►  Xray (Docker) ──┘
                      └─────────────────────────────────────────────────────────────┘
```

Why this shape:

- **redsocks** bridges plain TCP → socks5 (clients don't speak socks).
- **DNS is tunneled** through the proxy via a tiny DNS‑over‑socks forwarder, because
  the container maps the socks port **TCP-only** (so socks UDP / DNS-over-UDP can't traverse it).
- **Fail-closed:** non-tunnelable traffic (other UDP, QUIC on UDP/443, IPv6 forwarding)
  is dropped. Clients have internet **only while xray-core is egressing** — no plaintext leak path.
- `eth0` is never reconfigured, so remote SSH stays up during setup.

---

## 2. Components

| Service | Role | Key file |
|---|---|---|
| `hostapd` | WiFi access point on `wlan0` | `/etc/hostapd/hostapd.conf`, `/etc/default/hostapd` |
| `systemd-networkd` | static IP `10.42.0.1/24` on `wlan0` | `/etc/systemd/network/10-wlan0-ap.network` |
| `dnsmasq` | DHCP + DNS for clients | `/etc/dnsmasq.d/xrouter.conf`, `xrouter-static.conf` |
| `dns-over-socks` | tunnels DNS as DNS-over-TCP through socks5 | `/usr/local/sbin/dns-over-socks.py` + unit |
| `redsocks` | transparent TCP → socks5 | `/etc/redsocks.conf` + `LimitNOFILE` drop-in |
| `xrouter-firewall` | iptables redirect + leak prevention | `/usr/local/sbin/xrouter-firewall.sh` + unit |
| `wg-game` (networkd) | WireGuard game tunnel for the Xbox (section 9) | `/etc/systemd/network/30-wg-game.{netdev,network}` |

All six services are enabled at boot. `wpa_supplicant` is **disabled** (it was holding `wlan0`).

---

## 3. Prerequisites

- Pi reachable over SSH on `eth0` with sudo.
- `xray-manager` container running and publishing socks5 on `127.0.0.1:10808`
  (web UI on `:8080`). The proxy must be **egressing** (start/select a working
  server in the UI) for clients to get internet.
- Packages:

```bash
sudo apt-get update
sudo apt-get install -y hostapd dnsmasq redsocks iw iptables-persistent
```

---

## 4. Configuration files

### `/etc/hostapd/hostapd.conf`
```ini
interface=wlan0
driver=nl80211
ssid=XRouter
country_code=IR
ieee80211d=1
hw_mode=g
channel=6
ieee80211n=1
wmm_enabled=1
auth_algs=1
macaddr_acl=0
ignore_broadcast_ssid=0
wpa=2
wpa_key_mgmt=WPA-PSK
rsn_pairwise=CCMP
wpa_passphrase=<your-wifi-password>
```
`/etc/default/hostapd`:
```ini
DAEMON_CONF="/etc/hostapd/hostapd.conf"
```

### `/etc/systemd/network/10-wlan0-ap.network`
```ini
[Match]
Name=wlan0

[Link]
RequiredForOnline=no

[Network]
Address=10.42.0.1/24
ConfigureWithoutCarrier=yes
LinkLocalAddressing=no
IPv6AcceptRA=no
DHCP=no
```

### `/etc/dnsmasq.d/xrouter.conf`
```ini
interface=wlan0
bind-dynamic
listen-address=10.42.0.1
no-resolv
no-hosts
# upstream = local DNS-over-SOCKS5 forwarder (tunnels DNS through Xray)
server=127.0.0.1#5354
dhcp-range=10.42.0.50,10.42.0.200,255.255.255.0,12h
dhcp-option=option:router,10.42.0.1
dhcp-option=option:dns-server,10.42.0.1
dhcp-authoritative
domain-needed
bogus-priv
cache-size=1000
```
> `dnsmasq`'s `conf-dir` must be enabled so this drop-in is read:
> ```bash
> sudo sed -i 's|^#conf-dir=/etc/dnsmasq.d/,\*.conf|conf-dir=/etc/dnsmasq.d/,*.conf|' /etc/dnsmasq.conf
> ```

### `/usr/local/sbin/dns-over-socks.py`
```python
#!/usr/bin/env python3
"""Minimal DNS-over-SOCKS5 forwarder for XRouter.

Listens for plain UDP DNS queries from dnsmasq on 127.0.0.1:5354 and forwards
each query as DNS-over-TCP through the local SOCKS5 proxy (Xray, 127.0.0.1:10808)
to an upstream resolver. Tunnels client DNS through the proxy without needing
UDP support on the SOCKS port (Docker maps it TCP-only). Pure stdlib.
"""
import socket
import struct
import threading

LISTEN = ("127.0.0.1", 5354)
SOCKS = ("127.0.0.1", 10808)
UPSTREAM = ("1.1.1.1", 53)
TIMEOUT = 8


def socks5_connect(dst_ip, dst_port):
    s = socket.create_connection(SOCKS, timeout=TIMEOUT)
    s.settimeout(TIMEOUT)
    s.sendall(b"\x05\x01\x00")  # VER=5, 1 method, NO AUTH
    if s.recv(2) != b"\x05\x00":
        s.close()
        raise OSError("socks5 method negotiation failed")
    req = b"\x05\x01\x00\x01" + socket.inet_aton(dst_ip) + struct.pack(">H", dst_port)
    s.sendall(req)
    rep = s.recv(10)
    if len(rep) < 2 or rep[1] != 0x00:
        s.close()
        raise OSError("socks5 connect failed")
    return s


def recv_exact(s, n):
    buf = b""
    while len(buf) < n:
        chunk = s.recv(n - len(buf))
        if not chunk:
            break
        buf += chunk
    return buf


def handle(data, addr, srv):
    try:
        s = socks5_connect(*UPSTREAM)
        try:
            s.sendall(struct.pack(">H", len(data)) + data)  # DNS-over-TCP length prefix
            hdr = recv_exact(s, 2)
            if len(hdr) < 2:
                return
            n = struct.unpack(">H", hdr)[0]
            resp = recv_exact(s, n)
            if resp:
                srv.sendto(resp, addr)
        finally:
            s.close()
    except Exception:
        pass  # drop on failure; client retries / times out


def main():
    srv = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
    srv.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
    srv.bind(LISTEN)
    while True:
        data, addr = srv.recvfrom(4096)
        threading.Thread(target=handle, args=(data, addr, srv), daemon=True).start()


if __name__ == "__main__":
    main()
```
`/etc/systemd/system/dns-over-socks.service`:
```ini
[Unit]
Description=DNS over SOCKS5 forwarder (XRouter)
After=network.target

[Service]
ExecStart=/usr/bin/python3 /usr/local/sbin/dns-over-socks.py
Restart=always
RestartSec=2
User=nobody
NoNewPrivileges=yes

[Install]
WantedBy=multi-user.target
```

### `/etc/redsocks.conf`
```ini
base {
	log_debug = off;
	log_info = on;
	log = "syslog:daemon";
	daemon = on;
	user = redsocks;
	group = redsocks;
	redirector = iptables;
}

redsocks {
	local_ip = 0.0.0.0;
	local_port = 12345;
	ip = 127.0.0.1;
	port = 10808;
	type = socks5;
}
```
`/etc/systemd/system/redsocks.service.d/limits.conf` — **required**; the default 1024 fd
limit causes `accept: backing off` (stalls) under a busy client:
```ini
[Service]
LimitNOFILE=65536
```
> To use the **http proxy** instead of socks5: set `port = 10809` and
> `type = http-connect` here, then `sudo systemctl restart redsocks`.

### `/usr/local/sbin/xrouter-firewall.sh`
```bash
#!/bin/bash
# XRouter: redirect hotspot (wlan0) traffic into the Xray socks5 proxy.
#   - client TCP   -> redsocks (127.0.0.1:12345) -> socks5 127.0.0.1:10808
#   - client DNS   -> dnsmasq (10.42.0.1:53) -> dns-over-socks -> socks5
#   - other UDP    -> dropped (no leak; forces TCP fallback, e.g. QUIC->TLS)
#   - game clients -> all traffic (TCP/UDP/ICMP) via WireGuard wg-game to the
#                     Iran-Mullvad node (policy routing is set by systemd-networkd);
#                     their DNS still goes to dnsmasq
# Idempotent: safe to re-run.
set -uo pipefail
WIFI=wlan0
RP=12345
GAME_IF=wg-game
GAME_CLIENTS="10.42.0.124"   # Xbox (static lease in /etc/dnsmasq.d/xrouter-static.conf)

# Safety net: make sure the AP gateway IP is present (boot race with networkd)
ip addr show "$WIFI" 2>/dev/null | grep -q "10.42.0.1/24" \
  || ip addr replace 10.42.0.1/24 dev "$WIFI" 2>/dev/null || true

# Enable IPv4 forwarding
sysctl -qw net.ipv4.ip_forward=1

# ---- NAT: XROUTER chain (skip local/reserved nets, redirect rest of TCP) ----
iptables -t nat -N XROUTER 2>/dev/null || true
iptables -t nat -F XROUTER
for net in 0.0.0.0/8 10.0.0.0/8 100.64.0.0/10 127.0.0.0/8 169.254.0.0/16 \
           172.16.0.0/12 192.168.0.0/16 224.0.0.0/4 240.0.0.0/4; do
  iptables -t nat -A XROUTER -d "$net" -j RETURN
done
iptables -t nat -A XROUTER -p tcp -j REDIRECT --to-ports "$RP"
# Game clients' TCP is not redirected into redsocks; it is routed via wg-game.
for ip in $GAME_CLIENTS; do
  iptables -t nat -I XROUTER 1 -s "$ip" -j RETURN
done

# ---- PREROUTING hooks for wlan0 (idempotent) ----
# DNS (force every client DNS query through our resolver -> tunnel)
iptables -t nat -C PREROUTING -i "$WIFI" -p udp --dport 53 -j REDIRECT --to-ports 53 2>/dev/null \
  || iptables -t nat -A PREROUTING -i "$WIFI" -p udp --dport 53 -j REDIRECT --to-ports 53
iptables -t nat -C PREROUTING -i "$WIFI" -p tcp --dport 53 -j REDIRECT --to-ports 53 2>/dev/null \
  || iptables -t nat -A PREROUTING -i "$WIFI" -p tcp --dport 53 -j REDIRECT --to-ports 53
# All other TCP -> redsocks chain
iptables -t nat -C PREROUTING -i "$WIFI" -p tcp -j XROUTER 2>/dev/null \
  || iptables -t nat -A PREROUTING -i "$WIFI" -p tcp -j XROUTER

# ---- FORWARD: allow clients to reach the Docker-published web panel, drop rest ----
# Published ports (e.g. web panel :8080) are DNAT'd to the container, so they
# traverse FORWARD (in=wlan0, out=docker bridge) instead of INPUT. We must allow
# that flow + its established return, then drop all other wlan0 forwarding so
# there is no direct/UDP egress leak. Rebuilt deterministically each run; we only
# touch our own rules, never Docker's.
PANEL_PORT=8080
DOCKER_SUBNETS="$(ip -o -4 addr show | awk '$2 ~ /^(docker0|br-)/ {print $4}')"

# Remove our previous rules (idempotent rebuild)
iptables -D FORWARD -o "$WIFI" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT 2>/dev/null || true
for sub in $DOCKER_SUBNETS; do
  iptables -D FORWARD -i "$WIFI" -p tcp -d "$sub" --dport "$PANEL_PORT" -j ACCEPT 2>/dev/null || true
done
for ip in $GAME_CLIENTS; do
  iptables -D FORWARD -i "$WIFI" -o "$GAME_IF" -s "$ip" -j ACCEPT 2>/dev/null || true
  iptables -D FORWARD -i "$GAME_IF" -o "$WIFI" -d "$ip" -j ACCEPT 2>/dev/null || true
done
iptables -D FORWARD -i "$WIFI" -j DROP 2>/dev/null || true

# Re-insert at the top in reverse order so final order is:
#   [1] return traffic to clients  [2] client->panel  [3] game clients <-> wg-game
#   [4] drop everything else
iptables -I FORWARD 1 -i "$WIFI" -j DROP
for ip in $GAME_CLIENTS; do
  # Inbound from wg-game is allowed even when unsolicited (peer-to-peer / Teredo);
  # only the node can send into wg-game.
  iptables -I FORWARD 1 -i "$GAME_IF" -o "$WIFI" -d "$ip" -j ACCEPT
  iptables -I FORWARD 1 -i "$WIFI" -o "$GAME_IF" -s "$ip" -j ACCEPT
done
for sub in $DOCKER_SUBNETS; do
  iptables -I FORWARD 1 -i "$WIFI" -p tcp -d "$sub" --dport "$PANEL_PORT" -j ACCEPT
done
iptables -I FORWARD 1 -o "$WIFI" -m conntrack --ctstate ESTABLISHED,RELATED -j ACCEPT

# ---- Game tunnel MSS clamp (wg-game MTU 1380) ----
iptables -t mangle -C FORWARD -o "$GAME_IF" -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu 2>/dev/null \
  || iptables -t mangle -A FORWARD -o "$GAME_IF" -p tcp --tcp-flags SYN,RST SYN -j TCPMSS --clamp-mss-to-pmtu
iptables -t mangle -C FORWARD -i "$GAME_IF" -p tcp --tcp-flags SYN,RST SYN -m tcpmss --mss 1341:65535 -j TCPMSS --set-mss 1340 2>/dev/null \
  || iptables -t mangle -A FORWARD -i "$GAME_IF" -p tcp --tcp-flags SYN,RST SYN -m tcpmss --mss 1341:65535 -j TCPMSS --set-mss 1340

# ---- INPUT: only allow redsocks port from the hotspot itself ----
iptables -C INPUT -p tcp --dport "$RP" ! -i "$WIFI" -j DROP 2>/dev/null \
  || iptables -A INPUT -p tcp --dport "$RP" ! -i "$WIFI" -j DROP

# ---- IPv6: block forwarding from wlan0 (we only tunnel IPv4) ----
ip6tables -C FORWARD -i "$WIFI" -j DROP 2>/dev/null \
  || ip6tables -I FORWARD 1 -i "$WIFI" -j DROP

echo "xrouter-firewall applied"
```
`/etc/systemd/system/xrouter-firewall.service`:
```ini
[Unit]
Description=XRouter transparent proxy firewall rules
After=network-online.target redsocks.service dnsmasq.service docker.service
Wants=network-online.target

[Service]
Type=oneshot
RemainAfterExit=yes
ExecStart=/usr/local/sbin/xrouter-firewall.sh

[Install]
WantedBy=multi-user.target
```

### `/etc/sysctl.d/99-xrouter.conf`
```ini
net.ipv4.ip_forward=1
net.ipv6.conf.all.forwarding=0
```

---

## 5. Enable & start

```bash
# release wlan0 from the wifi client supplicant
sudo systemctl disable --now wpa_supplicant

# regulatory domain + forwarding
sudo iw reg set IR
sudo sysctl --system

# bring up the AP IP
sudo systemctl restart systemd-networkd
sudo networkctl reconfigure wlan0

# unmask hostapd (Ubuntu masks it until configured), enable everything
sudo systemctl unmask hostapd
sudo systemctl daemon-reload
sudo systemctl enable --now dns-over-socks hostapd dnsmasq redsocks xrouter-firewall
```

---

## 6. Verify

```bash
# all services up
systemctl is-active hostapd dnsmasq redsocks dns-over-socks xrouter-firewall

# AP is broadcasting
sudo iw dev wlan0 info | grep -E 'ssid|type|channel'

# proxy egress works (run on the Pi)
curl --socks5-hostname 127.0.0.1:10808 https://api.ipify.org   # -> proxy exit IP

# DNS tunneled through the proxy
dig @10.42.0.1 example.com +short

# connected clients
sudo cat /var/lib/misc/dnsmasq.leases

# live proxied sessions (client -> redsocks should equal redsocks -> 10808)
sudo ss -tnp state established '( sport = :12345 )' | grep -c 10.42.0
sudo ss -tnp state established '( dport = :10808 )' | grep -c redsocks
```

> **Note:** an `OUTPUT`-chain self-test from the Pi itself (redirecting local curl into
> redsocks) is unreliable — `SO_ORIGINAL_DST` can't recover the original destination for
> *locally-originated* redirected connections. Test from a **real WiFi client** instead.

---

## 7. Operate / troubleshoot

| Symptom | Check |
|---|---|
| Clients connect but no internet | Is xray-core egressing? `curl --socks5-hostname 127.0.0.1:10808 https://api.ipify.org`. If not, start/select a proxy in the `:8080` UI. |
| Web panel `http://10.42.0.1:8080` won't open from a client (but SSH works) | The panel is a **Docker** container: requests to `:8080` are DNAT'd to the container, so they hit `FORWARD` (not `INPUT` like SSH) and get caught by the `-i wlan0 -j DROP` leak guard. The firewall opens a hole for the panel — confirm `iptables -S FORWARD` shows the `--dport 8080 -j ACCEPT` rules **above** the `-i wlan0 -j DROP`. Re-run `sudo systemctl restart xrouter-firewall` if missing (e.g. after the Docker bridge subnet changed). |
| `redsocks` logs `accept: backing off` | fd exhaustion — confirm the `LimitNOFILE=65536` drop-in: `cat /proc/$(pidof redsocks)/limits \| grep 'open files'`. |
| AP not visible | `systemctl status hostapd`; ensure `wpa_supplicant` is disabled and `rfkill list` shows wlan not blocked. |
| DNS fails | `journalctl -u dns-over-socks`; verify it listens on `127.0.0.1:5354` and socks5 egresses. |
| Re-apply firewall | `sudo systemctl restart xrouter-firewall` (script is idempotent). |

Logs: `journalctl -u hostapd -u dnsmasq -u redsocks -u dns-over-socks -f`

---

## 8. Design notes & limitations

- **TCP-only tunneling.** Only TCP (and DNS-over-TCP-via-socks) is proxied. Other UDP —
  including **QUIC (UDP/443)** — is dropped, forcing browsers/apps to fall back to TCP/TLS.
  This prevents leaks but can affect UDP-only apps (some VoIP, game traffic).
  Exception: game clients (section 9) get full UDP through the WireGuard game tunnel.
- **No IPv6 egress** for clients (IPv6 forwarding off, no RA) — avoids an untunneled v6 leak path.
- **Fail-closed.** If the proxy stops, clients lose internet rather than falling back to direct.
- **Respects Xray routing.** All client TCP enters Xray's socks inbound, so Xray's own
  routing/rules/outbound selection apply unchanged.
- **Persistence** is via enabled systemd units + the idempotent firewall service (not
  `netfilter-persistent`), so Docker's volatile iptables chains aren't snapshotted.

---

## 9. Game tunnel (Xbox → WireGuard → Iran-Mullvad node)

Games need UDP (Teredo, lobbies, peer traffic), which neither redsocks nor the
Xray servers carry. So the Xbox bypasses Xray completely: all of its traffic
(TCP, UDP, ICMP) is policy-routed into a WireGuard tunnel, `wg-game`, to the
Iran-Mullvad node (a domestic hop), which sends it out through
its Mullvad DE WireGuard namespace with NAT. Every other client is unchanged.

```
Xbox 10.42.0.124 ─wlan0─► Pi: rule from 10.42.0.124 → table 252 → wg-game
   ══ WireGuard udp/51821 ══► node wg-game ─► netns mv-de (NAT) ─► wg-de ─► Mullvad DE
```

| Piece | Where | What |
|---|---|---|
| Tunnel | `/etc/systemd/network/30-wg-game.netdev` | `wg-game`, MTU 1380, peer `<node-ip>:51821`, `AllowedIPs=0.0.0.0/0`, keepalive 25, `RouteTable=off` (no routes from AllowedIPs) |
| Key | `/etc/systemd/network/wg-game.key` | private key, `root:systemd-network` 640 |
| Routing | `/etc/systemd/network/30-wg-game.network` | `10.250.1.2/24`; table 252 = default via `wg-game`; rule `from 10.42.0.124 lookup 252` (prio 1000), `from 10.42.0.124 to 10.42.0.0/24 lookup main` (prio 999) |
| Static lease | `/etc/dnsmasq.d/xrouter-static.conf` | `dhcp-host=<xbox-mac>,10.42.0.124,XBOX` |
| Firewall | `xrouter-firewall.sh` (`GAME_CLIENTS`) | Xbox TCP skips redsocks; FORWARD allows only Xbox ↔ `wg-game`; MSS clamp 1340. DNS still goes to dnsmasq. |

Node side (`wg-quick@wg-game`, `game-gw`, `game-gw-check.timer`) is documented in
the Notion "VPN Infrastructure" pages (01 · Flow E, Iran-Mullvad node page).

- **No NAT on the Pi:** the node's peer allows `10.42.0.124/32` and routes it back
  itself, so the Xbox sees one NAT less. Expect NAT type *Moderate* (Mullvad has no
  port forwarding).
- **Fail-closed:** if `wg-game` is down, table 252 has no route, the Xbox falls through
  to `main` and the `-i wlan0 -j DROP` rule blocks it. It never leaks direct, and it
  does not fall back to redsocks.
- **Add another game client:** give it a static lease, add its IP to `GAME_CLIENTS`
  and to a `[RoutingPolicyRule]` pair in `30-wg-game.network`, add it to the node
  peer's `AllowedIPs` and to `ROUTED` in `game-gw`.

```bash
networkctl status wg-game                  # link state
ip rule; ip route show table 252           # policy routing
ping -c3 10.250.1.1                        # node end of the tunnel
ip route get 1.1.1.1 from 10.42.0.124 iif wlan0   # should say: dev wg-game table 252
```

### Download mode: `xbox-mode`

Big downloads (game installs/updates) can be faster through Xray than through the
tunnel: redsocks → Xray splits each TCP connection into shorter hops, while in the
tunnel one TCP connection spans the whole ~130 ms path over UDP. `xbox-mode` switches
only the Xbox's **TCP** between the two paths; its UDP (games, Teredo) always stays in
`wg-game`. With the Germany (Mullvad) proxy active in the panel, both paths exit from
the same IP.

| Mode | Xbox TCP | Xbox UDP | How it is set |
|---|---|---|---|
| `game` (default) | `wg-game` | `wg-game` | `xrouter-firewall.sh` adds `-s 10.42.0.124 -j RETURN` at the top of `XROUTER` |
| `download` | redsocks → Xray (v2ray) | `wg-game` | that `RETURN` rule is removed, so the Xbox's TCP hits the `REDIRECT` like every other client |

```bash
sudo xbox-mode download   # before a big download, then pause/resume it once
sudo xbox-mode game       # back to the tunnel for everything
sudo xbox-mode status     # show the current mode
```

- Connections that are already open keep their path; pause and resume a running
  download after switching.
- `download` is not persistent: a reboot or `systemctl restart xrouter-firewall`
  brings back `game` mode.
- The rule is removed by line number, not by spec: `iptables -D XROUTER -s 10.42.0.124
  -j RETURN` failed to match the rule that the firewall script inserted.

`/usr/local/sbin/xbox-mode` (install with `sudo install -m 755 xbox-mode /usr/local/sbin/`):
```bash
#!/bin/bash
# xbox-mode: choose how the Xbox's TCP leaves the XRouter hotspot.
#   game     - all Xbox traffic via the WireGuard game tunnel (default; also after reboot)
#   download - Xbox TCP via redsocks -> Xray (v2ray); UDP still via the game tunnel
#   status   - show the current mode
# Run as root: sudo xbox-mode game|download|status
set -uo pipefail
XBOX=10.42.0.124

# Line numbers of the "skip redsocks" RETURN rule(s) for the Xbox, highest first.
return_lines() {
  iptables -t nat -L XROUTER -n --line-numbers \
    | awk -v ip="$XBOX" '$2 == "RETURN" && ($5 == ip || $5 == ip "/32") {print $1}' | sort -rn
}

case "${1:-status}" in
  download)
    for n in $(return_lines); do iptables -t nat -D XROUTER "$n"; done
    ;;
  game)
    systemctl restart xrouter-firewall
    ;;
  status) ;;
  *)
    echo "usage: xbox-mode game|download|status" >&2
    exit 2
    ;;
esac

if [ -n "$(return_lines)" ]; then
  echo "mode: game     (all Xbox traffic via wg-game)"
else
  echo "mode: download (Xbox TCP via v2ray, UDP via wg-game)"
fi
echo "XROUTER chain (first rules):"
iptables -t nat -S XROUTER | head -4 | sed 's/^/  /'
```
