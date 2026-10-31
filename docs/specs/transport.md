# Transport security: behaviour spec

Status: draft for review. Part 3, study task.

This spec says how Flightdeck clients and habs authenticate and encrypt their connections, and how
habs do the same with each other. It covers device identity, pairing, rotation and revocation. It
describes behaviour in our own words. It copies no code from the reference projects.

Owner rules this spec works within:

- A tailnet is fine as a network underneath. It is not part of the product. No hab trusts a caller
  because of its tailnet identity.
- SSH stays the first transport.
- One Mac alone must work with no network, no hub and no pairing.

Each section that settles a question ends with a **Decision** line, as in `hab-logic-api.md`.

## Contents

1. [References](#1-references)
2. [Threat model and goals](#2-threat-model-and-goals)
3. [Options](#3-options)
4. [Recommendation](#4-recommendation)
5. [Device identity](#5-device-identity)
6. [Flows](#6-flows)
7. [Cases](#7-cases)
8. [Discuss-first for the owner](#8-discuss-first-for-the-owner)
9. [Plan changes](#9-plan-changes)

---

## 1. References

### 1.1 Rex

- The server listens on a local Unix socket only, until the owner adds a network listener.
- Rex trusts a caller "based only on how they connect": Unix peer credentials, or the tailnet's
  identity for the calling node. The default admits only the user who owns the machine.
- Network carriers: an embedded tailnet node with HTTPS on the tailnet's DNS name; a forward from
  the tailnet's serve feature to the socket; or plain HTTP on the tailnet address, with the tailnet
  doing the encryption.
- Plain HTTP on other addresses is refused unless the owner passes an "allow anonymous" flag. The
  docs warn that every caller then has full access.
- `server tailscale pair` shows a QR code to pair a device. The welcome may list other carriers,
  such as QUIC, with a port and ALPN.
- A login server maps caller identities (a tailnet login, a tag, a Unix uid) to accounts. Reloading
  that file closes connections whose access was removed.
- The docs say a full authentication system is planned, including support for existing SSH keys.

What we take: local socket by default; a network listener only by explicit choice; refuse an
unauthenticated listener; close live connections when access is removed; a QR code for pairing.
What we leave: identity that comes from the tailnet.

Source: superlogical.com/rex/docs (llms-full.txt).

### 1.2 herdr and herdr-core

- `herdr --remote <host>` runs a thin local client. It opens SSH, starts or attaches the remote
  server, and streams the UI back. It adds SSH keepalives by default.
- herdr-core speaks JSON-RPC over stdio, so `ssh <host> hc connect` reaches a remote core.
- Neither adds its own cryptography or pairing. SSH keys are the only identity.

What we take: SSH stdio as the zero-listener transport (already `gff-hab connect`).

Source: the herdr and herdr-core repositories and herdr.dev.

### 1.3 Happy

- The computer shows a QR code that holds a one-time public key. The phone scans it and returns
  the account key, sealed so only that computer can open it. The computer is authorized when the
  owner approves it on the phone.
- A relay carries encrypted blobs between devices and stores them until both sides are ready. Both
  devices connect outward, so neither opens an inbound port.
- Keys form a hierarchy: an account secret on the phones, a derived content key pair, and random
  per-session and per-machine data keys.
- Removing a computer from the account leaves it able to read the session keys it already had.
- A lost phone is handled by restoring the account secret from a backup code. The docs describe no
  revocation of a phone.

What we take: the computer shows the QR code; a one-time value in the code; outbound-only as a
later carrier. What we leave: a hosted relay in the default path, and an account secret copied to
every phone (a lost phone then exposes everything).

Source: happy.engineering/docs/security and /docs/how-it-works.

### 1.4 Moshi

- An iOS terminal for coding agents. It connects straight to the machine over SSH or Mosh. No relay.
- Private keys live in the iOS Keychain, gated by Face ID or Touch ID.
- Mosh starts with SSH, which hands a session key to a UDP carrier that survives network changes.

What we take: keys in the Keychain behind biometrics; the pattern of an SSH-authenticated start
that sets up a faster carrier.

Source: the App Store listing and getmoshi.app.

### 1.5 Secure ShellFish

- An iOS SSH client. It can make keys inside the Secure Enclave, so the private key never leaves
  the device. It also takes keys from a hardware token and short-lived certificates from a CA.

What we take: Secure Enclave keys for the phone. Note: the Secure Enclave makes P-256 keys only,
not Ed25519.

Source: secureshellfish.app/help/ssh-keys.

### 1.6 Syncthing device ids

- Each device makes a key pair and a self-signed certificate on first start.
- The device id is the SHA-256 hash of that certificate, in base32 with check digits.
- Devices talk TLS with certificates on both sides. Each side computes the other's id from its
  certificate and drops the connection unless that id is in its configuration. No CA is involved.
- Known weak points: long ids are hard to compare by eye, and unauthenticated local discovery lets
  a device claim another's id (the TLS check still refuses it).

What we take: device id as a hash of the device's certificate, checked against a pinned list, with
standard TLS doing the rest.

Source: docs.syncthing.net/dev/device-ids.html.

### 1.7 Noise

- The Noise framework builds handshakes from Diffie-Hellman steps. XX: three messages, both sides
  send their static keys. IK: the initiator already knows the responder's static key.
- Some designs pair with a QR code or PIN that feeds a PAKE, then run Noise XX between device keys.

What we take: nothing in the first build (see 3). Noise stays the fallback if TLS client
authentication proves unworkable on iOS.

### 1.8 Apple platform facts

- Network.framework does TLS 1.3 and WebSocket. A verify block replaces the system trust check, so
  the app can pin a certificate hash instead of trusting a CA.
- Network.framework can export keying material from a TLS session (an exporter secret). That lets a
  client sign a value bound to this exact TLS session. Verify this in the stage that builds it.
- A jailbroken or instrumented device can bypass pinning in its own app. That attacks the phone's
  owner only, not the hab.

---

## 2. Threat model and goals

| Threat | Goal |
|---|---|
| Someone on the same LAN, café Wi-Fi or tailnet connects to a hab | Refused unless their device key is paired with that hab |
| Someone on the path watches or changes traffic | Everything encrypted and integrity-checked; endpoints pinned, so no CA can be abused |
| A lost or stolen phone | Revoked from any hab; the revocation reaches every hab; the key cannot be copied off the phone |
| A compromised hab | It can act on its own host only; it cannot mint trust for new devices on other habs without the owner |
| A hosted service goes away | Nothing depends on one; no hub, no relay in the default path |
| One Mac alone | Works with the local socket only; no keys, no listener, no pairing |

Out of scope: protecting the owner from a compromised account on a hab's own host. A process that
runs as the owner can already reach the local socket.

---

## 3. Options

| Option | How it works | For | Against |
|---|---|---|---|
| A. SSH only | Every client and peer runs `ssh <host> gff-hab connect`. The phone holds a Secure Enclave key in each host's authorized keys | No new listener; proven crypto; works today for `gff` and peers | The phone needs sshd reachable and a key added to every host by hand; no QR pairing; each host's SSH config becomes product config; a restricted key entry is needed so the phone gets `gff-hab connect` and not a shell |
| B. TLS 1.3 with pinned self-signed device keys and QR pairing | Each device has a key and a self-signed certificate. Its device id is the certificate hash. A hab listener admits only paired device ids. Pairing scans a QR code from the hab | Standard TLS in Go and in Network.framework; WebSocket rides on it, as `hab-logic-api.md` 4.3 already expects; same model later carries QUIC; Syncthing proves it at scale | A new listening port; client certificates on iOS need care (a Keychain identity); a second credential beside SSH keys |
| C. Noise handshake with device keys | Noise XX or IK over TCP or inside a WebSocket, with pinned static keys | Small, no certificates, strong identity hiding | Needs a Swift Noise library or our own; custom framing; harder to inspect with standard tools; we would own more crypto code |
| D. Tailnet identity (WhoIs) | The hab binds to the tailnet address and trusts the tailnet's view of the caller | No new keys | Ruled out by the owner: the tailnet would become part of the product; a loose ACL becomes a loose API |
| E. End-to-end encrypted relay (Happy) | Both sides connect outward to a relay that forwards sealed messages | Works from anywhere with no inbound port and no VPN | A hosted service in the path; latency; a relay to run or trust for liveness; contradicts "no hub" if it becomes the default |
| F. SSH-started fast carrier (Mosh pattern) | SSH starts the session and hands a key to a UDP or QUIC carrier | Survives roaming | A second protocol; the first build does not need roaming beyond reconnect |

Carriers are a separate axis from identity. Under B, the same device-key check runs whether the
bytes arrive over the LAN, a tailnet, an embedded tailnet node in the app, or (later) a relay.

---

## 4. Recommendation

**Keep SSH first, and add B: a paired TLS listener with pinned device keys, off by default.**

1. **Local socket, unchanged.** One Mac alone uses only the Unix socket. No keys, no pairing.
2. **SSH stdio, unchanged and first.** `gff` and hab peers keep using `ssh <host> gff-hab connect`.
   SSH keys stay their authentication. New: the welcome carries the hab's device id, and the caller
   pins it. So SSH bootstraps hab-to-hab trust with no QR code.
3. **Paired TLS listener, new and off by default.** A hab can open one TLS 1.3 listener carrying
   WebSocket, with the same messages as `hab-logic-api.md` 4.3. It admits a connection only when
   both sides present a pinned device id. The owner enables it and names the bind address: loopback,
   one LAN address, or the tailnet address. A tailnet is one possible bind address, not an identity.
4. **The phone uses the listener**, paired by QR code. It does not need SSH. Its key lives in the
   Secure Enclave.
5. **Carriers for the phone off the LAN** are a later, separate choice: an embedded tailnet node in
   the app (the original task-iv89iwd8), or an end-to-end relay. The device-key check does not
   change. Neither is in the first build.

Reasons:

- It meets the owner rule: the product has its own security, and a tailnet is only a network.
- SSH stays first and loses nothing. Hab-to-hab trust grows from SSH, which the owner already uses.
- The phone gets a real pairing flow instead of hand-editing authorized keys on every host.
- TLS 1.3 is in the Go standard library and in Network.framework. We write the trust check, not the
  crypto. Syncthing has run this model for years.
- The listener is off by default, so a hab that never enables it opens no port.
- QUIC is TLS 1.3 underneath, so a QUIC carrier later reuses the same device certificates.

How the client proves its key over TLS has two workable forms. The build stage picks one after a
spike on iOS:

- **B1. Mutual TLS.** The client presents its device certificate in the handshake. Needs a Keychain
  identity built from a Secure Enclave key and our self-signed certificate on iOS.
- **B2. Server-pinned TLS plus a signed exporter.** The client pins the hab's certificate. In
  `hab.hello` it sends its device certificate and a signature, made with its device key, over the TLS
  exporter value. The hab checks the signature before it answers. This binds the proof to this TLS
  session and avoids client certificates on iOS.

Prefer B1 if the iOS spike works, because the hab refuses an unknown device before any API message.
Noise (C) is the fallback only if both forms fail on iOS.

Decision: open (/consensus).

---

## 5. Device identity

| Item | Rule |
|---|---|
| Device key | One per device, made on first start. P-256 (the Secure Enclave's curve) on Apple devices; P-256 or Ed25519 on habs. One curve everywhere is simpler; the stage decides |
| Where it lives | Phone and iPad: Secure Enclave, this device only, never in backups. Mac app: Keychain. Hab: a key file, owner-only permissions, in the hab's state directory |
| Certificate | Self-signed, long-lived. Its only job is to carry the key through TLS |
| Device id | SHA-256 of the certificate, base32, grouped, with check digits. Shown short (first group) in lists and full on request |
| Device record | id, label, kind (hab, phone, desktop, gff), paired at, paired by, last seen, state (paired, revoked) |
| Trust store | One file per hab: the devices it admits and the peers it pins. Revocations stay in the file as tombstones |

A hab has one device id for both roles: as the listener's server identity and as a peer that dials
other habs.

Decision: proposed.

---

## 6. Flows

### 6.1 First pairing: a phone and a hab

1. The owner runs `gff-hab pair` on the hab, or presses "Pair a device" in the Mac app.
2. The hab makes a pairing offer: its device id, its listener endpoints, and a one-time token
   (128 bits). The token expires after five minutes or one use.
3. The hab shows the offer as a QR code, and as text for a device with no camera.
4. The phone scans it. It connects to an endpoint over TLS and pins the hab's device id from the
   code. A different certificate fails the connection.
5. The phone sends a pair request: the token, its device certificate and its label.
6. The hab checks the token. It shows the phone's short id and label and asks the owner to confirm.
   Scanning proves the phone saw the screen; the confirm step stops a photo of the code from being
   enough.
7. The hab adds the phone to its trust store, burns the token, and logs the pairing.
8. The phone stores the hab: id, label, endpoints.

The hab's listener must be enabled for this flow. The hab opens it for pairing only if the owner
asks; otherwise `gff-hab pair` says the listener is off.

### 6.2 The phone and the other habs

A phone paired with one hab still needs admission to each other hab. Two options:

- **Pair each hab.** Repeat 6.1 on each hab. Simple; one QR per host.
- **Introduction.** The first hab sends the phone's record to its peers over the authenticated
  hab-to-hab path. Each peer admits the phone only after the owner confirms on any client that is
  already paired with that peer.

Recommendation: pair each hab in the first build. Introduction is a trust-propagation choice for
the owner (see 8).

The phone learns the other habs' device ids and endpoints from the first hab. Learning a server's
id this way is safe: it came over a pinned, authenticated channel.

### 6.3 Hab to hab

1. A hab reaches a peer over SSH, as Part 3 stage 2 already plans.
2. The welcome carries the peer's device id. The caller stores it in its trust store as a pinned
   peer.
3. If the listener is in use between habs later, each side already knows the other's id, so no QR
   code is needed.
4. If an endpoint later presents a different id, the caller refuses it and says so. This replaces
   the host-id check in stage 2.

### 6.4 The desktop client and `gff`

- On the same host, they use the local socket. No pairing.
- On another host, `gff` uses SSH first. The Mac app may use SSH or pair with the listener, by the
  same flow as the phone.

### 6.5 Key rotation

- **A client rotates.** It makes a new key and certificate. Over a live, authenticated connection
  it sends a rotate request, signed by the old key, carrying the new certificate. Each hab replaces
  the record and keeps the old id as a tombstone. The client repeats this on each hab it knows.
- **A hab rotates.** It sends the new certificate to each connected client and peer, signed by the
  old key. They pin both ids for a grace period (seven days), then only the new one. A client that
  was offline for the whole grace period must pair again.
- No automatic schedule in the first build. Rotation runs when the owner asks.

### 6.6 Revocation

1. The owner runs `gff-hab devices revoke <id>` on any hab, or revokes from any paired client.
2. That hab marks the record revoked and closes the device's live connections at once.
3. It sends a revocation to every peer over the hab-to-hab path. Each peer revokes and closes too.
4. A peer that is offline gets the revocation from its peers' outbox when it comes back (the outbox
   from Part 3 stage 5).
5. Revocation always wins. A hab accepts a revocation from any pinned peer. It never accepts an
   admission that way without the owner (see 6.2).
6. A revoked device that reconnects gets a clear "revoked" error and nothing else.

---

## 7. Cases

### 7.1 One Mac alone

- The Mac app and `gff` talk to the local socket. Unix peer credentials authenticate them.
- No listener, no keys in use, no pairing, no network. The hab still makes its device key on first
  start, so pairing later needs no migration.

### 7.2 The iPhone with no system VPN

- On the home LAN: the phone reaches a hab whose listener binds a LAN address. Works with no VPN.
- On a tailnet with the system VPN on: the phone reaches a listener on the tailnet address. The
  device-key check still applies.
- Away from home with no VPN: the first build shows the hab as offline. Later carriers, each its own
  owner decision: an embedded tailnet node in the app, or an end-to-end relay. A router port forward
  is possible but not recommended.
- Background notification actions: the Secure Enclave key needs the device unlocked when biometrics
  gate it. iOS already asks for unlock on actions marked as needing authentication, so an approval
  from the lock screen fits this.

### 7.3 A lost phone

1. The owner revokes the phone from any hab or any other paired client (6.6).
2. Every hab closes the phone's connections and refuses it from then on.
3. The phone's key cannot be copied: it lives in the Secure Enclave and is excluded from backups.
4. Until revoked, a thief also needs the phone's passcode or biometrics to use the key, because the
   app gates key use behind them.
5. A new phone pairs from scratch. Nothing to restore: each device has its own key, so no shared
   secret was on the lost phone.

### 7.4 A hab's disk dies

- Its device key is gone. A rebuilt hab makes a new key and pairs again.
- Peers see a different id at the old endpoint and refuse it until the owner accepts the new id
  (`gff-hab peers accept <label>`), which shows both ids.

### 7.5 The listener on a shared network

- Port scanners see a TLS listener. The handshake fails for every device that is not paired.
- The hab logs each refusal with the presented device id and source address, rate-limits repeats,
  and never answers an API message before authentication.

---

## 8. Discuss-first for the owner

These follow the owner's hard stops: a new listening port, and any change to how the owner
authenticates.

| Item | Why it is discuss-first | Proposed default |
|---|---|---|
| Enabling the paired TLS listener on any hab | A new listening port | Off. The owner enables it per hab and names the bind address |
| The bind address: LAN, tailnet address, or both | Decides who can reach the port | One address per hab, never all interfaces; tailnet address first |
| The port number | A new port on each host | One fixed default in the 2700-9999 range, checked against ports in use, overridable by config |
| Device keys as a new way to authenticate to a hab | Changes how the owner authenticates (a second credential beside SSH keys) | Yes, for the listener only; SSH keys unchanged |
| Introduction between habs (6.2) | Trust spreads from one hab to others | Off; pair each hab |
| An embedded tailnet node in the iOS app | A new tailnet node; may need a tag, grant or ACL change | Later; propose the ACL diff first |
| An end-to-end relay | A service reachable from outside the LAN and tailnet | Later, if at all |
| A router port forward | Exposes a port beyond the LAN and tailnet | Not recommended |
| Adding the phone's key to sshd (option A for the phone) | Changes how the owner authenticates to every host | Not needed under the recommendation |

Not discuss-first: the local socket, SSH stdio, the hab making its own device key, and the welcome
carrying the device id. None opens a port or changes how the owner authenticates.

---

## 9. Plan changes

These change only if the owner adopts the recommendation. This study does not edit them.

### 9.1 Part 3 stage 2, task-phkmrjaw (peer list, SSH transport, online and offline)

- Add: each hab makes its device key on first start; the welcome carries its device id.
- Add: each peer entry may carry a pinned device id; the first SSH connection fills it in.
- Change: "detect that an endpoint now leads to a different hab (host id in the welcome)" becomes a
  device-id check, with `gff-hab peers accept` for a deliberate change.
- Change: online and offline come from the connection; the tailnet's local status becomes optional,
  never required.

### 9.2 Part 3 stage 3, task-99pfhjlg (shelved tailnet listener)

Replace its scope and unshelve it as **"Paired TLS listener with device keys and QR pairing
(owner)"**:

- A TLS 1.3 WebSocket listener, off by default, on one configured address, never all interfaces.
- Device-id admission (B1 or B2 after the iOS spike); refusal before any API message.
- `gff-hab pair`, `gff-hab devices list|revoke|rename`, rotation (6.5) and revocation fan-out to
  peers (6.6).
- Drop WhoIs authorization and the ACL-grant work. The tailnet is a bind address only.
- The owner approves the port, the bind address and the auth model before the listener is enabled
  on any real host.

### 9.3 Part 6, task-iv89iwd8 (iOS transport)

Rescope from "embedded tailnet on iOS" to **"iOS transport: device key, QR pairing, pinned TLS"**:

- A Secure Enclave device key and self-signed certificate; the device id shown in Settings.
- QR pairing (6.1) with a camera scanner and a text fallback.
- A Network.framework TLS 1.3 WebSocket client with the hab's id pinned in the verify block.
- The `Transport` protocol keeps its unit-tested fake; the first carrier is the plain network
  (LAN, or the tailnet when the system VPN is on).
- The embedded tailnet node becomes an optional follow-up task, marked owner.
- A spike decides B1 or B2 and records the result in this spec.

### 9.4 `hab-logic-api.md`

- **3.2 Handshake:** add `device_id` to the welcome. On the paired listener under B2, the hello
  carries the client's certificate and exporter signature.
- **3.1 Protocol:** add a `devices.*` method group (list, revoke, rename, rotate) and `hab.pair`.
- **3.4 Capabilities:** add a capability string that says the hab's paired listener is on.
- **4.2 SSH stdio:** add that the client pins the welcome's `device_id`.
- **4.3:** rename from "Tailnet WebSocket" to "Paired TLS listener (Part 3)". Keep the message
  mapping. Add: data tickets are bound to the authenticated device, and the transport reports the
  device id to gff-hab so events and input carry who sent them. Point to this spec for
  authentication.
