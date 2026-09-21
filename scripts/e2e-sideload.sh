#!/usr/bin/env bash
# End-to-end test of the mod inside the real LinuxServer.io Plex image.
#
# A fake Plex API (tools/fakeplex) runs in a sidecar container serving the
# test fixtures. The real lscr.io/linuxserver/plex image is started sharing
# the sidecar's network namespace, with the mod applied by the real
# docker-mods loader, either sideloaded from a directory
# (MOD_SOURCE=sideload:<dir>) or pulled from a registry
# (MOD_SOURCE=registry:<image ref>). Plex itself starts inside the
# container (unclaimed); the guard is pointed at the fake API on
# 127.0.0.1:32499. No bind mounts or host networking are needed, so the
# script works from CI runners and from sandboxes that only share a Docker
# socket with the host.
#
# Assertions:
#   1. the loader applies the mod and the init oneshot reports the version
#   2. in dry-run the guard logs "would terminate" and the fake API sees no
#      terminate call
#   3. after flipping the mode file to enforce, the fake API receives exactly
#      GET /status/sessions/terminate with the expected sessionId and reason,
#      and nothing else is terminated
#   4. the message file is created with the exact default text, mode 0644
#   5. the fixture token never appears in the container log
#   6. the container stops cleanly
#
# Every container it creates is removed on exit.
set -euo pipefail

MOD_SOURCE="${MOD_SOURCE:?set MOD_SOURCE=sideload:<dir> or registry:<ref>}"
PLEX_IMAGE="${PLEX_IMAGE:-lscr.io/linuxserver/plex:latest}"
SIDECAR_IMAGE="${SIDECAR_IMAGE:-alpine:3.20}"
NAME="${E2E_CONTAINER_NAME:-plex-4k-guard-e2e-$$}"
FAKE="${NAME}-fakeplex"
TIMEOUT="${E2E_TIMEOUT:-300}"
ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
FIXTURES="${ROOT}/internal/guard/testdata"
WORK="$(mktemp -d)"
EXPECTED='You are not allowed to transcode 4K content, please play the normal resolution version.'

log() { printf '[e2e] %s\n' "$*"; }
fail() { printf '[e2e] FAIL: %s\n' "$*" >&2; exit 1; }
# Container logs are captured into variables before grepping: with pipefail,
# `docker logs | grep -q` reports 141 (SIGPIPE) when grep exits early.
plex_logs() { docker logs "${NAME}" 2>&1 || true; }
fake_logs() { docker logs "${FAKE}" 2>&1 || true; }

cleanup() {
    set +e
    docker rm -f "${NAME}" "${FAKE}" >/dev/null 2>&1
    rm -rf "${WORK}"
}
trap cleanup EXIT

# 1. Fake Plex API sidecar. The binary and fixtures are copied in with
#    docker cp so no host path has to be visible to the daemon.
log "building fakeplex"
(cd "${ROOT}" && CGO_ENABLED=0 GOOS=linux go build -trimpath -o "${WORK}/fakeplex" ./tools/fakeplex)
docker pull -q "${SIDECAR_IMAGE}" >/dev/null
docker create --name "${FAKE}" --add-host localhost:127.0.0.1 "${SIDECAR_IMAGE}" /fakeplex -listen 127.0.0.1:32499 \
    -sessions /fixtures/sessions_mixed.xml -metadata-dir /fixtures -token fixtureTOKENvalue1234567 >/dev/null
docker cp "${WORK}/fakeplex" "${FAKE}:/fakeplex"
docker cp "${FIXTURES}" "${FAKE}:/fixtures"
docker start "${FAKE}" >/dev/null
sleep 1
grep -q "listening on" <<<"$(fake_logs)" || { fake_logs; fail "fakeplex did not start"; }

# 2. Mod source.
MOD_ENV=()
case "${MOD_SOURCE}" in
    sideload:*)
        DIR="${MOD_SOURCE#sideload:}"
        [[ -d "${DIR}/etc/s6-overlay" ]] || fail "sideload dir ${DIR} does not look like a mod root"
        MOD_ENV=(-e DOCKER_MODS=plex-4k-transcode-guard -e DOCKER_MODS_SIDELOAD=true)
        ;;
    registry:*)
        DIR=""
        MOD_ENV=(-e "DOCKER_MODS=${MOD_SOURCE#registry:}")
        ;;
    *) fail "unknown MOD_SOURCE ${MOD_SOURCE}" ;;
esac

# 3. The real Plex image, sharing the sidecar's network namespace.
log "creating ${PLEX_IMAGE} as ${NAME}"
docker create --name "${NAME}" --network "container:${FAKE}" \
    -e PUID=1000 -e PGID=1000 -e TZ=Etc/UTC -e VERSION=docker \
    "${MOD_ENV[@]}" \
    -e DOCKER_MODS_DEBUG=true \
    -e PLEX_4K_GUARD_DRY_RUN=true \
    -e PLEX_4K_GUARD_POLL_INTERVAL=2s \
    -e PLEX_4K_GUARD_HTTP_TIMEOUT=3s \
    -e PLEX_4K_GUARD_COOLDOWN=0 \
    -e PLEX_4K_GUARD_PLEX_URL=http://127.0.0.1:32499 \
    -e PLEX_4K_GUARD_PREFERENCES_FILE=/fixtures/preferences.xml \
    -e PLEX_4K_GUARD_MODE_FILE=/config/4k-guard-mode \
    -e PLEX_4K_GUARD_LOG_LEVEL=debug \
    "${PLEX_IMAGE}" >/dev/null
mkdir -p "${WORK}/fixtures"
cp "${FIXTURES}/preferences.xml" "${WORK}/fixtures/preferences.xml"
docker cp "${WORK}/fixtures" "${NAME}:/fixtures"
if [[ -n "${DIR}" ]]; then
    mkdir -p "${WORK}/mods"
    cp -a "${DIR}" "${WORK}/mods/plex-4k-transcode-guard"
    docker cp "${WORK}/mods" "${NAME}:/mods"
fi
docker start "${NAME}" >/dev/null

wait_for_log() {
    local pattern="$1" deadline=$(( $(date +%s) + TIMEOUT )) text
    while (( $(date +%s) < deadline )); do
        text="$(plex_logs)"
        if grep -qF -- "${pattern}" <<<"${text}"; then return 0; fi
        if [[ "$(docker inspect -f '{{.State.Running}}' "${NAME}" 2>/dev/null)" != "true" ]]; then
            tail -60 <<<"${text}" >&2
            fail "container exited while waiting for: ${pattern}"
        fi
        sleep 2
    done
    { echo "----- first 60 log lines -----"; sed -n '1,60p' <<<"${text}"; echo "----- last 40 log lines -----"; tail -40 <<<"${text}"; } >&2
    fail "timed out waiting for: ${pattern}"
}

log "waiting for the loader to apply the mod"
wait_for_log "[mod-init] plex-4k-guard"
grep -F "[mod-init]" <<<"$(plex_logs)" | sed -n '1,12p' 
log "waiting for the guard to start (dry-run)"
wait_for_log "[plex-4k-guard]"
wait_for_log "would terminate (dry-run)"
grep -F "[plex-4k-guard]" <<<"$(plex_logs)" | grep -E "starting|effective mode|stop message loaded|4K video transcode detected|would terminate" | sed -n '1,8p'

if grep -q TERMINATE <<<"$(fake_logs)"; then
    fake_logs; fail "dry-run must not call the terminate endpoint"
fi
log "dry-run made no terminate call"

# 4. Message file created with the exact default text.
ACTUAL="$(docker exec "${NAME}" cat /config/4k-stop-message.txt)"
[[ "${ACTUAL}" == "${EXPECTED}" ]] || fail "message file content mismatch: ${ACTUAL}"
MODE_BITS="$(docker exec "${NAME}" stat -c %a /config/4k-stop-message.txt)"
[[ "${MODE_BITS}" == "644" ]] || fail "message file mode ${MODE_BITS}, want 644"
log "message file created with the exact default text (mode ${MODE_BITS}, owner $(docker exec "${NAME}" stat -c %U:%G /config/4k-stop-message.txt))"

# 5. Flip to enforce through the mode file and expect the exact terminate call.
docker exec "${NAME}" sh -c 'printf enforce > /config/4k-guard-mode'
wait_for_log "terminated session"
sleep 3
FAKETEXT="$(fake_logs)"
grep -qF 'TERMINATE method=GET sessionId="e6gmj1bjf7jlbz7hu5cqcags" reason="'"${EXPECTED}"'"' <<<"${FAKETEXT}" \
    || { printf '%s\n' "${FAKETEXT}"; fail "expected exact terminate call not seen"; }
if grep -F TERMINATE <<<"${FAKETEXT}" | grep -vE 'sessionId="(e6gmj1bjf7jlbz7hu5cqcags|unselected-session-id)"'; then
    fail "a session that must never be terminated was terminated"
fi
log "enforce mode terminated only the two 4K transcodes:"
grep -F TERMINATE <<<"${FAKETEXT}" | sort -u

# 6. Back to dry-run through the mode file, and no token in the logs.
docker exec "${NAME}" sh -c 'printf dry-run > /config/4k-guard-mode'
wait_for_log "mode=dry-run source=file"
if grep -q fixtureTOKENvalue1234567 <<<"$(plex_logs)"; then
    fail "token leaked into the container log"
fi
log "token never appeared in the container log"

# 7. Graceful stop.
START=$(date +%s)
docker stop -t 20 "${NAME}" >/dev/null
log "container stopped in $(( $(date +%s) - START ))s"
log "PASS"
