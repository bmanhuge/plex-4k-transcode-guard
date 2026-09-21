# Fleet rollout runbook

A step-by-step procedure for introducing `plex-4k-transcode-guard` across
many LinuxServer.io Plex containers, replacing a host-level kill loop. It is
written for an operator; every command is read-only unless marked
**change**.

## 0. Preconditions

- Containers run `linuxserver/plex` / `lscr.io/linuxserver/plex` (any tag
  with s6-overlay v3 and `docker-mods.v3`, i.e. anything from 2023 on).
- Each container's `/config` holds `Library/Application Support/Plex Media
  Server/Preferences.xml` with a `PlexOnlineToken` (a claimed server). An
  unclaimed server is polled once the token appears.
- The container can reach `ghcr.io` at start (through the same proxy the
  loader already uses for other mods, if any). Offline restarts reuse the
  cached copy in `/modcache`.
- Know how each container is created:
  - **compose-managed**: a `docker-compose.yml` you edit and `docker compose up -d`.
  - **docker-run-managed** (no `com.docker.compose.*` labels): use
    `contrib/recreate-with-mod.sh`, and also update whatever creates new
    containers (deployment script, provisioning template) so new ones get
    the mod too.
- Inventory the current `DOCKER_MODS` values; the mod must be **appended**
  to whatever is already there:

  ```bash
  for c in $(docker ps --format '{{.Names}}'); do
    printf '%s\t' "$c"; docker inspect "$c" --format '{{range .Config.Env}}{{println .}}{{end}}' | grep -E '^DOCKER_MODS=' || echo 'DOCKER_MODS=<unset>'
  done | sort -k2 | uniq -c -f1
  ```

## 1. Canary in dry-run (**change**, one container)

Pick a container with real 4K libraries and active viewers.

compose:

```yaml
      - DOCKER_MODS=<existing value>|ghcr.io/bmanhuge/plex-4k-transcode-guard:latest
      - PLEX_4K_GUARD_DRY_RUN=true
      - PLEX_4K_GUARD_MODE_FILE=/config/4k-guard-mode   # optional, enables step 4 without a restart
```

```bash
docker compose -f /path/to/docker-compose.yml up -d
```

docker-run:

```bash
contrib/recreate-with-mod.sh --set PLEX_4K_GUARD_DRY_RUN=true --set PLEX_4K_GUARD_MODE_FILE=/config/4k-guard-mode <container>          # review
contrib/recreate-with-mod.sh --apply --set PLEX_4K_GUARD_DRY_RUN=true --set PLEX_4K_GUARD_MODE_FILE=/config/4k-guard-mode <container>  # do it
```

## 2. Verify the canary

```bash
c=<container>
docker logs "$c" 2>&1 | grep -F '[mod-init]'            # "Adding ghcr.io/bmanhuge/plex-4k-transcode-guard:latest", "applied to container", "plex-4k-guard vX.Y.Z installed; see the [plex-4k-guard] log lines"
docker exec "$c" s6-svstat /run/service/svc-plex        # up
docker exec "$c" s6-svstat /run/service/svc-mod-plex-4k-guard   # up ("down" means the guard refused its configuration; read the ERROR line)
docker logs "$c" 2>&1 | grep -F '[plex-4k-guard]' | grep -E 'starting|plex is ready|token loaded|stop message loaded|effective mode'
docker exec "$c" cat /config/4k-stop-message.txt         # default text, created by the guard
curl -fsS "http://127.0.0.1:<hostport>/identity" >/dev/null && echo plex-ok
```

Expected: no `WARN`/`ERROR` lines other than a possible one-off
`waiting for plex` while Plex boots.

## 3. Collect evidence (dry-run soak)

Leave the canary for as long as it takes to observe a real 4K transcode.
This is the gate; do not skip it.

```bash
docker logs "$c" 2>&1 | grep -F '4K video transcode detected'
docker logs "$c" 2>&1 | grep -F 'would terminate (dry-run)'
```

A qualifying pair shows, on the detection line, `source=<W>x<H>/<label>`
with `W >= 3840` or `H >= 2160` **and** `transcode=video:transcode,...`,
and on the dry-run line the expected `reason="..."`. Also confirm that
direct plays of 4K items are *not* detected (turn on
`PLEX_4K_GUARD_LOG_LEVEL=debug` to see `skipping session
reason=direct-play` for them).

If the soak ends without a real 4K transcode, extend the dry-run to more
containers (step 4a) rather than enforcing. Enforcement without observed
evidence is a guess.

## 4a. Expand dry-run (**change**, batches)

Repeat step 1 for the remaining containers in batches (for example five at
a time), running step 2 checks after each batch. For docker-run fleets a
loop over `contrib/recreate-with-mod.sh --apply ...` with a pause between
containers is fine; each recreation restarts that one Plex for roughly
10 to 30 seconds.

Update the provisioning template / deployment script for *new* containers
in the same change so the fleet does not drift.

## 4b. Enforce (**change**, canary first, then batches)

With the mode file enabled:

```bash
docker exec "$c" sh -c 'printf enforce > /config/4k-guard-mode'
docker logs "$c" 2>&1 | grep -F 'effective mode'     # mode=enforce source=file
```

Without it: recreate without `PLEX_4K_GUARD_DRY_RUN` (or with `false`).

Confirm the first real `terminated session` line and that the viewer saw
the dialog, then continue in batches. After each batch:

```bash
for c in <batch>; do docker exec "$c" s6-svstat /run/service/svc-plex /run/service/svc-mod-plex-4k-guard; done
docker logs "$c" 2>&1 | grep -F '[plex-4k-guard]' | grep -E 'WARN|ERROR' | tail
```

`terminate failed ... HTTP status 4xx` on one container usually means the
server account has no Plex Pass or the token was rotated; the guard keeps
polling and re-reads the token on 401.

## 5. Retire the legacy host loop (**change**, after every container enforces)

On each host:

```bash
crontab -l | grep -n runkill4k                       # confirm the @reboot entry
crontab -l | grep -v 'runkill4k' | crontab -         # remove it
pgrep -af runkill4k                                   # the running loop(s)
pkill -f runkill4k.sh                                 # stop them
ls -l /proc/*/fd 2>/dev/null | grep -c '(deleted)'    # the leaked nohup output files are released once the loop exits
```

If crontabs are installed from a canonical file (for example a shared
`crons.txt` applied by an `update-crons.sh`), remove the line there first or
the next refresh re-adds it. Move `kill4k.sh` and `runkill4k.sh` out of the
script directories into a dated backup directory rather than deleting them
outright, and remove them from the scripts repository in a separate PR.

## 6. Rollback matrix

| Situation | Action |
| --- | --- |
| One container misbehaves, mode file enabled | `docker exec c sh -c 'printf dry-run > /config/4k-guard-mode'` (or `off`) |
| One container misbehaves, no mode file | recreate with `PLEX_4K_GUARD_DRY_RUN=true`; or `docker exec c s6-svc -d /run/service/svc-mod-plex-4k-guard` until the next restart |
| Fleet-wide stop | same `s6-svc -d` loop over all containers; nothing else is touched |
| Remove entirely | drop the mod from `DOCKER_MODS` and recreate; delete `/config/4k-stop-message.txt` and `/config/4k-guard-mode` if unwanted |
| Bad release | pin `DOCKER_MODS` to the previous `vX.Y.Z`/`sha-` tag and restart |
| Recreation went wrong (docker-run) | `contrib/recreate-with-mod.sh --rollback <container>` restores the stopped backup |

## 7. Things that look like problems but are not

- `waiting for plex attempt=N` at start: Plex is still booting; the guard
  backs off up to 30 s between attempts.
- `plex token unavailable`: the server is not claimed yet; polling starts
  when the token appears. Logged at most every five minutes.
- `cannot resolve source media ... 404`: the item was removed from the
  library while playing; the session is left alone.
- `cooldown active`: the same session was already acted on within the
  cooldown window.
- `message file ... is blank; using the default message`: someone emptied
  the file; the default text is sent and the file is not modified.
- `mode file ... running in dry-run until it is fixed or removed`: the
  override file has unexpected content; the guard never escalates on a bad
  file, so fix or remove it and the baseline applies again.
- `media-not-found` (debug level): the streaming version is no longer in
  the library item (split, re-match, re-scan); that session is left alone.
