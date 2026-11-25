<div align="center">

# 📞 dongie-ims

**Keeps an OpenStick LTE dongle registered for Wi-Fi calling (VoWiFi), as a small service with a socket API**

[![License: GPL-3.0](https://img.shields.io/badge/license-GPL--3.0-blue?style=flat-square)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.26-00ADD8?style=flat-square&logo=go&logoColor=white)](go.mod)
[![Hardware](https://img.shields.io/badge/hardware-MSM8916%20%C2%B7%20UZ801-8839ef?style=flat-square)](#hardware)
[![Nix flake](https://img.shields.io/badge/built%20with-Nix-5277C3?style=flat-square&logo=nixos&logoColor=white)](flake.nix)
[![Built with Claude Opus 5.5 & Fable 5.2](https://img.shields.io/badge/built%20with-Claude%20Opus%205.5%20%26%20Fable%205.2-D97757?style=flat-square&logo=claude&logoColor=white)](https://claude.com/claude-code)

</div>

dongie-ims uses [damonto/ims-go](https://github.com/damonto/ims-go) to register the SIM
with the carrier's IMS core over **VoWiFi**. It opens an IKEv2/SWu tunnel to the ePDG
through the stick's ordinary LTE data connection, while the modem handles only the SIM
authentication (EAP-AKA, through `qmi-proxy`). No routes, interfaces or modem profiles
are changed, because the tunnel runs in userspace.

[dongie](https://github.com/derpyspike/dongie-rs) (dongie-rs, the EUPL-1.2 web dashboard) displays the status and
call log, and lets you **take and make calls in the browser**. Any SMS the network
delivers over IMS is copied into dongie's Messages.

### Browser calls

```
phone network ⇄ ePDG ⇄ IMS RTP (AMR-WB / AMR) ⇄ dongie-ims: decode ⇄ 16 kHz PCM ⇄ G.722 ⇄ WebRTC (DTLS-SRTP) ⇄ browser
```

- Signalling (SDP offer/answer) goes browser → dongie (admin login + CSRF) → socket → dongie-ims
- Media: [pion/webrtc](https://github.com/pion/webrtc), UDP 40100-40199, only on tailnet (100.64/10),
  hotspot (192.168.4/24) and USB (192.168.5/24) addresses; no STUN/TURN
- G.722 (HD) to the browser, own pure-Go codec in [`g722/`](g722) (round-trip SNR 49 dB low band /
  30 dB wideband in tests). AMR-WB/AMR via ims-go's pure-Go codecs
- Measured on the stick (1 GHz Cortex-A53): AMR-WB encode ~35-38 % of one core, decode ~6-7 %
- Browsers allow the microphone only over HTTPS: `tailscale serve --bg --https=443 http://<ts-ip>:8040`
  (tailnet only, **no Funnel**)

## Status

| | |
|---|---|
| Registration (Orange ES / Simyo) | ✅ ~2 s, re-registers on its own, watchdog restarts the session after 10 min without registration |
| Incoming calls | ✅ ring in dongie's call bar; answer in the **browser** (WebRTC) or reject; logged (answered / missed / rejected) |
| SMS over IMS | ✅ received and passed to dongie (sending still goes through ModemManager) |
| Outgoing calls | ✅ code done (dialer in dongie, charged to the SIM), not tested on the network yet |
| VoLTE (IMS PDN on the modem) | ❌ the MSM8916 firmware doesn't forward data on the second bam-dmux channel |

## Build & install

```bash
nix develop
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -trimpath -ldflags='-s -w' -o dongie-ims .
scp dongie-ims deploy/dongie-ims.service user@mystick:/tmp/
ssh user@mystick 'sudo install -m 0755 /tmp/dongie-ims /usr/local/bin/ &&
  sudo install -m 0644 /tmp/dongie-ims.service /etc/systemd/system/ &&
  sudo systemctl daemon-reload && sudo systemctl enable --now dongie-ims'
```

The binary is a static ~18 MB build and uses ~11 MB of RAM when idle. The unit caps memory at
`MemoryMax=96M` with `GOMEMLIMIT=40MiB`.

## Flags

| flag | default | |
|---|---|---|
| `-socket` | `/run/dongie-ims/api.sock` | API socket, owned `root:<group>`, mode 0660 |
| `-group` | `dongie` | group allowed to use the socket |
| `-carriers` | built-in [`carriers.json`](carriers.json) | ims-go carrier profiles (ePDG, IKE DH groups, IPsec algorithms) |
| `-imei` | from ModemManager | terminal IMEI (the stick's own; no spoofing) |
| `-on-call` | `ring` | `ring` lets the network decide (voicemail / no answer); `reject` sends 486 Busy |
| `-log-level` | `info` | `debug` also turns on ims-go's SIP traces (these contain phone numbers) |

## API (unix socket only)

```bash
curl --unix-socket /run/dongie-ims/api.sock http://x/v1/status
```

| | |
|---|---|
| `GET /v1/status` | `registered`, `since`, `registrations`, `msisdn`, `inner_ip`, `pcscf`, `last_error`, … |
| `GET /v1/calls` | RAM call log (last 50) |
| `GET /v1/messages?after=N` | SMS received over IMS with id > N (last 100) |
| `POST /v1/calls/{id}/reject` | reject a ringing call |
| `POST /v1/calls/{id}/answer` | `{"sdp": offer}` → `{"sdp": answer}`: answer and bridge to the browser |
| `POST /v1/calls/{id}/hangup` | hang up / cancel dialing / reject if ringing |
| `POST /v1/calls/{id}/dtmf` | `{"digits": "1#"}` |
| `POST /v1/dial` | `{"to": "+34…", "sdp": offer}` → `{"id", "sdp"}` |

No TCP listener is opened. Call log and SMS are kept in RAM only. In the logs, phone
numbers are masked (`+34•••••98`).

## Hardware

Tested on a UZ801 v3 (MSM8916) running Debian 13 with ModemManager and `qmi-proxy`,
using an Orange ES network (Simyo SIM). Measured Orange settings: ePDG
`epdg.epc.mnc003.mcc214.pub.3gppnetwork.org`, IKE DH 14 accepted, SIP sec-agree
`hmac-md5-96` / `ealg=null`, AMR-WB offered for HD voice.

## License

GPL-3.0-only, since it links ims-go (GPL-3.0). It runs as a separate process from
dongie (EUPL-1.2), and the two talk only over the socket.
