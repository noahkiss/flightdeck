# Transport security: behaviour spec

Status: recommendation settled by /consensus (3/3), 2026-10-31. Part 3, study task.

This spec says how Flightdeck clients and habs authenticate and encrypt their connections, and how
habs do the same with each other. It covers device identity, pairing, rotation and revocation. It
describes behaviour in our own words. It copies no code from the reference projects.

Owner rules this spec works within:

- A tailnet is fine as a network underneath. It is not part of the product. No hab trusts a caller
  because of its tailnet identity.
- SSH stays the first transport. The owner prefers SSH key auth: simple and familiar.
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

What we take: SSH from the phone with keys behind biometrics; the pattern of an SSH-authenticated
start that can set up a faster carrier later.

Source: the App Store listing and getmoshi.app.

### 1.5 Secure ShellFish

- An iOS SSH client. It can make keys inside the Secure Enclave, so the private key never leaves
  the device. It also takes keys from a hardware token and short-lived certificates from a CA.

What we take: Secure Enclave keys for the phone. The Secure Enclave makes P-256 keys only, not
Ed25519. P-256 is a standard SSH key type (`ecdsa-sha2-nistp256`), so the phone's key is an
ordinary SSH key.

Source: secureshellfish.app/help/ssh-keys.

### 1.6 Syncthing device ids

- Each device makes a key pair and a self-signed certificate on first start.
- The device id is the SHA-256 hash of that certificate, in base32 with check digits.
- Devices talk TLS with certificates on both sides. Each side computes the other's id from its
  certificate and drops the connection unless that id is in its configuration. No CA is involved.
- Known weak points: long ids are hard to compare by eye, and unauthenticated local discovery lets
  a device claim another's id (the TLS check still refuses it).

What we take: a device id that is a hash of the device's public key, checked against a pinned list.
Under SSH the key is the hab's host key, and the id is its fingerprint.

Source: docs.syncthing.net/dev/device-ids.html.

### 1.7 Noise

- The Noise framework builds handshakes from Diffie-Hellman steps. XX: three messages, both sides
  send their static keys. IK: the initiator already knows the responder's static key.
- Some designs pair with a QR code or PIN that feeds a PAKE, then run Noise XX between device keys.

What we take: nothing (see 3).

### 1.8 SSH libraries and platform facts

- Go: `golang.org/x/crypto/ssh` implements an SSH server and client. The server decides which
  channel and request types it accepts, so a server can offer named subsystems and refuse shell,
  exec, PTY, forwarding and agent requests.
- OpenSSH limits a connection to 10 session channels by default (`MaxSessions`). An embedded
  server sets its own limit.
- Swift: `swift-nio-ssh` implements SSH client and server on Apple platforms. Its private key type
  accepts Ed25519, ECDSA P-256, P-384 and P-521, and a P-256 key held in the Secure Enclave (Apple
  platforms only). It has no RSA support; a pull request adding RSA is open, not merged
  (checked 2026-10-31). It takes keys as ready-made key objects: it does not read OpenSSH key files
  or decrypt passphrase-protected ones, so the app needs its own reader (`app.md` 18.13). The
  Secure Enclave path works: see the spike result in 4.8.
- A jailbroken or instrumented device can bypass host-key pinning in its own app. That attacks the
  phone's owner only, not the hab.

---

## 2. Threat model and goals

| Threat | Goal |
|---|---|
| Someone on the same LAN, café Wi-Fi or tailnet connects to a hab | Refused unless their key is authorized on that hab |
| Someone on the path watches or changes traffic | Everything encrypted and integrity-checked by SSH; host keys pinned, so no CA can be abused |
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
| A. SSH everywhere: system sshd, then an embedded gff-hab SSH server | Clients and peers run `ssh <host> gff-hab connect` over the system sshd. Later, gff-hab can run its own SSH server, off by default, for the phone, with QR pairing | One credential type (SSH keys); proven crypto in Go and Swift; no new listener until the owner wants one; the embedded server removes hand-editing of `~/.ssh` and the 10-session cap | The embedded server is new network-facing security code; the phone needs an SSH library on iOS |
| B. TLS 1.3 listener with pinned self-signed device keys | A hab listener admits only paired device ids; QR pairing | Standard TLS in Go and Network.framework | Considered, not chosen: a second credential type beside SSH keys, and client certificates on iOS need extra work |
| C. Noise handshake with device keys | Noise XX or IK with pinned static keys | Small, no certificates | Considered, not chosen: we would own custom framing and more crypto code, with no Swift library to lean on |
| D. Tailnet identity (WhoIs) | The hab trusts the tailnet's view of the caller | No new keys | Ruled out by the owner: the tailnet would become part of the product |
| E. End-to-end encrypted relay (Happy) | Both sides connect outward to a relay | Works from anywhere with no inbound port | A hosted service in the path; contradicts "no hub" as a default. A later carrier at most |
| F. SSH-started fast carrier (Mosh pattern) | SSH starts the session and hands a key to a UDP or QUIC carrier | Survives roaming | A second protocol; the first build needs only reconnect. A later carrier at most |

Carriers are a separate axis from identity. Under A, the same SSH key check runs whether the bytes
arrive over the LAN, a tailnet, or a later carrier. WebSocket stays possible only as a later
carrier for a client that cannot speak SSH.

Decision: A (/consensus 3/3).

---

## 4. Recommendation

**SSH everywhere. System sshd first. Later, an SSH server embedded in gff-hab, off by default, for
the phone, with QR pairing and pinned host keys.**

### 4.1 First build

1. **Local socket, unchanged.** One Mac alone uses only the Unix socket. No keys, no pairing.
2. **SSH stdio over the system sshd.** `gff`, the Mac app off-host, and hab peers run
   `ssh <host> gff-hab connect`. The user's SSH keys authenticate them. sshd does the crypto.
3. **The welcome carries the hab's host-key fingerprint.** Each hab has its own SSH host key (its
   device id, see 5), separate from the system sshd's host key. The welcome carries its
   fingerprint. The caller pins it on first contact. A changed id is refused until the owner runs
   `gff-hab peers accept <label>`. The schema makes `device_id` optional, so an older hab may
   omit it. A caller that gets no `device_id` treats the hab as unpinned: it may use the
   connection, but it refuses to pair with that hab until a welcome carries `device_id`.

### 4.2 Next: the embedded gff-hab SSH server

4. **Off by default.** gff-hab can run an SSH server built on `golang.org/x/crypto/ssh`. The owner
   enables it per hab and names one bind address: loopback, one LAN address, or the tailnet
   address. Never all interfaces. A tailnet address is a bind address, not an identity.
5. **Narrow surface.** Public-key auth only, and only for keys admitted by pairing. It serves the
   API subsystems only (control and data). It refuses shell, exec, PTY, port forwarding and agent
   forwarding.
6. **Its own authorized-keys file** in gff-hab's state directory. It never reads or writes
   `~/.ssh`.
7. **Its own channel limit**, so one connection can carry one channel per visible pane without
   OpenSSH's 10-session cap.
8. **Its host key is the hab's device id** from 4.1, so a peer or client that pinned it over the
   system sshd already trusts the embedded server.

### 4.3 The phone

9. **Two kinds of key per device** (`app.md` 10.4):
   - **Generated (default):** a P-256 key in the Secure Enclave, used as an ordinary SSH key.
   - **Imported:** the owner's existing OpenSSH key (Ed25519 or ECDSA; not RSA, see 1.8),
     decrypted once with its passphrase on import, kept in the Keychain this device only, never in
     backups, never sent anywhere. Hosts that already trust it need no pairing.

   Both work only while the device is unlocked. Face ID inside the app is optional and off by
   default (`app.md` 18.2). An imported key trades the generated key's containment for reach: if
   the device is compromised, the key opens every host that trusts it, and revoking it means
   replacing it everywhere. The generated key stays the default.
10. **QR pairing** (6.1) admits the phone's key to one hab's authorized-keys file. After pairing,
    the phone connects with plain key auth.
11. **One QR per hab.** Introduction between habs stays an owner choice, off by default (6.2).

### 4.4 Per-pane data

- One SSH channel per visible pane on one connection (`hab-logic-api.md` 4.2, option A).
- SSH channel windows plus our credit frames give per-pane flow control.
- TCP head-of-line blocking is accepted. A QUIC carrier stays possible later.

### 4.5 Hab peers after pairing

After pairing, hab peers move from the system sshd to each other's embedded servers, where those
servers are enabled. The system sshd stays the bootstrap and the fallback. (Recommendation, 2/3.)

Open note (1/3): a host with no sshd could join by a one-line invitation, a pairing offer like 6.1
shown as text, instead of needing sshd for the first contact. Revisit when a real host lacks sshd.

### 4.6 Reasons

- One credential type everywhere: SSH keys. The owner already uses and likes them.
- It meets the owner rule: the product has its own security, and a tailnet is only a network.
- SSH stays first, and the first build opens no new port.
- The phone gets a real pairing flow instead of hand-editing authorized keys on every host.
- SSH is mature in Go and Swift. We write the admission policy, not the crypto.

### 4.7 Risks

- The embedded server is new network-facing security code. Fuzz its handshake and subsystem
  parsing. Rate-limit refusals. A fresh-eyes security review runs before any real host enables it.
- `swift-nio-ssh` with Secure Enclave keys is proven on macOS by the spike (4.8). It is not yet
  proven on an iPhone; the app's first device build checks it.

Decision: settled (/consensus 3/3, 2026-10-31).

### 4.8 Spike result: swift-nio-ssh with a Secure Enclave key

The spike is `swift/Spikes/SSHSpike` (task-iv89iwd8, 2026-11-01). It is a macOS command-line tool
built on `swift-nio-ssh` 0.15.1. `run.sh` starts a throwaway sshd on a loopback port with its own
host key and authorized-keys file, and a `gff-hab serve` with a temporary socket and state. It
never touches the system sshd or `~/.ssh`.

**Result: the path works.**

| Check | Result |
|---|---|
| Secure Enclave P-256 key (`ecdsa-sha2-nistp256`) | Works from an unsigned SwiftPM command-line tool on Apple silicon. No entitlement, no access-control flags, no prompt. A key made in one process and loaded from its `dataRepresentation` in another signs normally. OpenSSH 10.5 accepted it |
| Software P-256 key | Works the same way |
| `gff-hab connect` on an exec channel | `hab.hello` returns a welcome with revision, `client_id` and `device_id` |
| Two more channels at once on the same connection | Both open, and each gets its own welcome (distinct `client_id`). The first channel still answers `hab.ping` afterwards |
| A burst of 10 more channels (13 in all) | 7 open, 3 refused: OpenSSH's `MaxSessions` is 10. The refusal arrives as `NIOSSHError.channelSetupRejected` (reason 2). The connection and the open channels keep working |
| Host key pinning | The client accepts exactly one pinned host key. A different host key fails the handshake with a mismatch error before auth, and no channel opens |

Timing on an M1 Pro over loopback, Secure Enclave key, first connection of a fresh process:

| Step | Time |
|---|---|
| TCP connect | 1.9 ms |
| SSH handshake, auth and exec reply | 45.5 ms |
| `hab.hello` round trip (includes the remote shell and `gff-hab connect` starting) | 94.1 ms |
| Connect to first reply | 141.5 ms |
| Two more channels, open to both welcomes | 92.1 ms |

Later runs on the same machine under a load average near 200 ranged from 255 ms to 1.1 s. Most of
the time goes to starting the remote process, not to the crypto. Loading the Secure Enclave key
took about 3 ms. The cold-start time on an iPhone over a real network is still to measure
(task-iv89iwd8 acceptance).

**What `swift-nio-ssh` 0.15.1 supports:**

- Client and host keys: Ed25519, ECDSA P-256, P-384, P-521, and a Secure Enclave P-256 key. OpenSSH
  certificates for those types.
- Key exchange: `curve25519-sha256`, `ecdh-sha2-nistp256`, `nistp384`, `nistp521`.
- Ciphers: `aes128-gcm@openssh.com` and `aes256-gcm@openssh.com` only. MAC: `hmac-sha2-256`.
- The embedded gff-hab server (Go, `golang.org/x/crypto/ssh`) offers all of these, so the phone
  and the embedded server agree with no extra work.

**What it lacks, and who covers it:**

| Missing | Covered by |
|---|---|
| RSA keys | Not supported (1.8). Import refuses RSA with a clear message |
| Reading OpenSSH key files and passphrase decryption | Our own reader, task-1csglxtc |
| `known_hosts` parsing | Not needed: the app pins the hab's key from the pairing code (6.1). The spike parses one OpenSSH public-key line with `NIOSSHPublicKey(openSSHPublicKey:)` |
| `chacha20-poly1305@openssh.com` and `aes-ctr` | Not needed for our servers. A system sshd limited to those ciphers would refuse the app; OpenSSH's defaults include AES-GCM |
| Keepalive | The app sends `hab.ping` on the control connection (`hab-logic-api.md` 3.5) |
| A high-level client API | The app writes its own: connect, one channel per connection, exec or subsystem, line or frame reading. The spike's `openExec` and `LineChannel` are about 120 lines |

**What the app target needs:**

- Store the Secure Enclave key's `dataRepresentation` in the Keychain with
  `kSecAttrAccessibleWhenUnlockedThisDeviceOnly`, not synchronizable. The blob is wrapped by this
  device's enclave and useless elsewhere, but it stays out of backups.
- Create the key with `.privateKeyUsage` only. Face ID is one app setting for every SSH key,
  generated or imported (`app.md` 10.7): the app asks for Face ID before it signs, so turning the
  setting on or off keeps the key. A biometric `SecAccessControl` flag would tie the key to the
  setting and force a new key on each change.
- `SecureEnclave.isAvailable` is false in the simulator. The simulator and unit tests use a
  software P-256 key behind the same key protocol.
- CryptoKit Secure Enclave keys need no entitlement. A Keychain access group is needed only if a
  notification extension must sign with the same key.
- A channel refused for the session limit is an ordinary error. Against a system sshd the app opens
  a second SSH connection after nine panes (`hab-logic-api.md` 4.2). The embedded server sets its
  own limit.
- On the embedded server the app opens subsystem channels instead of exec, with
  `SSHChannelRequestEvent.SubsystemRequest`. The spike used exec because it ran against a system
  sshd.

Decision: settled. `swift-nio-ssh` is the app's SSH library.

**Follow-up: the carrier in the app (task-iv89iwd8, 2026-11-01).** The spike's path is now the
app's `NIOSSHTransport` (`app.md` 10.6). What the spike left open:

| Item | Result |
|---|---|
| Keychain storage | The key blob is a Keychain item, usable only when unlocked, this device only, not synchronisable. A locked screen refuses to make or use a Secure Enclave key (`errSecInteractionNotAllowed`), as designed |
| `Transport` protocol and fake | The carrier implements `Transport`. Its tests run against an in-process `swift-nio-ssh` server on loopback bridged to the fake hab, so pinning, refusal and the session limit are unit-tested |
| Live check on a Mac | `apple/scripts/ssh-live-check.sh`: a throwaway loopback sshd running a fresh `gff-hab connect` with a holder. Host key probe, changed key refused, one-shot ping 213 ms, connect to online (hello and subscribe) 271 ms, a live pane's output over a second channel on the same connection 120 ms. An imported passphrase-protected Ed25519 key (`app.md` 10.7): one-shot ping 136 ms. an Apple silicon laptop under load; software key, because the screen was locked |
| iOS build | The same path runs in the iOS simulator (software key, NIOPosix sockets) against the same sshd: process start to the first hab reply 552 to 625 ms over four cold starts, load average near 25 |
| iPhone device run | Not run yet. The phone was locked and the Mac had no iOS development signing (no Xcode account, no development profile for the bundle id). Cold start on a real network is still to measure |
| QR pairing | The code format is fixed (6.1) and parsed; the hab side (`devices.pair`, task-99pfhjlg) is not built, so the app stops after reading the code and offers the by-hand path |
| Background action | A local notification action runs one `hab.ping` over the carrier and disconnects (`app.md` 10.6). Not yet run on a device |

---

## 5. Device identity

| Item | Rule |
|---|---|
| Hab key | An SSH host key owned by gff-hab, made on first start, in its state directory with owner-only permissions. Not the system sshd's host key |
| Hab device id | The SHA-256 fingerprint of that host key, in the standard SSH form. Shown short in lists and full on request |
| Client key | Phone and iPad: P-256 in the Secure Enclave, this device only, never in backups; or an imported OpenSSH key in the Keychain, this device only, never in backups (4.3). Mac app: a key in the Keychain, made or imported. `gff`: the user's own SSH keys over the system sshd |
| Client device id | The SHA-256 fingerprint of the client's public key. An imported key used on several devices gives them one id, so a hab cannot tell them apart or revoke one alone |
| Device record | id, label, kind (hab, phone, desktop, gff), paired at, paired by, last seen, state (paired, revoked) |
| Trust store | One per hab: the embedded server's authorized-keys file (devices it admits). The peers a hab pins live in its peer file, `peers.toml` (`mesh.md` 3.2 and 4.4), not here. Revocations stay as tombstones |

A hab has one device id for both roles: as the embedded server's host key and as the id it shows
in the welcome over the system sshd.

Decision: proposed.

---

## 6. Flows

### 6.1 First pairing: a phone and a hab

1. The owner enables the embedded server on the hab (owner approval, see 8).
2. The owner runs `gff-hab pair` on the hab, or presses "Pair a device" in the Mac app.
3. The hab makes a pairing offer: its host-key fingerprint, its embedded server endpoints, and a
   one-time token (128 bits). The token expires after five minutes or one use.
4. The hab shows the offer as a QR code, and as text for a device with no camera.
5. The phone scans it. It opens SSH to an endpoint and pins the hab's host key from the code. A
   different host key fails the connection.
6. The phone presents the token and its public key (see the open note below).
7. The hab checks the token. It shows the phone's short fingerprint and label and asks the owner to
   confirm. Scanning proves the phone saw the screen; the confirm step stops a photo of the code
   from being enough.
8. The hab adds the phone's key to its authorized-keys file, burns the token, and logs the pairing.
9. The phone stores the hab: device id, label, endpoints. From then on it uses plain key auth,
   usable only while the device is unlocked.

If the embedded server is off, `gff-hab pair` says so and stops.

**The code.** The QR code, the pairing link and the text form carry the same string: the fields of
the API's `PairingOffer` (`devices.pair`) plus a label.

```
flightdeck://pair?v=1&id=SHA256:<43 chars>&ep=<host:port>&ep=...&t=<token>&l=<label>
```

| Key | Value |
|---|---|
| `v` | Format version, 1. A higher version tells the user to update the app |
| `id` | The hab's device id: the SHA-256 fingerprint of its host key, as `ssh-keygen -l` prints it |
| `ep` | One per endpoint, in the order to try: `host`, `host:port` or `[v6]:port`; the port defaults to 22. One to eight |
| `t` | The one-time token: 128 bits as unpadded base64url (22 characters) |
| `l` | The hab's label, optional |

Values are percent-encoded as in any URL query. Unknown keys are ignored, so a later hab can add
some. The app refuses a code that repeats `v`, `id`, `t` or `l`. It finds the link inside pasted
text. The iOS app opens `flightdeck:` links.

Built: the app's parser (`PairingCode`, task-iv89iwd8). The hab side follows it when
`devices.pair` is built (task-99pfhjlg).

Open note: how the phone presents the token is undecided (split 1/3 and 1/3). Either SSH
keyboard-interactive auth carries the token before any channel opens, or a pair request runs in a
pairing-only subsystem. The build stage picks one. Both refuse every other request until the owner
confirms.

### 6.2 The phone and the other habs

A phone paired with one hab still needs admission to each other hab. Two options:

- **Pair each hab.** Repeat 6.1 on each hab. Simple; one QR per host.
- **Introduction.** The first hab sends the phone's key to its peers over the authenticated
  hab-to-hab path. Each peer admits the phone only after the owner confirms on any client that is
  already paired with that peer.

Recommendation: pair each hab. Introduction is a trust-propagation choice for the owner, off by
default (see 8).

The phone may learn the other habs' device ids and endpoints from the first hab. Learning a host
key this way is safe: it came over a pinned, authenticated channel.

### 6.3 Hab to hab

1. A hab reaches a peer with `ssh <endpoint> gff-hab connect` over the system sshd, as Part 3
   stage 2 plans.
2. The welcome carries the peer's host-key fingerprint. The caller pins it in its peer file,
   `peers.toml` (`mesh.md` 4.4).
3. Where the peer's embedded server is enabled, the caller may move to it (4.5). It already knows
   the host key, so no QR code is needed. The peer admits the caller's hab key after the owner
   approves it once.
4. If an endpoint later presents a different id, the caller refuses it and says so, until
   `gff-hab peers accept <label>`.

### 6.4 The desktop client and `gff`

- On the same host, they use the local socket. No pairing.
- On another host, `gff` uses the system sshd with the user's SSH keys. The Mac app does the same,
  or pairs with the embedded server by the same flow as the phone.

### 6.5 Key rotation

- **A client rotates.** It makes a new key. Over a live, authenticated connection it sends a rotate
  request carrying the new public key, signed by the old key. Each hab replaces the entry and keeps
  the old fingerprint as a tombstone. The client repeats this on each hab it knows.
- **A hab rotates.** It sends its new host key to each connected client and peer, signed by the old
  key. They pin both for a grace period (seven days), then only the new one. A client that was
  offline for the whole grace period must pair again.
- No automatic schedule in the first build. Rotation runs when the owner asks.

### 6.6 Revocation

1. The owner runs `gff-hab devices revoke <id>` on any hab, or revokes from any paired client.
2. That hab marks the key revoked in its authorized-keys file and closes the device's live SSH
   connections at once.
3. It sends a revocation tombstone to every peer over the hab-to-hab path. Each peer revokes and
   closes too.
4. A peer that is offline gets the tombstone from its peers' outbox when it comes back (the outbox
   from Part 3 stage 5, `mesh.md` 6.3).
5. Revocation always wins. A hab accepts a revocation from any pinned peer. It never accepts an
   admission that way without the owner (see 6.2).
6. A revoked key that reconnects fails authentication, and the hab logs it as revoked.

---

## 7. Cases

### 7.1 One Mac alone

- The Mac app and `gff` talk to the local socket. Unix peer credentials authenticate them.
- No listener, no keys in use, no pairing, no network. The hab still makes its host key on first
  start, so pairing later needs no migration.

### 7.2 The iPhone with no system VPN

- On the home LAN: the phone reaches a hab whose embedded server binds a LAN address. Works with no
  VPN.
- On a tailnet with the system VPN on: the phone reaches an embedded server on the tailnet address.
  The SSH key check still applies.
- Away from home with no VPN: the first build shows the hab as offline. Later carriers, each its own
  owner decision: an embedded tailnet node in the app, or an end-to-end relay. A router port forward
  is possible but not recommended.
- Background notification actions: the device key, generated or imported, works only while the
  device is unlocked (4.3). iOS already asks for unlock on actions marked as needing
  authentication, so an approval from the lock screen fits this.

### 7.3 A lost phone

1. The owner revokes the phone from any hab or any other paired client (6.6).
2. Every hab closes the phone's live sessions and refuses its key from then on.
3. A generated key cannot be copied: it lives in the Secure Enclave and is excluded from backups.
   An imported key is in the Keychain, this device only, and also excluded from backups, but it
   was copied onto the phone in the first place, and other devices may hold it too.
4. Until revoked, a thief also needs to unlock the phone to use the key, because the key works only
   while the device is unlocked.
5. A new phone pairs from scratch. With generated keys there is nothing to restore: each device
   has its own key, so no shared secret was on the lost phone.
6. **An imported key on the lost phone** needs more. Revoking it on the habs is not enough: the
   owner removes it from every host that trusts it (each `authorized_keys` file, any forge or
   service account) and replaces it on every device that used it. Until then a thief who can unlock
   the phone can reach those hosts. The app's key list says which habs used it; it cannot know the
   other hosts.

### 7.4 A hab's disk dies

- Its host key is gone. A rebuilt hab makes a new key and pairs again.
- Peers see a different id at the old endpoint and refuse it until the owner accepts the new id
  (`gff-hab peers accept <label>`), which shows both ids.

### 7.5 The embedded server on a shared network

- Port scanners see an SSH server. Authentication fails for every key that is not paired.
- The hab logs each refusal with the presented key fingerprint and source address, rate-limits
  repeats, and opens no channel before authentication.

### 7.6 More than ten panes on one host

- Over the system sshd, OpenSSH's default limit of 10 sessions means a client showing more than nine
  panes opens a second SSH connection.
- Over the embedded server, its own channel limit applies, so one connection is enough.

---

## 8. Discuss-first for the owner

These follow the owner's hard stops: a new listening port, and any change to how the owner
authenticates.

| Item | Why it is discuss-first | Proposed default |
|---|---|---|
| Enabling the embedded SSH server on any hab | A new listening port | Off. The owner enables it per hab, after the security review |
| The bind address: LAN, tailnet address, or both | Decides who can reach the port | One address per hab, never all interfaces |
| The port number | A new port on each host | One fixed default in the 2700-9999 range, checked against ports in use, overridable by config |
| Device keys in gff-hab's authorized-keys file | A new credential that authenticates to a hab | Yes, for the embedded server only; the system sshd and `~/.ssh` unchanged |
| Introduction between habs (6.2) | Trust spreads from one hab to others | Off; pair each hab |
| Any off-LAN reach: a relay, an embedded tailnet node in the app, a router port forward | Reach beyond the LAN, or a new tailnet node that may need an ACL change | Later, each its own proposal; port forward not recommended |

Not discuss-first: the local socket, SSH stdio over the system sshd, the hab making its own host
key, and the welcome carrying its fingerprint. None opens a port or changes how the owner
authenticates.

---

## 9. Plan changes

Applied 2026-10-31 after the /consensus.

### 9.1 Part 3 stage 2, task-phkmrjaw (peer list, SSH transport, online and offline)

- Peers connect over the system sshd.
- Each hab makes its own host key on first start; the welcome carries its fingerprint.
- Each peer entry pins that fingerprint on first connection; a changed id is refused until
  `gff-hab peers accept`.
- Online and offline come from the connection; a tailnet's local status is optional, never required.

### 9.2 Part 3 stage 3, task-99pfhjlg (unshelved)

Rescoped as **"Embedded gff-hab SSH server with QR pairing (off by default, owner approval to
enable)"**:

- An SSH server in gff-hab, off by default, on one configured address.
- Public-key auth for paired keys only; API subsystems only; its own authorized-keys file; its own
  channel limit.
- `gff-hab pair`, `gff-hab devices list|revoke|rename`, rotation (6.5) and revocation fan-out (6.6).
- Fuzzing, rate-limited refusals, and a fresh-eyes security review before any real host enables it.
- No WhoIs authorization, no ACL work. A tailnet is a bind address only.

### 9.3 Part 6, task-iv89iwd8 (iOS transport)

- A spike first: `swift-nio-ssh` with a Secure Enclave P-256 key, against gff-hab's embedded server
  or a system sshd.
- A Secure Enclave SSH key; its fingerprint shown in Settings.
- Import of an existing OpenSSH key (Ed25519, ECDSA), with our own key-file reader and passphrase
  decryption (`app.md` 10.4, 18.13). Acceptance: a passphrase-protected Ed25519 key reaches a host
  over SSH.
- QR pairing (6.1) with a camera scanner and a text fallback; the hab's host key pinned.
- The `Transport` protocol keeps its unit-tested fake.

Status 2026-11-01: the spike (4.8), the carrier, the Secure Enclave key with its fingerprint in
Settings, the by-hand path (`app.md` 18.8), the code parser and scanner, and the background action
are built (`app.md` 10.6). Left: the iPhone device run with the cold-start time on a real network,
pairing itself once `devices.pair` exists (task-99pfhjlg), and key import (task-1csglxtc).

### 9.4 `hab-logic-api.md`

Section 4 is updated. The Part 2 stage that built the handshake added the section 3 items.

- **4.2 SSH stdio:** the client pins the welcome's `device_id` (done).
- **4.3:** now the embedded gff-hab SSH server; WebSocket only as a later carrier (done).
- **3.2 Handshake:** the welcome carries `device_id`, the hab's host-key fingerprint (done).
- **3.1 Protocol:** add a `devices.*` method group (done for `devices.pair`, `devices.list` and
  `devices.revoke`, which answer `unavailable` until pairing is built; rename and rotate are
  pending). Pairing is `devices.pair`, not `hab.pair`.
- **3.4 Capabilities:** add a capability string that says the embedded SSH server is on (done:
  `devices`).
