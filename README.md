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

It can also register over **VoLTE** (IMS over the LTE connection itself), and the
**mode** is chosen in dongie: VoWiFi only, VoLTE only, VoLTE with VoWiFi as fallback,
VoWiFi with VoLTE as fallback, or off. See [VoLTE](#volte).

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
| VoLTE (IMS PDN on the modem) | ✅ registration in ~1-1.5 s, incoming call answered with AMR-WB (echo test, 2026-11-26, Orange ES) |
| Fallback between VoWiFi and VoLTE | ✅ VoWiFi blocked → VoLTE registered ~10 s later; primary retried every 15 min while no call is up |
| Voicemail on the stick | ✅ takes unanswered calls (after N s or at once), greeting (own WAV / beep / none), records until hang-up (max 10-300 s); 16.5 s message from a 17.5 s call kept its pauses (2026-11-26) |

## VoLTE

The MSM8916 modem has a second QMI control channel, `DATA6_CNTL`, whose data goes to
bam-dmux channel 1 (`wwan1`). dongie-ims runs the IMS PDN there, next to
ModemManager's internet connection on DATA5 (`wwan0qmi0` / `wwan0`). The approach is
from [damonto/sigmo](https://github.com/damonto/sigmo)'s "Qualcomm 410" data path (MIT):

1. `DATA6_CNTL` as an rpmsg_char endpoint (`RPMSG_CREATE_EPT_IOCTL` on the modem edge's
   `/dev/rpmsg_ctrlN` → `/dev/rpmsgN`). The mainline kernel has no QMI device for it.
   ModemManager must ignore it: [`deploy/78-mm-ignore-rpmsg-ept.rules`](deploy/78-mm-ignore-rpmsg-ept.rules),
   otherwise it makes DATA6 its primary port and LTE data stops.
2. The endpoint resets the channel when closed, so one session keeps it open. rpmsg
   returns whole messages per `read()`; a small adapter feeds wwan-go's framing.
3. WDA link layer **raw-IP** (DATA6 defaults to 802.3), a persistent 3GPP profile for
   APN `ims` with **P-CSCF via PCO** (without it the modem returns no P-CSCF), the
   P-Access-Network-Info from the serving cell.
4. `wwan1` (moved out of the `null` netns that OpenStick-Builder parks it in) gets the
   PDN address as a point-to-point link; sockets on it use routing table 1006. The
   main routing table isn't touched.
5. IPsec (sec-agree) to the P-CSCF: ims-go does ESP in userspace over a raw socket, so
   no kernel xfrm/ESP is needed. [`patches/ims-go-esp-numeric.patch`](patches/ims-go-esp-numeric.patch)
   opens it as `ip4:50`: the static build can't resolve `ip4:esp` on the stick.

### Modes

dongie writes the mode to `/var/lib/dongie/ims-mode` (`-mode-file`) and calls
`POST /v1/mode/reload`: `vowifi` (default when missing), `volte`, `volte+vowifi`,
`vowifi+volte`. With a fallback, the second access starts when the first ends or
can't register; the first is retried every 15 min, never during a call. "Off" is
dongie stopping the service.

## Build & install

The ims-go patch is applied to a vendored copy (`vendor/` isn't committed):

```bash
nix develop
scripts/vendor.sh   # go mod vendor + patches/
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go build -mod=vendor -trimpath -ldflags='-s -w' -o dongie-ims .
scp dongie-ims deploy/dongie-ims.service deploy/78-mm-ignore-rpmsg-ept.rules user@mystick:/tmp/
ssh user@mystick 'sudo install -m 0755 /tmp/dongie-ims /usr/local/bin/ &&
  sudo install -m 0644 /tmp/dongie-ims.service /etc/systemd/system/ &&
  sudo install -m 0644 /tmp/78-mm-ignore-rpmsg-ept.rules /etc/udev/rules.d/ &&
  sudo udevadm control --reload &&
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
| `-mode-file` | `/var/lib/dongie/ims-mode` | access mode, written by dongie (see [Modes](#modes)) |
| `-voicemail-file` | `/var/lib/dongie/ims-voicemail.json` | voicemail settings, written by dongie: `mode` off/delay/now, `delay_secs`, `greeting` none/beep/file, `max_secs`, `persist` (flash `<state-dir>/voicemail`, else RAM `/run/dongie-ims/voicemail`; 30 messages / 20 MB max) |
| `-greeting` | `/var/lib/dongie/ims-greeting.wav` | the greeting: WAV PCM 16-bit mono 16 kHz, up to 60 s (dongie's page converts uploads/recordings) |
| `-log-level` | `info` | `debug` also turns on ims-go's SIP traces (these contain phone numbers) |

## API (unix socket only)

```bash
curl --unix-socket /run/dongie-ims/api.sock http://x/v1/status
```

| | |
|---|---|
| `GET /v1/status` | `access` (vowifi/volte), `mode`, `fallback`, `registered`, `since`, `registrations`, `msisdn`, `inner_ip`, `pcscf`, `last_error`, … |
| `POST /v1/mode/reload` | re-read the mode file and switch now (ends the current session) |
| `GET /v1/voicemail` | recordings (`id`, `from`, `at`, `secs`, `codec`, `stored`) + settings |
| `GET /v1/voicemail/{id}.wav` | a recording as WAV (16 kHz mono; stored as G.722) |
| `DELETE /v1/voicemail/{id}` | delete one |
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
