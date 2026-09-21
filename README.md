# plex-4k-transcode-guard

A [LinuxServer.io Docker Mod](https://github.com/linuxserver/docker-mods) for
the `lscr.io/linuxserver/plex` image that stops video sessions which are
**transcoding a 4K/UHD source**, showing the viewer a configurable message.
Everything else is left alone: 4K direct play, 4K direct stream (video
copied, audio transcoded), sub-4K transcodes, music and photos.

```
DOCKER_MODS=ghcr.io/bmanhuge/plex-4k-transcode-guard:latest
```

[![CI](https://github.com/bmanhuge/plex-4k-transcode-guard/actions/workflows/ci.yml/badge.svg)](https://github.com/bmanhuge/plex-4k-transcode-guard/actions/workflows/ci.yml)
[![Publish](https://github.com/bmanhuge/plex-4k-transcode-guard/actions/workflows/publish.yml/badge.svg)](https://github.com/bmanhuge/plex-4k-transcode-guard/actions/workflows/publish.yml)

## Contents

- [How it works](#how-it-works)
- [Installation](#installation)
- [Configuration](#configuration)
- [The stop message file](#the-stop-message-file)
- [Dry-run rollout](#dry-run-rollout)
- [Sample logs](#sample-logs)
- [Validation](#validation)
- [What it never stops, and known limitations](#what-it-never-stops-and-known-limitations)
- [Migrating from the kill4k / runkill4k cron loop](#migrating-from-the-kill4k--runkill4k-cron-loop)
- [Rollback](#rollback)
- [Containers created with docker run](#containers-created-with-docker-run)
- [Security notes](#security-notes)
- [Development](#development)

## How it works

The mod adds two s6-overlay v3 services to the Plex container:

| Service | Type | Purpose |
| --- | --- | --- |
| `init-mod-plex-4k-guard` | oneshot | Prints the installed version and the baseline mode. Never fails, so Plex always starts. |
| `svc-mod-plex-4k-guard` | longrun | Runs `/usr/local/bin/plex-4k-guard` as the container's `abc` user (root when `PUID=0`), supervised by s6. |

`plex-4k-guard` is a static Go binary (linux/amd64 and linux/arm64) with no
runtime dependencies; it does not need Python, curl or anything else from the
Plex image. On every poll it:

1. Reads the server token from
   `/config/Library/Application Support/Plex Media Server/Preferences.xml`
   (`PlexOnlineToken`). The file is re-read whenever it changes and whenever
   Plex answers HTTP 401. The token is sent only in the `X-Plex-Token`
   header and is redacted from every log line.
2. Fetches `GET http://127.0.0.1:32400/status/sessions`.
3. Keeps only `<Video>` sessions with a `<TranscodeSession
   videoDecision="transcode">`. No `TranscodeSession` is direct play;
   `videoDecision="copy"` is direct stream (possibly with an audio
   transcode); `<Track>` and `<Photo>` are ignored.
4. Resolves the **source** resolution. While transcoding, Plex reports the
   transcoded *output* in the session's own `<Media>`/`<Part>`/`<Stream>`
   elements (for example `videoResolution="720p" width="1280"
   protocol="dash"`), so those values must not be used. The guard fetches
   `GET /library/metadata/{ratingKey}` and matches the session's `Media id`
   against the library item's versions, exactly as Tautulli and
   python-plexapi's `PlexSession.source()` do. A source is 4K when
   `width >= 3840` or `height >= 2160` or `videoResolution` normalises to
   `4k`, `2160` or `uhd`. If the id does not match, the guard only concludes
   4K when the item has a single version or every version is 4K; mixed
   versions and missing metadata always mean "leave it alone".
5. Terminates a match with `GET
   /status/sessions/terminate?sessionId=<Session id>&reason=<message>`,
   where `sessionId` is the `id` attribute of the session's `<Session>`
   element (not `sessionKey`). This is the call python-plexapi's
   `PlexSession.stop()` and Tautulli's `get_sessions_terminate()` make.
6. Remembers the session id for the cooldown window so one session is not
   hit repeatedly while Plex tears it down.

Polling uses bounded HTTP timeouts, exponential backoff after failures
(capped at five minutes), a plain `select`-based sleep (no busy loop),
ignores any `http_proxy` environment (it only talks to loopback), and exits
promptly on `SIGTERM`/`SIGINT`.

## Installation

Add the mod to `DOCKER_MODS`. **If the variable already lists other mods,
append with a `|`**; replacing the value would remove them.

Docker Compose (see [`examples/docker-compose.yml`](examples/docker-compose.yml)):

```yaml
services:
  plex:
    image: lscr.io/linuxserver/plex:latest
    environment:
      - PUID=1000
      - PGID=1000
      - VERSION=docker
      - DOCKER_MODS=ghcr.io/bmanhuge/plex-4k-transcode-guard:latest
      - PLEX_4K_GUARD_DRY_RUN=true   # remove once the logs look right
    volumes:
      - /path/to/config:/config
      - /path/to/media:/media
```

Already using a mod:

```yaml
      - DOCKER_MODS=ghcr.io/someone/other-mod:stable|ghcr.io/bmanhuge/plex-4k-transcode-guard:latest
```

`docker run`:

```bash
docker run -d --name plex \
  -e PUID=1000 -e PGID=1000 -e VERSION=docker \
  -e DOCKER_MODS=ghcr.io/bmanhuge/plex-4k-transcode-guard:latest \
  -e PLEX_4K_GUARD_DRY_RUN=true \
  -v /path/to/config:/config -v /path/to/media:/media \
  -p 32400:32400 lscr.io/linuxserver/plex:latest
```

Tags: `latest` follows `main`; `vX.Y.Z`, `vX.Y`, `vX` and `sha-<short>` are
immutable and can be pinned (see [RELEASING.md](RELEASING.md)). The mod is
fetched anonymously by LinuxServer's loader at every container start and
re-applied only when its digest changed; a container that cannot reach
GHCR keeps using the cached copy in `/modcache`.

## Configuration

All variables are optional and validated strictly; an invalid value is
logged and the service refuses to start (Plex is unaffected). Durations
accept Go syntax (`15s`, `1m30s`) or plain seconds (`15`).

| Variable | Default | Meaning |
| --- | --- | --- |
| `PLEX_4K_GUARD_DRY_RUN` | `false` | `true` polls and classifies but never calls the terminate endpoint; matches are logged as `would terminate (dry-run)`. |
| `PLEX_4K_GUARD_POLL_INTERVAL` | `10s` | Delay between polls. Range `2s` to `5m`. |
| `PLEX_4K_GUARD_HTTP_TIMEOUT` | `5s` | Per-request timeout (connect, headers, body). Range `1s` to `60s`. |
| `PLEX_4K_GUARD_COOLDOWN` | `60s` | Minimum time between two actions on the same session id. `0` disables. Max `1h`. |
| `PLEX_4K_GUARD_PLEX_URL` | `http://127.0.0.1:32400` | Base URL of the local server. `http`/`https` only, no credentials. |
| `PLEX_4K_GUARD_PREFERENCES_FILE` | `/config/Library/Application Support/Plex Media Server/Preferences.xml` | Where the token is read from. |
| `PLEX_4K_GUARD_MESSAGE_FILE` | `/config/4k-stop-message.txt` | The stop message file (below). |
| `PLEX_4K_GUARD_MODE_FILE` | *(unset)* | Optional runtime override file; see [Switching modes without recreating the container](#switching-modes-without-recreating-the-container). |
| `PLEX_4K_GUARD_LOG_LEVEL` | `info` | `debug` also logs every skipped session with its reason. |

### Switching modes without recreating the container

Container environment cannot be changed in place, and every recreation
restarts Plex. When `PLEX_4K_GUARD_MODE_FILE` is set (for example to
`/config/4k-guard-mode`) the guard re-reads that file at every poll:

| File content | Effect |
| --- | --- |
| *(file missing)* | The environment baseline applies (`PLEX_4K_GUARD_DRY_RUN`). |
| `dry-run` | Classify and log only. |
| `enforce` | Terminate matches. |
| `off` | Do not poll at all. |
| anything else | Warn once, use the environment baseline. |

```bash
docker exec plex sh -c 'printf enforce > /config/4k-guard-mode'   # go live
docker exec plex sh -c 'printf dry-run > /config/4k-guard-mode'   # back off
docker exec plex rm /config/4k-guard-mode                          # back to the env baseline
```

Only enable this where the config directory is not writable by untrusted
users: whoever can write the file can switch the guard off.

## The stop message file

The reason shown in the Plex client comes from `/config/4k-stop-message.txt`.

- **Missing**: the guard creates it atomically (temp file + rename in the
  same directory, so a reader never sees a partial file), mode `0644`,
  owned by the user the service runs as, containing exactly:

  ```
  You are not allowed to transcode 4K content, please play the normal resolution version.
  ```

- **Present**: its content is used after trimming and collapsing line
  breaks into spaces (Plex shows a single-line dialog). Edits are picked up
  on the next poll, no restart needed.
- **Blank or whitespace only**: the built-in default text is sent, a warning
  is logged once, and the file is left untouched.
- **Unreadable** (permissions) or **not creatable** (`/config` missing):
  the built-in default is sent and a warning is logged once.
- Longer than 1000 characters: truncated with a warning.

The exact bytes sent are visible in the log line for each action
(`reason="..."`) and in the request query as
`reason=You+are+not+allowed+to+transcode+4K+content%2C+please+play+the+normal+resolution+version.`

## Dry-run rollout

1. **Canary in dry-run.** Add the mod with `PLEX_4K_GUARD_DRY_RUN=true` to
   one container. Optionally set `PLEX_4K_GUARD_MODE_FILE=/config/4k-guard-mode`
   now so the later switch does not need another recreation.
2. **Check health.** `docker logs <container> | grep -F '[mod-init]'` shows
   the loader applying the mod and `plex-4k-guard vX.Y.Z installed`;
   `docker logs <container> | grep -F '[plex-4k-guard]'` shows `starting`,
   `plex is ready`, `plex token loaded` and `stop message loaded`. Plex
   itself must be unaffected: `docker exec <container> s6-svstat /run/service/svc-plex`
   reports `up`, and `s6-svstat /run/service/svc-mod-plex-4k-guard` too.
3. **Wait for evidence.** You need at least one log line pair like the one
   in [Sample logs](#sample-logs): `4K video transcode detected` with
   `source=3840x2160/4k` (or `height>=2160`) **and**
   `transcode=video:transcode,...`, followed by `would terminate (dry-run)`
   with the expected `reason=`. Direct play and direct stream of the same
   4K items must appear only as `skipping session reason=direct-play` /
   `audio-only-transcode` at debug level, never as a detection. If no real
   4K transcode happens during the soak, keep dry-run on; do not enforce on
   assumptions.
4. **Expand dry-run** to the rest of the fleet the same way.
5. **Enforce progressively**: switch the canary (`enforce` in the mode file,
   or recreate without `PLEX_4K_GUARD_DRY_RUN`), confirm `terminated
   session` lines and that the viewer sees the dialog, then continue in
   batches, checking `s6-svstat` and Plex reachability after each.

## Sample logs

Taken from the repository's end-to-end test (fixture users and titles)
running inside `lscr.io/linuxserver/plex:latest` with the sideloaded mod.

```
[mod-init] Running Docker Modification Logic
[mod-init] Installing plex-4k-transcode-guard from /mods/plex-4k-transcode-guard/
[mod-init] plex-4k-transcode-guard applied to container
[mod-init] plex-4k-guard v1.0.0 installed; baseline mode from environment: dry-run
[plex-4k-guard] 2026-09-21T21:33:10Z INFO starting version=v1.0.0 mode=dry-run dry_run_env=true poll_interval=10s http_timeout=5s cooldown=1m0s plex_url=http://127.0.0.1:32400 preferences_file="/config/Library/Application Support/Plex Media Server/Preferences.xml" message_file=/config/4k-stop-message.txt mode_file=/config/4k-guard-mode
[plex-4k-guard] 2026-09-21T21:33:12Z INFO plex is ready attempts=2
[plex-4k-guard] 2026-09-21T21:33:12Z INFO effective mode mode=dry-run source=env
[plex-4k-guard] 2026-09-21T21:33:12Z INFO plex token loaded file="/config/Library/Application Support/Plex Media Server/Preferences.xml"
[plex-4k-guard] 2026-09-21T21:33:12Z INFO stop message loaded source=file file=/config/4k-stop-message.txt reason="You are not allowed to transcode 4K content, please play the normal resolution version."
[plex-4k-guard] 2026-09-21T21:33:12Z INFO 4K video transcode detected key=7 session=e6gmj1bjf7jlbz7hu5cqcags user=viewer-a title="Example Movie UHD" evidence="source=3840x2160/4k transcode=video:transcode,audio:transcode output=1280x720 protocol=dash session_media=1399271/1280x720/720p stream_title=\"4K (HEVC Main 10)\" player=\"Plex for Samsung\" state=playing" detail="source media id=1399271 3840x2160 videoResolution=\"4k\""
[plex-4k-guard] 2026-09-21T21:33:12Z INFO would terminate (dry-run) session=e6gmj1bjf7jlbz7hu5cqcags key=7 user=viewer-a title="Example Movie UHD" reason="You are not allowed to transcode 4K content, please play the normal resolution version." reason_source=file
[plex-4k-guard] 2026-09-21T21:33:12Z DEBUG skipping session reason=direct-play key=1 kind=Video user=viewer-b title="Direct Play Movie UHD" detail="no TranscodeSession element"
[plex-4k-guard] 2026-09-21T21:33:12Z DEBUG video transcode is not a 4K source reason=source-not-4k key=16 user=viewer-c title="Example Show - Episode Six" evidence="source=1920x1080/1080 transcode=video:transcode,audio:copy output=1920x1080 protocol=hls session_media=1477136/1920x1080/1080p stream_title=\"1080p (AV1)\" player=\"Plex for Android (TV)\" state=playing" detail="source media id=1477136 1920x1080 videoResolution=\"1080\""
[plex-4k-guard] 2026-09-21T21:33:12Z DEBUG skipping session reason=audio-only-transcode key=20 kind=Video user=viewer-d title="Direct Stream Movie UHD" detail="videoDecision=\"copy\" audioDecision=\"transcode\""
[plex-4k-guard] 2026-09-21T21:33:12Z DEBUG video transcode is not a 4K source reason=source-not-4k key=23 user=viewer-g title="Upscaled Output Movie" evidence="source=1920x1080/1080 transcode=video:transcode,audio:transcode output=3840x2160 protocol=dash session_media=7001/3840x2160/4k stream_title=\"1080p (H.264)\" player=\"Plex for Apple TV\" state=playing" detail="source media id=7001 1920x1080 videoResolution=\"1080\""
[plex-4k-guard] 2026-09-21T21:33:12Z DEBUG poll complete mode=dry-run sessions=9 videos=8 video_transcodes=6 uhd_transcodes=2 would_terminate=2 terminated=0 failed=0 skipped=0
```

After `printf enforce > /config/4k-guard-mode`:

```
[plex-4k-guard] 2026-09-21T21:33:40Z INFO effective mode mode=enforce source=file
[plex-4k-guard] 2026-09-21T21:33:40Z INFO 4K video transcode detected key=7 session=e6gmj1bjf7jlbz7hu5cqcags user=viewer-a title="Example Movie UHD" evidence="source=3840x2160/4k transcode=video:transcode,audio:transcode output=1280x720 protocol=dash ..." detail="source media id=1399271 3840x2160 videoResolution=\"4k\""
[plex-4k-guard] 2026-09-21T21:33:40Z INFO terminated session session=e6gmj1bjf7jlbz7hu5cqcags key=7 user=viewer-a title="Example Movie UHD" reason="You are not allowed to transcode 4K content, please play the normal resolution version." reason_source=file
```

Notice that the 1080p source whose *output* is 3840x2160 (key 23) is
correctly left alone, and that the token never appears.

## Validation

Inside a running container:

```bash
docker logs plex 2>&1 | grep -F '[mod-init]'                 # loader applied the mod
docker logs plex 2>&1 | grep -F '[plex-4k-guard]' | tail -20  # guard activity
docker exec plex s6-svstat /run/service/svc-mod-plex-4k-guard # "up (pid N) M seconds"
docker exec plex s6-svstat /run/service/svc-plex              # Plex unaffected
docker exec plex cat /config/4k-stop-message.txt              # message in use
docker exec plex /usr/local/bin/plex-4k-guard --version
```

Repository checks (also run by CI on every pull request):

```bash
make lint            # gofmt, go vet, staticcheck
make race            # unit tests with the race detector
make image verify-image
MOD_SOURCE=sideload:$PWD/modroot ./scripts/e2e-sideload.sh   # real Plex image + fake Plex API
```

The end-to-end script starts `lscr.io/linuxserver/plex:latest` with the mod
loaded by the real docker-mods loader (sideloaded from a directory, or
`MOD_SOURCE=registry:<ref>` for a published tag), points the guard at a fake
Plex API (`tools/fakeplex`) serving the test fixtures, and asserts: no
terminate call in dry-run, the exact terminate request after switching to
enforce, the exact message file content and mode, and no token in the log.
It uses `docker create` + `docker cp` and a sidecar network namespace, so
it needs no bind mounts or host networking. The publish workflow runs the
same script against the freshly pushed GHCR tag before it is considered
released.

Unit tests cover token parsing and redaction, default/custom/blank/
unreadable message files, every classification positive and negative
(including output-resolution confusion and multi-version items), the exact
terminate request encoding, dry-run never terminating, timeouts, 401
handling, backoff, cooldown, mode-file overrides and graceful shutdown.

## What it never stops, and known limitations

Never stopped: sessions without a `TranscodeSession`; sessions whose video
decision is `copy` (direct stream) even if audio or subtitles are
transcoded; `<Track>`/`<Photo>` sessions; video transcodes whose matched
library version is below 3840x2160 and not labelled 4k/2160/uhd; sessions
whose library item cannot be fetched or has mixed-resolution versions with
no matching id; sessions without a `<Session id>` (logged as a warning).

Limitations:

- It only guards the Plex server inside the same container. One mod
  instance per container.
- It relies on Plex marking the streaming version with `selected="1"` and
  on `/library/metadata` being available (a library scan removing the item
  mid-stream yields "cannot resolve source media" and no action).
- Live TV / DVR sessions usually have no library metadata and are therefore
  never stopped.
- Plex's terminate endpoint requires Plex Pass on the server account; the
  request is still made and its HTTP status is logged.
- Hardware "4K to 4K" transcodes (tone-mapping, bitrate reduction with a
  4K output) *are* stopped: the source is 4K and the video is being
  transcoded, which is what the guard is for.
- Sub-4K sources upscaled to a 4K output are *not* stopped (see key 23 in
  the sample logs).
- The unit of enforcement is the Plex session; a client that immediately
  restarts playback creates a new session and is stopped again after the
  cooldown.

## Migrating from the kill4k / runkill4k cron loop

The legacy approach ran a host-level loop every 5 seconds that
`kill -9`'d any Plex transcoder process whose command line matched HEVC
patterns or 4K library paths:

```
@reboot nohup bash /opt/setup/scripts/runkill4k.sh &
```

Behavioural differences to plan for:

| | Legacy `kill4k.sh` | This mod |
| --- | --- | --- |
| Runs | on the host, one loop for all containers | inside each container, one service per Plex |
| Decides by | transcoder process arguments (`hevc`, `libx264`, `h264_vaapi`, `/data/movies4k`, `/data/tv4k`) | Plex session API: video transcode **and** 4K source metadata |
| Also killed | any HEVC to H.264 transcode regardless of resolution (1080p HEVC included), and direct streams whose ffmpeg line matched | only 4K-source video transcodes; 1080p HEVC transcodes and 4K direct streams are untouched |
| User sees | playback error / buffering | a Plex dialog with the configured message |
| Side effects | `nohup` output to a deleted `/tmp` file that grows forever | structured container log |

Suggested order:

1. Deploy the mod fleet-wide in **dry-run** while the cron loop keeps
   running; collect `would terminate` evidence.
2. Enforce progressively (see above) while the loop still runs; both will
   act on true 4K transcodes, which is harmless.
3. Once every container enforces, remove the `@reboot` entry from the
   crontab (and from any canonical cron source it is installed from), stop
   the running loop (`pkill -f runkill4k.sh`; it holds a deleted `/tmp`
   file open that is released when it exits) and remove `kill4k.sh` and
   `runkill4k.sh` from the script directories, keeping a backup copy.
4. Accept that 1080p HEVC transcodes are now allowed again; if that is not
   desired, that is a separate policy (Plex's own transcode limits or a
   different mod), not this one.

## Rollback

- **Per container, without a restart** (only if `PLEX_4K_GUARD_MODE_FILE`
  is set): `docker exec plex sh -c 'printf dry-run > /config/4k-guard-mode'`
  (or `off`).
- **Per container, with a restart**: recreate it with
  `PLEX_4K_GUARD_DRY_RUN=true`, or remove the mod from `DOCKER_MODS`. A
  container recreated without the mod is clean: nothing persists outside
  `/config/4k-stop-message.txt` and the optional mode file, and the mod's
  files live in the container layer only.
- **Fleet-wide, immediately**: the mod only ever acts on the loopback
  Plex API, so stopping the service is enough:
  `docker exec plex s6-svc -d /run/service/svc-mod-plex-4k-guard` (restarts
  with the container).
- **Publishing rollback**: re-point `DOCKER_MODS` to an earlier immutable
  tag (`vX.Y.Z` or `sha-<short>`); the loader re-applies the older layer on
  the next container start.

## Containers created with docker run

Containers without a compose file cannot have their environment edited in
place. `contrib/recreate-with-mod.sh` rebuilds an equivalent `docker run`
from `docker inspect` (same image ID, name, environment, binds, ports,
networks, restart policy, devices, capabilities, limits, logging, labels)
with the mod appended to `DOCKER_MODS`, prints the command for review, and
with `--apply` performs a stop, rename-to-backup, run, health wait and
automatic rollback on failure:

```bash
contrib/recreate-with-mod.sh plex                                  # print only, secrets masked
contrib/recreate-with-mod.sh --apply --set PLEX_4K_GUARD_DRY_RUN=true plex
contrib/recreate-with-mod.sh --rollback plex                       # restore the backup
```

It refuses to run when it finds a setting it cannot reproduce (GPU device
requests, `--link`, anonymous volumes, custom healthchecks, ...). It keeps
the exact image ID by default so the recreation does not silently upgrade
Plex; pass `--image lscr.io/linuxserver/plex:latest` to upgrade on purpose.
CI exercises it against a throwaway container (`contrib/test-recreate.sh`).

## Security notes

- The token is read from disk, kept in memory, sent only as a request
  header to loopback, and stripped from every log line by a redactor that
  is primed before the first request. It is never placed in argv, URLs,
  error strings, tests or source.
- The image contains no credentials, no shell and no package manager: one
  static binary plus the s6 service files, in a single layer.
- Proxy environment variables present in many Plex deployments are ignored
  by the guard's HTTP client.
- The guard needs no capabilities, no Docker socket and no host access.
- Workflows use the repository `GITHUB_TOKEN` with `packages: write` only,
  pinned action SHAs and no third-party secrets.

## Development

```
cmd/plex-4k-guard/      entrypoint (signals, config, exit codes)
internal/guard/         config, token, message, mode, cooldown, Plex client,
                        XML parsing, classification, poll loop, tests
root/                   files copied into the container by the loader
tools/fakeplex/         fake Plex API for end-to-end runs
scripts/e2e-sideload.sh in-container end-to-end test
contrib/                docker-run recreation helper
```

`make all` runs lint, tests and a build. See [RELEASING.md](RELEASING.md)
for publishing and [CHANGELOG.md](CHANGELOG.md) for history. MIT licensed.
