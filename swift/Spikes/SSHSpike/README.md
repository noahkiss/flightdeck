# SSH spike

A macOS command-line tool. It proves the app's SSH path before Part 6 builds on it:
`swift-nio-ssh` with a P-256 key held in the Secure Enclave, against an sshd that runs
`gff-hab connect`. The result is in `docs/specs/transport.md` 4.8.

This is a throwaway spike, not part of the Flightdeck app.

## What it does

`run.sh` does everything in a temporary directory and removes it on exit:

1. Builds `gff-hab` from this repo and starts `gff-hab serve` with a temporary socket and state.
2. Makes two Ed25519 host keys, and a client P-256 key with `sshspike key` (Secure Enclave by
   default). The client's public key goes into a temporary authorized-keys file.
3. Starts a throwaway sshd on `127.0.0.1` (port 2743, or `$SSHSPIKE_PORT`) with its own config.
   It never touches the system sshd config or `~/.ssh`.
4. Runs `sshspike connect`. It pins the host key, runs `gff-hab connect` on an exec channel,
   sends `hab.hello` and reads the welcome. Then it opens two more channels at once on the same
   connection, sends `hab.hello` on each, and sends `hab.ping` on the first. Last, it opens a
   burst of more channels (`$SSHSPIKE_BURST`, default 10) to show OpenSSH's session limit.
5. Connects again, warm, for a second timing.
6. Connects with a different pinned host key and checks that the connection is refused.

## Run

You need a Mac with Apple silicon (for the Secure Enclave), Xcode or the Swift toolchain, and Go.

```sh
swift/Spikes/SSHSpike/run.sh        # Secure Enclave key
swift/Spikes/SSHSpike/run.sh soft   # software P-256 key
```

The tool alone:

```sh
sshspike key --kind se|soft --out <file>     # prints the authorized_keys line
sshspike connect --port <n> --key <file> --host-key <host key .pub> --command '<cmd>' \
    [--host 127.0.0.1] [--user <name>] [--burst <n>]
```
