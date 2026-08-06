# icloud-synk

*По-русски: [README_RU.md](README_RU.md)*

A daemon for Ubuntu (Go, no CGO) that keeps a local folder in sync with iCloud:

* **iCloud Drive — two-way, as you work.** Drop a file into the folder and it goes to the
  cloud (inotify, ~2 s debounce). Add a file on icloud.com and it shows up locally
  (polling, every 30 s by default).
* **Photos — download only, and append only.** The photo library is written to
  `Photos/YYYY/MM/` as originals (HEIC/JPEG/MOV untouched). Deleting a photo in the cloud
  does **not** delete your local copy — see
  [Photos are an archive, not a mirror](#photos-are-an-archive-not-a-mirror).

```
~/iCloud/
├── Drive/          ← two-way sync with iCloud Drive
├── Photos/2026/08/ ← download only, never deleted
└── .icloud-synk/
    ├── state.json  ← sync database
    └── trash/      ← where files deleted in iCloud Drive land
```

## Read this before installing

Apple provides **no** public iCloud API for Linux: CloudKit Web Services only reaches your
own app's container, not the user's Drive and not the photo library. So this program speaks
the same private web API that icloud.com does (SRP login against `idmsa.apple.com`, then the
`drivews` / `docws` / `ckdatabasews` services) — the approach pyicloud and icloudpd have used
for years. Consequences:

* Apple can change these endpoints without notice, and then the code needs fixing.
  Everything protocol-version-dependent lives in `internal/icloud/client.go`.
* There are no push notifications for cloud-side changes, hence polling.
* Polling too aggressively can look like abuse to Apple — the default is 30 s with
  exponential backoff up to 15 minutes on errors.
* Uploading **into** the photo library is deliberately unsupported: the private CloudKit
  asset-upload flow is undocumented and can corrupt the library. Files you place into
  `Photos/` by hand are left alone and never sent anywhere.

## Photos are an archive, not a mirror

The `Photos/` folder only ever grows. There is no code path that removes a file from it: the
photo pass walks what iCloud hands out and downloads whatever is missing
(`internal/synk/photos.go`). The `delete_remote` / `delete_local` flags apply to `Drive/`
only — deletion reconciliation runs over that tree and never sees `Photos/`.

What follows from that:

* **Delete a photo in the cloud (or on your iPhone) and the local file stays.** Apple purges
  "Recently Deleted" after 30 days; your copy lives on. That is the intent: the local folder
  is an archive nothing disappears from on its own.
* **Delete a file from `Photos/` by hand and it will not be re-downloaded.** The asset is
  recorded in `state.json` as fetched, and a re-download only happens if the asset itself
  changed in iCloud (its checksum changed). To get a deleted file back, drop the matching
  entry from `state.json` or reset the state.
* **Reorganizing `Photos/` is safe** — renames and moves go unnoticed and are never
  reverted.

## Install

### Prebuilt binary (no Go needed)

Static, CGO-free binaries are attached to every
[release](https://github.com/kolesnikav/icloud-synk/releases) for `linux/amd64` and
`linux/arm64`:

```bash
curl -fsSL https://github.com/kolesnikav/icloud-synk/releases/latest/download/icloud-synk-linux-amd64.tar.gz \
  | tar -xz --strip-components=1 -C /tmp icloud-synk-linux-amd64/icloud-synk
install -Dm755 /tmp/icloud-synk ~/.local/bin/icloud-synk
icloud-synk version
```

Each tarball also carries `icloud-synk.service` and both READMEs. `SHA256SUMS` next to the
archives lets you verify the download:

```bash
sha256sum -c SHA256SUMS --ignore-missing
```

### From source

```bash
git clone git@github.com:kolesnikav/icloud-synk.git && cd icloud-synk
make build              # bin/icloud-synk
make install            # ~/.local/bin/icloud-synk
```

Or straight through Go:

```bash
go install github.com/kolesnikav/icloud-synk/cmd/icloud-synk@latest
```

Requires Go 1.24+ (built and tested on 1.26). One external dependency: `fsnotify`.

## First run

```bash
icloud-synk login --dir ~/iCloud
```

It asks for your Apple ID, password (terminal echo off) and the six-digit 2FA code.
The password is **not stored**: only the session and trust token go to disk
(`~/.config/icloud-synk/session.json`, mode 0600), good for roughly 30 days.

### If the 2FA code never arrives

Apple **never emails the code**. Where to get it:

* Your iPhone/iPad/Mac shows an "Apple ID Sign In Requested" dialog — the code appears
  **only after you tap "Allow"**. Tap "Don't Allow" and the prompt will not come back.
* More reliable is generating a code yourself, without any notification:
  **Settings → [your name] → Sign-In & Security → Two-Factor Authentication →
  Get Verification Code** (on a Mac, System Settings).
* At the `Two-factor code:` prompt these commands work:
  * `sms` — send the code by SMS to a trusted number (`sms 2` picks the second one);
  * `push` — re-send the request to your trusted devices;
  * `q` — cancel the login.

The program prints how many trusted devices and which numbers are available.
Codes are accepted with spaces too (`123 456`).

Start with a dry run to see the plan without changing anything:

```bash
icloud-synk sync --dry-run
```

Then a single full pass:

```bash
icloud-synk sync
```

## Daemon

```bash
make service     # installs the user unit, enables and starts it
journalctl --user -u icloud-synk -f
```

It is a user unit rather than a system one: `notify-send` needs the session bus. To keep the
daemon running without an active graphical login:

```bash
sudo loginctl enable-linger $USER
```

When the trust token expires the daemon does not die: it logs the fact, shows a notification
and waits. Just run `icloud-synk login` in a terminal — the daemon picks up the new session
by itself, no restart needed.

## Commands

| Command | What it does |
|---|---|
| `icloud-synk login [--dir PATH] [--apple-id ID]` | log in, write config and session |
| `icloud-synk sync [--dry-run] [--drive\|--photos]` | one pass, then exit |
| `icloud-synk daemon [--poll 30s]` | continuous sync |
| `icloud-synk status` | Apple ID, session expiry, counters, connectivity check |
| `icloud-synk config` | effective configuration |
| `icloud-synk logout` | forget the session (folder and state untouched) |
| `icloud-synk version` | build version, platform and Go version |

`--verbose` turns on debug logging for any command.

## Configuration

`~/.config/icloud-synk/config.json` (created by `login`; values below are the defaults):

```json
{
  "apple_id": "you@example.com",
  "target_dir": "/home/you/iCloud",
  "sync_drive": true,
  "sync_photos": true,
  "poll_interval": "30s",
  "photos_interval": "15m",
  "debounce": "2s",
  "workers": 4,
  "delete_remote": true,
  "delete_local": true,
  "conflict": "both",
  "max_file_size": 0,
  "exclude": [".icloud-synk", ".DS_Store", "*.part", "*.crdownload", "*.partial",
              ".goutputstream-*", "~$*", ".~lock.*", "*.swp", "4913", ".Trash-*"],
  "notify": true,
  "password_command": ""
}
```

* `delete_remote` — a local deletion moves the file to the iCloud trash.
* `delete_local` — a deletion in iCloud moves the local file to `.icloud-synk/trash/<date>/`.
  Either flag can be turned off, in which case deleted files simply come back from the other
  side. Both apply to `Drive/` only and have no effect on `Photos/`.
* `conflict` — `both` (keep both versions), `newer` (the newer one wins),
  `remote` (the iCloud version always wins, the local one goes to the local trash).
* `exclude` — patterns are matched against every path segment.
* `password_command` — a command that prints the password, for logging in without a terminal
  (e.g. `secret-tool lookup service icloud`). The `ICLOUD_PASSWORD` variable works too.

## How it decides what to copy (Drive)

Every pass compares **three** sources: the local tree, the iCloud tree, and the state left by
the previous pass (`state.json`). Without the third one a missing file is indistinguishable
from a deleted one.

| local | iCloud | state | action |
|---|---|---|---|
| new | — | — | upload to iCloud |
| — | new | — | download |
| changed | unchanged | present | upload |
| unchanged | changed | present | download |
| changed | changed | present | conflict (see `conflict`) |
| deleted | present | present | iCloud trash |
| present | deleted | present | `.icloud-synk/trash/` |

"Changed locally" means a different size or mtime; when sizes match, SHA-256 is compared as
well, so a `touch` does not trigger a re-upload. "Changed in iCloud" means a different etag.

With `conflict: both` the newer version wins and the loser is kept alongside it as
`report.conflict-2026-08-06T12-30-00.docx`, which the next pass uploads too — nothing is
lost.

For `Photos/` this table does not apply: there are only two states, downloaded or not.

## What it does not do

* Does not upload photos or videos into the photo library (see above).
* Does not delete anything from `Photos/` — neither on the cloud's request nor on its own.
* Does not sync iCloud bundle packages (`.pages`, `.numbers`, `.key`): they are skipped with
  a log line.
* Does not follow symlinks — they are skipped.
* Does not carry renames across as renames: the file is uploaded afresh under the new name
  and the old one goes to the trash.
* Does not support collaborative editing or iCloud version history.

## Development

```bash
make test    # unit tests: SRP against an independent verifier, reconcile, scanner, state
make vet     # go vet + gofmt
make release # dist/: one tarball per platform + SHA256SUMS (runs test and vet first)
```

Releasing is just a tag — `.github/workflows/release.yml` runs `make release` and publishes
the archives:

```bash
git tag -a v0.2.0 -m "icloud-synk v0.2.0" && git push origin v0.2.0
```

Layout:

```
cmd/icloud-synk/       CLI
internal/config/       configuration
internal/icloud/       private-API client: SRP, session, Drive, Photos
internal/synk/         state, scanner, reconcile, apply, daemon
packaging/             systemd user unit
```
