# localsend-cli

[![CI](https://github.com/zebroc/localsend-cli/actions/workflows/ci.yml/badge.svg)](https://github.com/zebroc/localsend-cli/actions/workflows/ci.yml)

A headless [LocalSend](https://localsend.org/) client in a single Go binary. It speaks
LocalSend protocol v2.1 and interoperates with the official apps on phones and desktops.

One binary does both jobs:

* **daemon** — a long-running process that accepts every incoming transfer and stores it
  in a directory, keeping the sender's folder structure.
* **sender** — a one-shot command that pushes files and directories to a device you name
  by its alias.

## Install

### Homebrew

```sh
brew tap zebroc/tap
brew trust zebroc/tap        # Homebrew 6 requires third-party taps to be trusted
brew install localsend-cli
```

The formula is `localsend-cli`; the cask `localsend` is the official GUI app, so the two
can coexist — though not on the same port, see the limitation below.

### From source

```sh
go install github.com/zebroc/localsend-cli@latest
# or
git clone https://github.com/zebroc/localsend-cli && cd localsend-cli && go build .
```

Cross-compiling works as usual, e.g. `GOOS=linux GOARCH=arm64 go build`.

## Usage

### Receive (daemon)

```sh
localsend-cli --daemon --dstdir ~/Downloads
```

Everything that arrives is accepted and written below `--dstdir`. A sender that transfers
a folder ends up as a folder: `Photos/2024/img.jpg` becomes
`~/Downloads/Photos/2024/img.jpg`. Existing files are never overwritten — the new file
becomes `img (1).jpg`.

Add `--pin 123456` to require a PIN, and `-v` for verbose logging.

### Send

```sh
localsend-cli phone report.pdf photo.jpg    # files
localsend-cli phone ~/Downloads/            # a directory, sent recursively
localsend-cli phone notes.txt ~/Pictures    # mixed; each path is classified by itself
```

The first argument is the target, the rest are paths — the tool decides on its own
whether a path is a file or a directory.

The target is normally the **alias** of the other device (the name it shows in the app),
matched case-insensitively, exact matches before prefix matches. You can also give an
address, which skips discovery entirely:

```sh
localsend-cli 192.168.1.5 backup.zip
localsend-cli 192.168.1.5:53317 backup.zip
localsend-cli phone.local:53317 backup.zip
```

A bare hostname without a port is treated as an alias, not as a host — use `host:port`
when you mean a hostname.

### List devices

```sh
localsend-cli --list
```

```
ALIAS       ADDRESS              PROTO  TYPE      MODEL    FINGERPRINT
MacBook     192.168.178.2:53317  https  desktop   macOS    E5BD94265791548A…
Pixel       192.168.178.31:53317 https  mobile    Pixel 8  A1B2C3D4E5F60718…
```

### Flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--daemon` | off | run as a receiving daemon |
| `--dstdir DIR` | `~/Downloads` | where received files go |
| `--list` | off | list devices and exit |
| `--port N` | `53317` | TCP and UDP port |
| `--alias NAME` | hostname | the name other devices see (persisted) |
| `--pin PIN` | none | require a PIN (daemon) / send a PIN (sender) |
| `--timeout D` | `5s` | how long to wait for devices to answer |
| `--parallel N` | `1` | files to upload at the same time |
| `--no-tls` | off | serve plain HTTP instead of HTTPS |
| `--group ADDR` | `224.0.0.167` | multicast group |
| `--announce-interval D` | `5m` | how often the daemon re-announces (`0` = only at startup) |
| `-v` | off | verbose logging |

## Running the daemon as a service

The daemon logs to stdout, never forks and writes no PID file, so any process
supervisor can run it directly.

### brew services

```sh
brew services start localsend-cli     # start now and at login
brew services stop localsend-cli
brew services info localsend-cli
tail -f "$(brew --prefix)/var/log/localsend-cli.log"
```

Files land in `~/Downloads`. To change that, pass your own arguments — Homebrew has no
mechanism for editing a formula's service, so run it under your own launchd agent or
supervisor instead:

```sh
localsend-cli --daemon --dstdir ~/Inbox
```

Only one LocalSend instance can hold port 53317, so quit the official app before
starting the service (see the limitation below).

### daemontools

`~/service/localsend-cli/run`:

```sh
#!/bin/sh
exec 2>&1
exec envdir /Users/zebroc/service/localsend-cli/env \
  /opt/homebrew/bin/localsend-cli --daemon --dstdir /Users/zebroc/Downloads
```

Point the path at wherever the binary lives — `$(brew --prefix)/bin/localsend-cli` for a
Homebrew install, or the binary in your checkout when you build from source.

`log/run` alongside it:

```sh
#!/bin/sh
exec multilog s1048576 n10 ./main
```

Put `HOME` into `env/` — the configuration directory is derived from it, and
`supervise` does not necessarily inherit one:

```sh
echo /Users/zebroc > ~/service/localsend-cli/env/HOME
```

A `down` file keeps it from starting at boot. Then:

```sh
svc -u ~/service/localsend-cli    # start
svc -d ~/service/localsend-cli    # stop
svc -t ~/service/localsend-cli    # restart (after rebuilding the binary)
svstat ~/service/localsend-cli    # status
tail -f ~/service/localsend-cli/log/main/current
```

SIGTERM is handled gracefully; in-flight transfers get up to five seconds to finish.

## Configuration directory

`~/Library/Application Support/localsend-cli` on macOS, `~/.config/localsend-cli` on
Linux. `LOCALSEND_CLI_CONFIG_DIR` overrides it, which is also how you run a second
identity on one host.

| File | Contents |
| --- | --- |
| `cert.pem`, `key.pem` | the self-signed certificate identifying this device (mode 600) |
| `config.json` | the alias |
| `peers.json` | devices seen before, so a repeat send does not need a scan |

The certificate is created on first run and reused afterwards: peers recognise a device
by the SHA-256 fingerprint of its certificate, so replacing it would make this device
look new to everyone.

## How discovery works

1. The sender announces itself to the multicast group `224.0.0.167:53317`, three times
   (100 ms, 500 ms, 2 s apart) as the reference implementation does.
2. Every LocalSend device that hears the announcement answers with
   `POST /api/localsend/v2/register` to the port from the announcement. The sender runs a
   short-lived HTTPS server for those callbacks. Devices that cannot reach it fall back
   to a multicast answer.
3. The daemon does the same in reverse: it answers other devices' announcements, which is
   what makes it appear in the app, and re-announces itself every few minutes.
4. Devices found this way are cached in `peers.json`. A later send verifies the cached
   address — including the certificate fingerprint — and only starts a new scan if that
   fails.

### Limitation: sharing the port with the official app

Only one LocalSend instance per host can own port 53317. If the official app is running,
the daemon refuses to start with `port 53317 is already in use`. Quit the app, or give
the daemon its own `--port` — but note that a non-default port also moves the multicast
port, so devices announcing on 53317 will no longer find it. Two instances of
`localsend-cli` on separate ports discover each other fine.

The listener is deliberately IPv4-only. LocalSend discovery is IPv4 (`224.0.0.167`) and
the official app binds IPv4; a dual-stack listener would happily bind `[::]:53317`
alongside the app's IPv4 socket and then silently receive nothing.

The same asymmetry exists for UDP: macOS and BSD only let two sockets share a UDP port
when **both** set `SO_REUSEPORT`, and the app sets only `SO_REUSEADDR`. Two
`localsend-cli` instances share it without trouble.

## Security notes

* **The daemon accepts everything.** Anyone who can reach the port can drop files into
  `--dstdir`. That is the point of the tool, but keep it off untrusted networks, or use
  `--pin` (wrong PINs are rate-limited per IP).
* Incoming file names are sanitised: path traversal (`..`), absolute paths, Windows drive
  letters and control characters cannot escape `--dstdir`.
* A file is written to `<name>.part` and only renamed once exactly the announced number of
  bytes arrived, so an interrupted transfer never leaves a file that looks complete.
* LocalSend has no certificate authority, so peer certificates are self-signed by design.
  We check the validity period, and for a cached device also that the fingerprint still
  matches — a mismatch triggers a fresh scan instead of a transfer to the wrong device.
  Discovery itself is unauthenticated: on a hostile network an alias can be spoofed, so
  compare the fingerprint from `--list` if it matters.

## Protocol notes

Implemented from the [protocol specification](https://github.com/localsend/protocol)
(v2.1) and verified against the reference implementation:

* Routes: `POST /register`, `GET /info`, `POST /prepare-upload`, `POST /upload`,
  `POST /cancel` below `/api/localsend/v2`.
* Announcements carry both `announce` (v2) and `announcement` (v1) so old peers
  understand them too.
* `fileType` is sent as a MIME type; the legacy v1 enum is accepted on receive.
* The HTTPS server of current app versions demands a client certificate, so the sender
  always presents one. Our server asks for one but serves senders without it.
* **Directories are not a protocol concept.** A folder is transferred as a set of files
  whose `fileName` carries the relative path. Empty directories therefore disappear —
  the official app behaves the same way.
* Several senders may transfer at once. The official app answers a second sender with
  409; nothing here needs to ask a user, so that limit would serve no purpose.
* Not implemented: the reverse "download API" (section 5 of the spec, browser downloads
  from the sender) and WebRTC signalling for transfers over the internet.

## Tests

```sh
go test ./...              # everything
go test -race ./...        # what CI runs
```

The suite runs offline and needs no LocalSend instance: it binds ephemeral ports on the
loopback interface and creates its own throwaway identities in temporary directories
(`LOCALSEND_CLI_CONFIG_DIR`), so it never touches your certificate or peer cache.

What it covers beyond the obvious:

* The fingerprint format is pinned against the reference implementation's own test vector,
  so a change here cannot silently make us unrecognisable to real peers.
* The spec's example JSON payloads are parsed as-is, including the v1 `announcement` key
  and the legacy `fileType` enum.
* Received file names are attacked with `..`, absolute paths, drive letters and control
  characters; nothing may end up outside the destination directory.
* Full transfers over TLS: directory trees, non-ASCII names, collisions, wrong PINs,
  parallel uploads, a file that disappears mid-transfer, and plain HTTP.
* Answering an announcement really registers with the peer over mTLS, with the fingerprint
  taken from the client certificate.

One test needs real multicast delivery and is therefore opt-in, because container and CI
networks often drop it:

```sh
LOCALSEND_CLI_MULTICAST_TESTS=1 go test -run TestMulticastRoundTrip ./internal/discovery/
```

It runs the whole discovery loop: announce, a listening daemon answers by calling our
register route, the scan returns it.

## License

[MIT](LICENSE). This is an independent implementation written from the published
protocol specification; it contains no code from the LocalSend project.
