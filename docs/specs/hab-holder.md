# gff-hab-holder: behaviour spec

Status: draft, from the Part 1 study stage. Every question in section 7 is decided.

gff-hab-holder (the holder) is the small process on each host that owns every PTY. It spawns
programs, pumps their output into a raw byte ring per pane, resizes, signals and reaps. It parses
nothing. gff-hab (the logic process) parses screens, keeps checkpoints, runs harness adapters and
serves the API. gff-hab restarts at any time. The holder almost never restarts, and upgrades by
drain.

This spec states behaviour in our own words. No code from the reference projects is copied here,
and none may be copied into the implementation.

Contents:

1. [References](#1-references)
2. [Behaviour](#2-behaviour)
3. [Protocol needs](#3-protocol-needs)
4. [Upgrade](#4-upgrade)
5. [Service install](#5-service-install)
6. [Edge cases](#6-edge-cases)
7. [Open questions](#7-open-questions)
8. [Stage map](#8-stage-map)

---

## 1. References

### 1.1 Summary

| Tool | Who owns the PTYs | Screen state in that process | What a restart or upgrade does to programs |
|---|---|---|---|
| dtach, abduco | One process per session | None | Upgrade is impossible without ending the session |
| shpool | One daemon for all sessions | An in-memory VT render | Daemon restart ends every session; one client per session |
| zmx | One daemon per session | libghostty-vt, with scrollback | Old daemons keep the old build; an IPC change between builds ends sessions |
| tmux | One server for all sessions | Full grid | Server restart ends everything |
| zellij (and the fork) | One server per session | Full grid, layout, plugins | Server restart ends programs; the fork restores layout only |
| Rex | One Go server per account, PTYs in-process | Full, libghostty per terminal | `server stop` ends every program; a personal server keeps the old build until its sessions end |
| herdr-core | A holder process, separate from the core | None in the holder; the core parses | Core restarts freely; a new holder build drains the old one |

### 1.2 herdr-core holder (closest match)

herdr-core splits a host into a holder and a core. Behaviour observed in its `holder.rs` and
`holder_proto.rs`:

- **Spawn.** The core picks a terminal id and sends argv, env, cwd and size. A repeated spawn for a
  live id answers with the existing pid instead of starting a second program.
- **Pump.** One reader per PTY reads all the time, connected core or not. Each read lands in a
  per-terminal tail of about 1 MB and goes to the core as an output frame.
- **Backpressure.** Frames to the core go through a writer of their own. When the core falls more
  than about 8 MB behind, readers stop reading. Programs then slow down, as with a PTY nobody reads.
- **Input.** Each terminal has its own input writer, so a program that stops reading its input
  blocks only itself.
- **Resize** sets the PTY window size. **Signal** goes to the child pid, and never after the
  child was reaped, because the pid may belong to another process by then.
- **Foreground query.** The core can ask which process group holds the terminal.
- **Reap.** A waiter per child collects the exit status. The holder reports the exit only after
  the reader hit end of file, or after a short drain window (about 200 ms) once output can flow.
  The exit therefore follows the last output byte.
- **Release.** The core forgets a terminal. If background jobs still hold the PTY, the holder
  hangs them up.
- **Leftover sweep.** Each spawn may carry a scope token in its environment. When the program
  exits, the holder ends every process that still carries the token.
- **One core at a time.** The newest hello owns the holder. An older connection still open is
  treated as a crash.
- **Welcome.** It carries the protocol version, a random instance id, the holder pid, the build
  id, how the last core left (none, handover or crash), and every live terminal with pid, size and
  exit state. A tail frame per terminal follows.
- **Handover.** A core that restarts on purpose saves its parsed screens, then says goodbye with
  mode handover. The holder clears every tail, so the next core replays only later bytes.
- **Crash.** The next core gets the full tail and rebuilds each screen from it.
- **Supervision.** If no core comes back within a grace period (3 s by default), the holder
  starts one, with exponential backoff capped at one minute. A holder with no terminals and no core
  exits.
- **Drain.** A core that sees a holder of another build starts a new holder for new terminals and
  sends drain to the old one. A drained holder keeps serving its terminals and exits after the last
  one ends. Its spawn path does not check the drain flag; the core simply stops sending spawns
  there. Each holder has its own socket.
- **Wire.** A frame is a kind byte, a terminal id, a length and a payload. Control payloads are
  JSON; terminal bytes are raw.

Two weak points we fix (section 2.6):

- Clearing the tail at handover loses any bytes that were queued to the core but not yet parsed
  when it saved. Output frames carry no offset, so the core cannot tell.
- A core that is alive but hung stalls every program, because backpressure has no time limit.

### 1.3 Zellij fork server

- One server per session owns PTYs, grid, layout and plugins. A server restart or upgrade ends
  programs. The fork's snapshot archive and resurrect restore layout and resume hints only.
- macOS lessons (FORK.md, "session up will not create a session in the wrong macOS session
  domain" and "macOS decides about file access at server start"):
  - Only the graphical (`Aqua`) session domain carries TCC context, the login keychain, the
    pasteboard and notifications. A process inherits its domain at creation and never changes it.
    A process started over SSH lands in the `Background` domain.
  - The fork's launch agent is bootstrapped into `gui/<uid>` and sets
    `LimitLoadToSessionType Aqua`.
  - macOS charges a pane's file access to the **responsible process**: the server binary, not the
    shell. TCC keys a plain binary on its absolute path. A package manager's versioned path is a new
    client at every upgrade, so grants vanish and a dismissed prompt becomes a silent, permanent
    denial. The fork answers with a pinned copy at a stable path.
- Generated units name a stable absolute binary path, set `TERM`, `PATH` and a working directory,
  and route launchd output to log files (launchd discards it otherwise).

### 1.4 Rex

- One Go process owns the PTYs, with a libghostty terminal per block. There is no holder split.
- `rex server stop` ends every program. `--restore` reloads saved sessions, but not their programs.
- A personal or account server keeps running the old build until its sessions end. That is drain
  at the level of the whole server.
- Rex ships its server as a helper bundle inside the app. That is relevant to section 5.3.

### 1.5 zmx, shpool, dtach

- **zmx.** One daemon and one socket per session. The daemon feeds output both to clients and to a
  libghostty-vt instance, which answers a snapshot on reattach. Its README lists a known issue: a
  build that changes the IPC ends existing sessions.
- **shpool.** One daemon for all sessions, with a VT render for reattach and one client per
  session. Its systemd user unit uses socket activation, `KillMode=mixed` and `SendSIGHUP=yes`, so
  stopping the unit ends every shell. Its README asks for `loginctl enable-linger`. It advises bash
  users to set `huponexit`, or background jobs pile up in the daemon's process tree.
- **dtach.** One process per session holds the PTY master and nothing else. A client redraws by
  sending a window-size change or Ctrl+L. Nothing survives replacing the process.

---

## 2. Behaviour

### 2.1 Fixed choices

| Choice | Value | Why |
|---|---|---|
| Parsing | None. The holder never interprets output bytes | A parser bug or upgrade would force a holder restart, and a holder restart ends programs (2.7) |
| Language | Pure Go, no cgo | Builds on macOS and Linux with no toolchain beyond Go |
| Screen checkpoints | Live in gff-hab, not here | Same reason as parsing |
| Ring | 4 MiB per pane by default, allocated as it fills; set per holder at start | See 2.4 |
| Upgrade | Drain first; re-exec in place (option C) held in reserve | See section 4 |
| Logic connections | One owner at a time; the newest hello wins | Matches herdr-core; avoids two writers |
| Supervising gff-hab | The init system does it, not the holder | Several holders may run at once (drain); only one should own supervision. See 7, Q3 |

### 2.2 Spawn

- gff-hab sends: pane uuid (the stable pane id used everywhere in GFF), argv, the complete
  environment, cwd, and size. The holder adds nothing to the environment.
- The holder opens a PTY pair and starts the child in a new session, with the PTY as its
  controlling terminal.
- Every descriptor the holder owns is close-on-exec. A child must never inherit another pane's
  PTY master or the holder's socket; an inherited master would keep a dead pane alive.
- The holder assigns a small numeric handle for frames. The pair (holder instance, handle) is
  unique on the host; the pane uuid is unique everywhere.
- A spawn for a uuid the holder already runs answers with the existing pane. This makes a retry
  after a lost reply harmless.
- A spawn on a draining holder is refused with a reason (4.3).
- Failure to start the program answers with an error frame. No pane is created.

### 2.3 Pump

- One reader per PTY reads all the time, whether or not gff-hab is connected.
- Each read appends to the pane's ring and advances the pane's **byte count**: a 64-bit offset
  from spawn, which never resets and never wraps in practice.
- When gff-hab is connected, each read is also sent as an output frame that carries its starting
  offset. gff-hab can then detect a gap or a duplicate exactly.

### 2.4 Ring

- The ring keeps the most recent bytes of each pane, up to its size, plus two numbers: the total
  byte count and the offset of the oldest byte kept.
- A reader asks for "bytes from offset N". If N is older than the oldest byte kept, the answer
  starts at the oldest byte and says that a gap occurred.
- The holder never clears a ring on handover (compare 1.2). Memory does not shrink by clearing,
  and the bytes stay useful for a later crash rebuild and for command byte ranges.
- Size rationale. gff-hab rebuilds a screen from its last checkpoint plus the ring bytes after
  that checkpoint's offset. The ring must therefore outlast several checkpoint intervals.
  Part 2 is asked to checkpoint each busy pane at least every 10 s or every 1 MiB of output,
  whichever comes first. A 4 MiB ring then holds at least four intervals. herdr-core's 1 MB is
  tight for agent TUIs, which redraw heavily.
- The holder reports its ring size in the welcome, so gff-hab can adapt its checkpoint rate.

### 2.5 Input, resize, signal, foreground

- **Input.** Each pane has its own bounded input queue (1 MiB) and writer. A child that stops
  reading blocks only its own queue. When the queue is full, the holder refuses further input for
  that pane with an error frame rather than block anything else.
- **Resize.** The holder sets the PTY window size and records it. The kernel sends SIGWINCH to the
  foreground group only when the size changes. Redraw nudges (cols−1, then back) are gff-hab
  policy; the holder applies sizes as told.
- **Signal.** Targets are the child (the session leader) or the PTY's foreground process group.
  Allowed signals: HUP, INT, QUIT, TERM, KILL, WINCH, CONT, STOP, USR1, USR2. The holder never
  signals a child it has already reaped, nor a group it cannot confirm is still the pane's.
- **Foreground.** On request the holder reports the PTY's foreground process group id. It does
  not look up process names; gff-hab does that from the OS, for agent detection and status.

### 2.6 Logic connection: hello, handover, crash

- **Hello.** gff-hab offers a protocol version range and its build id. The holder picks the
  highest version both speak, or refuses with its own range (3.2).
- **Welcome.** The holder answers with the chosen version, its instance id, pid, build id, ring
  size, whether it is draining, how the last logic process left (none, handover or crash), and every
  pane: uuid, handle, pid, size, byte count, oldest kept offset, and exit status if any. No bytes
  follow on their own.
- **Replay.** gff-hab asks for each pane's bytes from the offset its checkpoint covers. Each
  replay ends at the pane's byte count when the request arrives, so it ends however fast the
  program writes. Live output frames resume from the end of each replay and queue behind it. The
  holder delivers the replay and live frames in offset order, so nothing is lost or doubled.
- **Handover.** gff-hab saves its screens and their offsets, then sends handover and closes. The
  holder records "handover" for the next welcome. It discards nothing. A write that fails
  because gff-hab already closed does not turn this into a crash: the holder stops writing,
  reads what gff-hab sent before it closed, and records "handover" when that includes the
  handover frame. If it cannot read that within the hang timeout, it records "crash".
- **Crash.** The connection ends without handover, or a newer hello arrives while a connection
  that has not sent handover is open. The holder records "crash". A newer hello that replaces a
  connection which already sent handover records "handover". The next gff-hab replays from its
  last checkpoint on disk; the only difference from a handover is how fresh that checkpoint is.

  This is a deliberate change from herdr-core: the holder holds no state about the logic
  process's saves. The logic process names the offset it wants.
- **Backpressure.** The holder queues frames for gff-hab through one writer. When more than 8 MiB
  is queued, readers pause, and programs slow down. When the writer makes no progress for 5 s
  (not one byte reaches the socket while frames are queued; found within 5 to 6 s), the
  connection is hung: the holder drops it, records "crash", and resumes pumping into the rings
  alone. A hung logic process
  therefore cannot stall programs for long. A slow but moving reader is not hung.
- **Ping.** Either side may ping; the other answers. gff-hab uses it to detect a hung holder.

### 2.7 Reap and exit

- A waiter per child collects the exit status: an exit code, or the signal that ended it.
- The exit frame goes out only after the pane's reader reached end of file, or 200 ms after the
  child exited and output could flow, whichever comes first. A reader paused by backpressure does
  not use up that window. The exit frame carries the final byte count, so gff-hab knows when it
  holds all output.
- The pane stays in the holder, with its ring and exit status, until gff-hab releases it. A new
  logic process can still read the last output of a program that ended while no logic ran.
- **Release.** gff-hab drops a pane. If the reader has not ended, other processes still hold the
  PTY. The holder sends SIGHUP to the pane's session and closes the master. If the child itself has
  not exited yet, the holder keeps its waiter, so the child is still reaped later.

### 2.8 Shutdown

- An explicit shutdown (a shutdown frame, or SIGTERM from the init system) means: hang up every
  pane, wait up to 5 s for children, send SIGKILL to what remains, reap, remove the socket and
  registry entry, exit.
- "Every pane" includes released panes whose programs still run. SIGKILL goes to each pane's
  process group too, also after its leader was reaped, while the group still exists (a
  background job that ignores SIGHUP).
- Shutdown is the only path on which the holder ends programs on purpose.

### 2.9 What the holder does not do

- Parse output, track screens, answer terminal queries, or scan for escape sequences (but see
  7, Q1).
- Read harness transcripts, track layout, or decide sizes.
- Sweep processes that escaped a pane (for example a daemon that called `setsid`). That is
  gff-hab policy (7, Q4).
- Start or restart gff-hab (7, Q3).
- Keep timers beyond the backpressure limit and the exit window. A laptop that sleeps simply
  resumes; nothing is owed on wake.

### 2.10 Local socket and access

- Each holder listens on its own Unix socket in a per-user runtime directory with mode `0700`;
  the socket is `0600`. The holder refuses a socket directory that is a symlink, belongs to
  another user or is open to others, and one whose parent lets other users rename it (a
  writable parent must be sticky). It never falls back to a shared directory such as `/tmp`.
- The holder checks the peer's uid on every connection (peer credentials) and refuses any other
  user.
- Socket paths stay short: macOS limits a socket path to 104 bytes.

---

## 3. Protocol needs

Stage 2 designs the encoding. This section states what the protocol must carry.
The encoding is in [`docs/protocol/holder.md`](../protocol/holder.md).

### 3.1 Frames

| Direction | Frame | Carries |
|---|---|---|
| logic → holder | hello | protocol version range, build id |
| holder → logic | welcome | chosen version, instance id, pid, build id, ring size, draining flag, previous exit (none, handover, crash), pane list (2.6) |
| holder → logic | refuse | the holder's version range, a reason |
| logic → holder | spawn | pane uuid, argv, env, cwd, size |
| holder → logic | spawned / spawn failed | handle and pid, or an error |
| logic → holder | input | handle, input offset, bytes |
| holder → logic | input refused | handle, reason (queue full, wrong offset, pane gone), the input offset expected |
| holder → logic | output | handle, start offset, bytes |
| logic → holder | replay | handle, from offset, optional to offset |
| holder → logic | replay data | handle, offset, bytes; then a replay end with the actual start offset, end offset and gap flag |
| logic → holder | resize | handle, cols, rows |
| logic → holder | signal | handle, target (child or foreground group), signal |
| logic → holder | foreground query | handle |
| holder → logic | foreground | handle, process group id, or none |
| holder → logic | exited | handle, code or signal (in protocol numbers), final byte count |
| logic → holder | release | handle |
| logic → holder | handover | (nothing) |
| logic → holder | drain | (nothing) |
| logic → holder | shutdown | (nothing) |
| holder → logic | bye | (nothing; last frame before the holder closes) |
| either | ping / pong | a token |

The optional upper bound on replay lets gff-hab fetch one command's byte range (7, Q1).

### 3.2 Versioning

- The protocol version is an integer, separate from the build id.
- gff-hab offers a range; the holder picks the highest common version. With no common version
  the holder refuses, and gff-hab starts a holder of its own build (section 4).
- A receiver skips a frame kind it does not know. Unknown fields in a payload are ignored.
- gff-hab must speak every protocol version that a live holder on the host may still use. A
  drained holder can live for weeks, so gff-hab keeps old versions until no holder of that
  version can remain.

---

## 4. Upgrade

### 4.1 Options

| Option | How it works | Fit |
|---|---|---|
| A. Drain (herdr-core) | A new holder build starts beside the old one and takes new panes. The old one gets drain and exits after its last pane ends | **Chosen.** No state handoff; a bad new build hurts only new panes |
| C. Re-exec in place | The holder execs its new binary, keeping PTY masters open across exec, and hands its state to itself | **Reserve.** For a security fix that cannot wait. Same pid, so children keep their parent. One bad build breaks every session at once |
| D. Pass PTY masters to a new holder over the socket | The new process receives the descriptors | Rejected. Children stay children of the old pid, so the old process must live on as reaper anyway |
| E. Linux fd store | systemd keeps PTY masters across a holder restart | Reserve, Linux only (6.6). Children are reparented, so exit codes need extra work |

### 4.2 Holder registry: how gff-hab finds every holder

- Holders live under one per-user directory: `$XDG_RUNTIME_DIR/gff/holders/` on Linux, and a
  `~/Library/Application Support/gff/holders/` on macOS (not Caches, which storage cleanup may
  empty). The path comes from
  configuration, never from a baked-in name.
- Each holder writes one small file, `<instance>.json`, with instance id, build id, protocol
  version range, pid, socket path, start time and draining flag. It writes the file atomically
  (write, then rename) and removes it on exit. Its socket is `<instance>.sock` beside it.
- One file per holder, rather than one shared registry file, avoids lock races between holders
  that start and exit independently.
- A holder writes the file after its socket listens, so a listed holder accepts connections. It
  rewrites the file when it starts draining, before it can exit. The temporary file is
  `.tmp-<instance>-<random>`; readers skip dot files.
- gff-hab lists the directory at start and on change. A file whose pid is gone, or whose socket
  refuses connections, is stale; gff-hab removes it. The pid alone is not enough, because an
  unrelated process may reuse it, so a live pid with a refusing socket is stale too. When it
  removes a stale file, gff-hab also removes the socket file beside it, unless something listens
  there again. It removes a file only while the file still names the same pid and start time, so
  a holder restarted under the same instance id keeps its new file. To close the window between
  that check and the removal, it first renames the file aside, checks what it took, and puts a
  newer holder's file back. It removes a socket the same way: renamed aside, probed at the new
  name, and put back if something answers. Temporary files and unreadable registry files older
  than one minute are left over from a crash and removed on every scan.
- A holder checks its own file every 10 s and writes it again if it is gone, so a prune that
  raced its restart cannot hide it from gff-hab for good.
- gff-hab trusts a registry file only as far as it can check it. The directory must pass the
  holder's own socket-directory check (2.10). The socket must be `<instance>.sock` in that
  directory, a real socket file (not a symlink) of this user, and not the same file as a socket
  gff-hab is already connected to. The peer must run as this user, and its welcome must name the
  file's instance and pid. A file that fails is skipped, not pruned. Without these checks a file
  could point at another holder's socket, and two connections to that holder would replace each
  other by hello takeover (6.8).
- A holder removes only its own file, and only while it still names its own pid and start time.
- gff-hab keeps one connection per live holder, each at that holder's protocol version. Status
  shows which holder (build) each pane lives on.

### 4.3 Drain lifecycle

1. A new gff-hab build starts. It finds holder H1 of another build, or an older holder of its own
   build (a start that was retried leaves two); either is drained.
2. gff-hab connects to H1 if a common protocol version exists, and keeps serving H1's panes.
3. gff-hab starts holder H2 of its own build through the init system (5.4) and waits for H2's
   registry file and socket.
4. gff-hab sends drain to H1. H1 sets its draining flag, rewrites its registry file, refuses new
   spawns, and keeps serving its panes.
5. New panes go to H2.
6. When H1's last pane is released, and every released pane's program has been reaped, H1
   removes its socket and registry file and exits with status 0. A released program that
   ignores the hang-up is killed, with its process group, ShutdownGrace (5 s) after the last
   release (6.1). The init system does not restart a clean exit. A pane whose program has ended still counts
   until gff-hab releases it, so gff-hab releases each ended pane on a draining holder once it
   holds the pane's last output.
7. gff-hab removes H1's job definition (5.4) and, if no other holder uses it, H1's binary copy.

Two or more builds can coexist for weeks. A third build simply adds H3 and drains H2 as well.

If gff-hab and H1 share no protocol version, gff-hab cannot reach H1's panes. It reports them as
unreachable and keeps H1 alone. The compatibility rule in 3.2 exists to prevent this.

### 4.4 Binary copies

- A running drained holder keeps its executable path. The install keeps each holder build's
  binary in its own directory under the GFF state directory until that build's last holder exits.
  A package manager that deletes the old version's files cannot break a running holder's job
  definition or a later re-exec.
- On macOS this interacts with permission grants; see 5.3.
- Built on macOS (stage 6): per-build copies. `gff-hab install` copies the two helper bundles
  into `<state>/builds/<build>` (`macos-identity.md` 6.1). Each holder job names the executable
  in its own build's bundle, so a holder that restarts after a crash runs its own build. gff-hab
  removes a build directory once no job names it: at install, after it retires a holder, and at
  its start.
- Built on Linux (stage 6), unchanged: `gff-hab install` copies both binaries into `<state>/bin`
  by write and rename, never in place, so a running holder keeps its own executable file. Every
  holder unit names that one path, so a holder that restarts after a crash runs the newest
  build. Linux has no grants to keep, so per-build directories there matter only for a re-exec
  (option C); they are not built yet.

---

## 5. Service install

### 5.1 Jobs

| Job | Restart policy | Notes |
|---|---|---|
| gff-hab (logic) | Always restart, with a short delay | Restarts freely; owns no programs |
| gff-hab-holder, one job per holder instance | Restart only on failure | A clean exit 0 after drain stays down |

The holder must never live inside the logic job: an init system that stops the logic job would
end its children. gff-hab therefore asks the init system to start each holder as its own job.

### 5.2 Linux: systemd user units

- Unit names carry the configured prefix (5.4): `<prefix>-gff-hab.service` and
  `<prefix>-gff-hab-holder@.service`. The default prefix is `flightdeck`.
- `<prefix>-gff-hab.service`: the logic process. `Restart=always`, `RestartSec=1`, and
  `StartLimitIntervalSec=0`: systemd's default start limit (5 starts in 10 s) would refuse a
  sixth restart by hand, and the logic job restarts freely.
- `<prefix>-gff-hab-holder@.service`: a template; the instance name is the holder instance id.
  `Restart=on-failure`. `KillMode=process`, so a stop or a crash restart signals only the holder
  itself and never its children directly; the holder's own shutdown path (2.8) ends programs when
  that is intended. `KillMode=process` also keeps option E possible.
- Lingering: `loginctl enable-linger` keeps the user manager, and so both units, running without a
  login. The install command checks `loginctl show-user` for `Linger=yes` and prints the command
  when it is off. Some distributions need administrator approval for it.
- A unit gets a bare environment. gff-hab passes each pane's full environment in spawn (2.2), so
  the holder unit needs only `PATH` for itself. Install writes the installing user's `PATH` into
  both units.
- Built (stage 6): install also writes the installing user's locale (`LANG` and every `LC_*`)
  into both units and both plists. gff-hab builds each pane's environment from its own, then the
  request's entries on top. Its own is changed first: `TERM=xterm-256color` and
  `COLORTERM=truecolor` always, in every init mode including `-init none`, because the pane's
  terminal is gff-hab's emulator, not the terminal gff-hab started in (coordinator ruling); a
  `session.create` env that sets `TERM` still wins. Then a UTF-8 `LANG` when no `LANG`,
  `LC_ALL` or `LC_CTYPE` is set (`C.UTF-8` on Linux, `en_US.UTF-8` on macOS), and the job's own variables are dropped
  (`XPC_SERVICE_NAME`, `INVOCATION_ID`, `JOURNAL_STREAM`, `SYSTEMD_EXEC_PID`).
- Follow-up: start the default shell (no argv given) as a login shell, so the user's profile
  builds `PATH` again instead of the `PATH` frozen at install.

### 5.3 macOS: launchd agents in the GUI domain

**Domain.** Both jobs are launch agents bootstrapped into `gui/<uid>`, with
`LimitLoadToSessionType Aqua`. Only that domain carries TCC context (Files and Folders, Full Disk
Access), the login keychain, the pasteboard and notifications. Children inherit the domain at
creation. The `user/<uid>` domain and anything started over SSH run as `Background`, and their
programs can never reach those resources.

- With no graphical login there is no `gui/<uid>` domain. gff-hab then refuses to start a holder
  and says why. An explicit option may start a `Background` holder, flagged in status as having no
  GUI grants. A Mac that must recover unattended after a reboot needs automatic login.
- A job's output goes to log files under the GFF state directory; launchd discards it otherwise.
  The install creates that directory first.
- The plist names a stable absolute binary path and sets a working directory.
- Built (stage 6): the logic plist has `KeepAlive` true and `ThrottleInterval` 1. Each holder
  plist has `KeepAlive` with `SuccessfulExit` false, `AbandonProcessGroup` true (launchd signals
  only the holder when the job ends) and `ExitTimeOut` 30 (above the 5 s grace of 2.8). Logs go
  to `<state>/logs/gff-hab.log` and `<state>/logs/gff-hab-holder.<instance>.log`.

**Responsible process.** macOS charges a pane's protected access to the holder: launchd starts
the holder, so the holder is the responsible process for every program in its panes. Two
consequences:

- The holder, not gff-hab, must be the job launchd starts. A holder spawned by gff-hab would make
  gff-hab's binary responsible, and gff-hab's binary changes at every update.
- Drain runs two holder builds at once, from two paths. If TCC keys the holder by path, every
  holder build re-asks for every grant (1.3). See the decision below.

**Decision: option 1, signed helper bundles (coordinator), pending Mac proof P3 — how the holder
keeps one TCC identity across builds.**

| Option | How | For | Against |
|---|---|---|---|
| 1. Signed helper bundle | Ship the holder as a minimal app bundle with a fixed bundle identifier, signed with the same Developer ID team at every release. TCC then keys the grant on the bundle identifier and code requirement, not the path | Grants survive every build and every path; two builds share them. Rex ships its server as a helper bundle | Needs signing in every release build, including local builds that should keep grants. Must be verified on current macOS before relying on it |
| 2. Pinned path, replaced between builds | One stable path for the newest holder; older builds run from copies | Simple, no signing | Older drained holders run from other paths; any new grant they need re-prompts. The pinned binary changes content at every upgrade, and TCC may reject the changed code for a path client |
| 3. Disclaim responsibility per child | The holder starts each child as responsible for itself | Grants follow each program, as in a terminal app | Needs private, undocumented macOS spawn attributes; not reachable from pure Go without cgo |

The holder ships as a helper bundle with the fixed bundle id `<prefix>.hab-holder`, and gff-hab
as one with `<prefix>.hab`, both signed in CI with Developer ID, hardened runtime and a constant
identifier, then notarized and stapled. Each holder build runs from its own bundle copy under the
state directory (4.4); TCC keys bundled code on its bundle id and requirement, not its path, so a
drained older build keeps every grant. Local dev builds use `.dev` bundle ids and hold grants only
until their next rebuild. Path pinning (option 2) is dropped, because drain runs builds from
different paths. Grants per process, the signing pipeline, `gff setup` and the Mac proof steps P0
to P12 are in `macos-identity.md`; stage 6 runs P0, P2 and P3 before stage 5 relies on this.

**The permission check stage 6 runs**, from a program inside a holder pane:

1. `launchctl managername` prints `Aqua` (it prints `Background` over SSH). Passed (P0,
   `macos-identity.md` 8.1).
2. A pasteboard round trip works: `pbcopy` then `pbpaste` returns the same text. Passed (P2).
3. After granting Desktop access once, listing `~/Desktop` succeeds, then still succeeds from a
   pane of a second holder build started by drain (tests option 1).

### 5.4 Starting a holder

| Init system | Start | Remove after drain |
|---|---|---|
| systemd | `systemctl --user start gff-hab-holder@<instance>` | Nothing to remove; the instance simply stays stopped |
| launchd | Write a per-instance plist (label `<prefix>.gff-hab-holder.<instance>`) to the user's LaunchAgents directory, then `launchctl bootstrap gui/<uid> <plist>` | `launchctl bootout` the label after the holder has exited, then delete the plist. Never boot out a running holder: that ends it |

The label prefix comes from configuration. One launchd job runs one process, so each holder
instance needs its own label.

Built (stage 6):

- gff-hab picks the instance id and asks for the job. It starts a holder whenever none of its
  own build is connected: at start, at each registry scan, and before a spawn. A failed start is
  retried by scans after 30 s, and at once by a spawn. A new build therefore starts its holder
  and drains the older ones as soon as it runs (4.3).
- When a holder says bye, gff-hab waits until its pid is gone or launchd runs no process for its
  job (a live pid launchd does not run for the job is another process that reused the pid), then
  boots out its launchd job and deletes its plist and its log. A holder still running after a
  minute keeps its job. On systemd there is nothing to remove.
- A holder can still leave its plist: it exits while gff-hab is down, or gff-hab restarts during
  that minute. launchd would start a holder from each such plist at every login. So at start, with
  `-init launchd`, gff-hab sweeps: it removes each holder plist (and log) whose instance has no
  live registry entry and whose job launchd runs no process for, and each holder log with no
  plist.
- Running with no init system stays possible: `gff-hab serve -init none` (the default) starts no
  holders; they are started by hand or by a test.

### 5.5 Install and uninstall

- `gff-hab install` writes the units or plists from templates, with the binary paths, state
  directory and label prefix filled in from the running user's environment. No user name or host
  name is baked in.
- `gff-hab uninstall` stops the logic job, sends shutdown to every holder (after confirmation,
  because it ends programs), boots out or disables every job, and removes every file install
  wrote, per-instance holder plists included.

Built (stage 6):

- The settings come from flags, else the environment, else defaults: `-prefix`
  (`$GFF_SERVICE_PREFIX`, default `flightdeck`), `-state`, `-holders`, `-socket` and `-unit-dir`.
  `gff-hab serve` takes the same flags, and the jobs pass them on, so both sides agree.
- Install writes: the job files (two units, or the logic plist), the enable link that
  `systemctl --user enable` makes in `default.target.wants`, the binary copies in `<state>/bin`
  (Linux), and on macOS the log directory, the build directory `<state>/builds/<build>` with both
  helper bundles, and the link `<state>/current` (`macos-identity.md` 6.1). A second install
  replaces the binaries (Linux) or adds a build directory (macOS), replaces the job files and
  restarts the logic job only. `gff-hab install -dry-run` prints the job files.
- On macOS install takes only bundles: `-hab-bin` is `GFF Hab.app` or the executable in it, and
  `-holder-bin` defaults to the `GFF Holder.app` beside it. Bare binaries are refused with a
  pointer to `scripts/macos-bundle.sh -dev`, because a holder outside a bundle has no stable
  privacy identity.
- Install records each file and each directory it creates in `<state>/install.json` before it
  writes it. A binary whose source is already the installed copy is not written, so not recorded.
  Uninstall removes what the record lists, the holder plists and holder logs by their names, every
  build directory in `<state>/builds` by its name (16 hex digits, or a leftover temporary
  directory), the record, then each directory install created, only when it is empty. So `-state ~` never costs
  the user `~/logs` or a `~/bin/gff-hab` install did not write. With no record it removes the
  job files by name only, and says so.
- Install and uninstall refuse a state directory whose record names another prefix, init system
  or unit directory, and change nothing: two logic jobs on one state directory would fight for
  its lock, and one would restart forever. The message names the uninstall to run first.
- On Linux, when `systemctl --user disable --now` fails, uninstall checks `is-active` and stops
  if the logic unit still runs, before it stops any holder.
- On Linux, install checks `loginctl show-user <uid> --property=Linger` and prints
  `loginctl enable-linger` when it is off. It never runs sudo.
- On macOS, install and each holder start refuse when `launchctl print gui/<uid>` fails (Q5).
- **The socket folder.** gff-hab refuses a socket folder other users can open (4.1 of
  `hab-logic-api.md`). A folder made for `hab.toml` with a plain `mkdir` is 0755, and the logic
  job would exit and be restarted every second. So install makes a missing socket folder with mode
  0700 and records it. It sets an existing folder to 0700, and says so, when it is the default
  socket's folder or one an earlier install made. A folder the user named (`-socket` or
  `$GFF_HAB_SOCKET`) that install did not make is never changed: install fails before it writes
  anything, with the `chmod 700` that fixes it. A folder the user made stays at uninstall.
- **gff-hab waits for a private socket folder.** At start, before anything else runs, gff-hab
  checks the folders of its two sockets. While one is open to other users it logs one line, with
  the folder, its mode and the `chmod 700` to run, then checks again every 2 s and logs nothing
  more while it waits. It starts once the folder is private, with one line saying so, and SIGTERM or SIGINT ends the wait with exit 0.
  Waiting, not a distinct exit status, because launchd's `KeepAlive` true restarts on any exit
  and a longer `ThrottleInterval` would slow every real crash recovery; the wait works the same
  under systemd and by hand.
- On macOS, install copies `gff` into `<state>/bin` and prints one PATH line
  (`macos-identity.md` 6.1).
- Uninstall asks once when any holder job runs; `-yes` skips the question, and with no terminal
  and no `-yes` it changes nothing. Shutdown is the init system's stop: SIGTERM, which runs the
  holder's shutdown (2.8). Uninstall leaves gff-hab's own state (structure, keys, checkpoints).

---

## 6. Edge cases

### 6.1 A child that ignores SIGHUP

On release or shutdown the holder sends SIGHUP and closes the master. A child that ignores SIGHUP
keeps running; its terminal writes then fail. The holder does not escalate on its own after
release; gff-hab decides whether to send TERM, then KILL (signals stay allowed until the child is
reaped). On shutdown the holder escalates to SIGKILL after 5 s (2.8). A draining holder whose last
pane is released escalates the same way: it never exits with a child it has not reaped, because
nothing else would reap it, and launchd, unlike a systemd unit's control group, does not end
the processes a job leaves behind. The holder keeps its waiter either way, so the child is
reaped whenever it ends.

### 6.2 Zombies and reaping

- Each direct child has its own waiter, which reaps it. Go's process wait does this per pid.
- The holder does not reap with "wait for any child", which would race the per-pid waiters.
- The holder does **not** become a child subreaper on Linux. Grandchildren that outlive their
  parent go to the user's service manager, which reaps them. If the holder became a subreaper,
  every orphan from every pane would turn into a zombie the holder must collect.
- A pid is never signalled after its waiter has collected it.

### 6.3 PTY end of file before exit status

- **Output ends, child lives.** The child closed its terminal descriptors, or redirected them. The
  reader ends; the pane shows output closed and the child running. The exit frame comes later from
  the waiter.
- **Child exits, output continues (Linux).** A background job still holds the PTY, so the reader
  never ends. The exit frame goes out 200 ms after exit (2.7). The pane keeps pumping until release
  hangs up the remaining processes.
- **Child exits, terminal revoked (macOS).** When the session leader exits, macOS revokes its
  controlling terminal. The master reads end of file at once, and a surviving background job's
  writes fail with EIO, even when it ignores SIGHUP. The reader ends; the job keeps running
  without a terminal until release or shutdown kills the process group. Checked on macOS 27
  (arm64).
- **macOS.** After the last slave descriptor closes, reading the master may end early. Stage 3
  tests that output written just before exit is not lost on macOS.

### 6.4 A full ring

The ring overwrites its oldest bytes; the byte count keeps rising and the oldest kept offset moves
forward. A replay from an offset older than the oldest kept byte answers from the oldest byte with
the gap flag set. gff-hab then rebuilds that screen from what it has and marks it possibly
incomplete. A full ring never blocks a program.

### 6.5 A slow or hung logic process

Covered in 2.6: readers pause while more than 8 MiB is queued. When the writer makes no progress
for 5 s, the holder drops the connection as a crash. With no logic connected, output goes only to the rings.

### 6.6 A holder crash

This is the case the design cannot save today, and later stages must not promise otherwise.

- When the holder dies, the kernel closes every PTY master it held. Each pane's session gets
  SIGHUP. Programs that honour SIGHUP end; programs that ignore it live on with a dead terminal.
- Survivors are reparented to the service manager. Their exit status is lost to GFF.
- On Linux with `KillMode=process`, survivors stay in the unit's cgroup until they exit; systemd
  logs them as left-over processes at the next start.
- gff-hab sees the holder's socket refuse and its registry entry go stale. It marks those panes
  lost and offers each harness's native resume (for example a resumed agent session in a new pane).
  Native transcripts make this possible.

Mitigations now: keep the holder tiny; no parsing; recover from a panic in every goroutine and log
it instead of exiting; fuzz the protocol decoder. Every critical section on the holder lock
releases it through a deferred unlock, so a panic frees the lock while it unwinds, and recovery
never has to guess whether the lock is still held. A source check in the tests enforces this.

A recovered panic also repairs what its goroutine owned, so no part of the holder is left
waiting on a goroutine that is gone:

- Connection writer or reader: the holder drops the connection and records a crash. gff-hab
  reconnects and replays, and pumps paused by the dead connection's queue resume.
- Pane pump: nothing reads the PTY any more, so the holder hangs the pane up and counts its
  reader as ended. The exit is reported as usual once the child is reaped.
- Pane waiter: the holder reports the pane exited with an unknown status (code -1), hangs it
  up, and reaps an uncollected child in the background. A released pane leaves the owned set,
  so a draining holder can still exit.

Accepted risk: a panic part way through a critical section leaves that section's change half
done. The holder keeps serving rather than ending every program, so state can be left
inconsistent: a pane missing from one index but not the other, a ring count that does not match
a queued frame, an input count that counts bytes never queued, or a connection's queue byte
count that is off (which can pause or never pause the pumps). gff-hab sees these as protocol
errors or gaps on that pane, and can release it. The panic is logged, so the fault can be found
and fixed.

Mitigation in reserve: the Linux systemd fd store (option E), which keeps the masters open across
a holder restart. macOS has no equivalent.

### 6.7 A crash during drain

A drained holder that crashes is restarted by its job only if it failed. A restarted holder has no
panes, sees it is a non-current build, and exits at once. gff-hab treats its panes as lost (6.6).

How the restarted holder knows: the crash left its registry file, marked draining. A holder that
starts and finds its own instance's file marked draining starts draining itself. With no panes,
it removes its socket and registry file and exits with status 0, so the init system does not
restart it again. If gff-hab pruned the stale file first, the restarted holder comes up empty and
not draining. gff-hab's next upgrade pass finds a holder of another build and drains it, which
ends it at once.

### 6.8 Two logic processes

A second gff-hab that says hello takes over; the first is dropped. It is recorded as a crash,
unless it had already sent handover. The holder first lets the first connection's reader take
what is already in its socket (up to 100 ms), so a handover sent just before the second hello
counts. The newest hello wins: when a third hello arrives while the second still waits for that
reader, the second ends without a welcome, and never takes over from the third. Each
logic process holds a lock in the state directory, so this happens only by accident.

---

## 7. Open questions

### Q1. Who finds the OSC 133 prompt marks? **Decision: option 2, coordinator ruling**

The four marks (prompt start, command start, output start, command end with exit status) let the
console show each command as a card. The design doc says the holder can record them; "parses
nothing" argues against.

| Option | How | For | Against |
|---|---|---|---|
| 1. Holder byte scanner | A small state machine scans each read for `ESC ] 133 ; <A-D> … (BEL or ESC \)` and records offsets per pane, without a VT parser | Marks are recorded even while no logic process runs; offsets refer to the ring directly | Breaks "parses nothing". A scanner bug or a policy change (fake marks, nested shells, alternate screen, a nonce) forces a holder release, and holder releases are the expensive kind. The protocol grows command frames |
| 2. gff-hab finds them | gff-hab already reads every byte to parse screens; libghostty-vt tracks these marks. gff-hab records each mark's byte offset and saves the list with its checkpoint | The holder stays dumb. Policy lives where it can change freely. No new holder frames: a command's bytes are a replay with an upper bound (3.1) | Marks during logic downtime are found only when gff-hab replays the ring on return |

Recommendation: **option 2**. The one gain of option 1, marks during logic downtime, is already
covered: gff-hab replays the ring from its checkpoint on return and finds the same marks, under
the same condition as rebuilding the screen. Both options lose command output that has left the
ring, so long-term card output needs storage in gff-hab either way.

Ruling (coordinator, not an owner decision): option 2. gff-hab finds the marks; libghostty tracks
them. The holder only serves replay with an upper bound. Stage 7 moves its detection and command
list into gff-hab (Part 2), and the holder protocol has no mark frames.

### Q2. TCC identity for holder builds on macOS. **Decision: option 1 (coordinator), pending Mac proof P3**

See 5.3 and `macos-identity.md`: signed helper bundles with fixed bundle ids.

### Q3. Should the holder restart a crashed gff-hab? **Decision: no (coordinator)**

herdr-core's holder does. Recommendation: no; the init system restarts gff-hab. With drain,
several holders run at once, and only one supervisor should start gff-hab. The init system already
has backoff and logging. The holder's grace period would only matter where no init system runs,
which we do not support.

Ruling (coordinator, not an owner decision): the init system supervises gff-hab; the holder does
not start or restart it. See 2.1 (fixed choices) and 2.9.

### Q4. Who ends processes that escaped a pane? **Decision: gff-hab, by pane token (coordinator, 2026-11-01)**

herdr-core's holder sweeps every process carrying a pane's scope token when the program exits.
Recommendation: gff-hab owns this policy. The holder sets nothing beyond what spawn gives it;
gff-hab adds a pane token to the environment, and sweeps on release by reading process
environments from the OS. Keeping it out of the holder keeps policy out of the process that must
not change.

Ruling (coordinator, 2026-11-01): take the recommendation. hc sweeps by a scope token too
(`landscape.md` 2.1); we keep the same behaviour but run it in gff-hab. Not built yet.

### Q5. A Mac without a graphical login. **Decision: refuse by default; a flagged `Background` holder later, by explicit option (coordinator, 2026-11-01)**

Recommendation in 5.3: refuse by default, allow a flagged `Background` holder by explicit option.

Stage 6 builds the refusal (5.5). The explicit `Background` option is not built. herdr also runs
its server in the GUI domain (`landscape.md` 2.16), so the refusal matches the bar.

---

## 8. Stage map

| Stage | Task | Sections it builds from |
|---|---|---|
| 2. Wire protocol | task-n7roe6ye | 3.1, 3.2, 2.6 |
| 3. Holder core | task-bqffgxz8 | 2.2 to 2.5, 2.7, 2.8, 2.10, 6.1 to 6.4 |
| 4. Handover and crash recovery | task-fuqwlsop | 2.6, 2.4, 6.5, 6.8 (Q3 decided: no supervision) |
| 5. Upgrade by drain | task-sm3zgmxm | 4.1 to 4.4, 6.7 |
| 6. Service install | task-6xtx0tqr | 5.1 to 5.5, 6.6; Q2, Q5 |
| 7. OSC 133 marks | task-m845kpi9 | Q1, 3.1 (replay with upper bound) |
