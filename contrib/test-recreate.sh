#!/usr/bin/env bash
# Exercises contrib/recreate-with-mod.sh against a throwaway container with
# a representative set of settings and verifies they survive recreation.
# Every container and network it creates is removed on exit.
set -euo pipefail
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
NAME="recreate-test-$$"
NET="recreate-net-$$"
NET2="recreate-net2-$$"
VOL="$(mktemp -d)"
cleanup() {
    set +e
    docker rm -f "${NAME}" "${NAME}-plain" >/dev/null 2>&1
    for c in $(docker ps -aq --filter "name=${NAME}-plain."); do docker rm -f "$c" >/dev/null 2>&1; done
    for c in $(docker ps -aq --filter "name=${NAME}.pre-4k-guard."); do docker rm -f "$c" >/dev/null 2>&1; done
    for c in $(docker ps -aq --filter "name=${NAME}.new-4k-guard."); do docker rm -f "$c" >/dev/null 2>&1; done
    docker rm -f "x${NAME}.pre-4k-guard.99991231T235959Z" >/dev/null 2>&1
    docker network rm "${NET}" "${NET2}" >/dev/null 2>&1
    rm -rf "${VOL}"
}
trap cleanup EXIT
fail() { echo "FAIL: $*" >&2; exit 1; }

HAVE_NETS=true
if ! docker network create "${NET}" >/dev/null 2>&1 || ! docker network create "${NET2}" >/dev/null 2>&1; then
    echo "NOTE: cannot create user networks on this host (address pools exhausted?); testing on the default bridge without aliases" >&2
    HAVE_NETS=false; NET=bridge
fi
docker pull -q alpine:3.20 >/dev/null
NETARGS=(--network "${NET}")
[[ "${HAVE_NETS}" == false ]] || NETARGS+=(--network-alias original)
docker run -d --name "${NAME}" --restart unless-stopped "${NETARGS[@]}" \
    -p 127.0.0.1:18080:80 -p 18099:81 --mount type=tmpfs,target=/scratch -v "${VOL}:/data:ro" -v "${VOL}:/data-rw" \
    -e PUID=1000 -e PGID=1000 -e "DOCKER_MODS=ghcr.io/example/first-mod:stable" -e "SECRET_TOKEN=do-not-print" \
    --label app=test --label tier=canary --device /dev/null:/dev/null:rwm --cap-add NET_ADMIN --cap-drop MKNOD \
    --memory 128m --cpus 0.5 --add-host example.internal:10.9.8.7 --dns 1.1.1.1 --ulimit nofile=2048:4096 \
    --log-opt max-size=1m --log-opt max-file=2 --hostname mybox --stop-timeout 7 --shm-size 128m --pids-limit 200 \
    alpine:3.20 sleep infinity >/dev/null
[[ "${HAVE_NETS}" == false ]] || docker network connect --alias second "${NET2}" "${NAME}"
BEFORE="$(docker inspect "${NAME}" | jq '.[0]')"

# Dry run prints a command and masks the secret.
OUT="$("${HERE}/recreate-with-mod.sh" --mod ghcr.io/bmanhuge/plex-4k-transcode-guard:latest "${NAME}" 2>&1)"
echo "${OUT}"
grep -q -- '--restart unless-stopped' <<<"${OUT}" || fail "restart policy missing"
grep -q -- '-p 18099:81/tcp' <<<"${OUT}" || fail "plain host port mapping mangled"
grep -qF -- 'type=tmpfs\,target=/scratch' <<<"${OUT}" || fail "tmpfs mount mangled"
grep -q -- '18099::' <<<"${OUT}" && fail "empty HostIp shifted fields"
grep -qF -- 'DOCKER_MODS=ghcr.io/example/first-mod:stable\|ghcr.io/bmanhuge/plex-4k-transcode-guard:latest' <<<"${OUT}" || fail "DOCKER_MODS not appended"
grep -qF 'SECRET_TOKEN=\<masked\>' <<<"${OUT}" || fail "secret not masked"
grep -q 'do-not-print' <<<"${OUT}" && fail "secret printed"
[[ "${HAVE_NETS}" == false ]] || grep -q "docker network connect --alias second ${NET2} ${NAME}" <<<"${OUT}" || fail "extra network missing"
SHORT_ID="$(jq -r '.Id[0:12]' <<<"${BEFORE}")"
grep -q -- "--network-alias ${SHORT_ID}" <<<"${OUT}" && fail "docker's automatic short-id alias must not be re-added explicitly"
docker inspect "${NAME}" >/dev/null || fail "dry run must not touch the container"

# Apply.
"${HERE}/recreate-with-mod.sh" --apply --wait 30 --mod ghcr.io/bmanhuge/plex-4k-transcode-guard:latest --set EXTRA=1 "${NAME}"
AFTER="$(docker inspect "${NAME}" | jq '.[0]')"

cmp() { # jq expression, label
    local b a
    b="$(jq -cS "$1" <<<"${BEFORE}")"; a="$(jq -cS "$1" <<<"${AFTER}")"
    [[ "${b}" == "${a}" ]] || fail "$2 differs:\n before ${b}\n after  ${a}"
}
cmp '.Image' image
cmp '.HostConfig.Binds | sort' binds
cmp '.HostConfig.PortBindings' ports
cmp '.HostConfig.Mounts' mounts
cmp '.HostConfig.RestartPolicy' restart
cmp '.HostConfig.NetworkMode' network-mode
cmp '.NetworkSettings.Networks | keys' networks
cmp '[.NetworkSettings.Networks[] | (.Aliases // []) | map(select(length != 12))] | flatten | sort' aliases
cmp '.HostConfig.Devices' devices
cmp '.HostConfig.CapAdd' cap-add
cmp '.HostConfig.CapDrop' cap-drop
cmp '.HostConfig.Memory' memory
cmp '.HostConfig.NanoCpus' cpus
cmp '.HostConfig.ExtraHosts' extra-hosts
cmp '.HostConfig.Dns' dns
cmp '.HostConfig.Ulimits' ulimits
cmp '.HostConfig.LogConfig' logging
cmp '.HostConfig.ShmSize' shm
cmp '.HostConfig.PidsLimit' pids
cmp '.Config.Hostname' hostname
cmp '.Config.StopTimeout' stop-timeout
cmp '.Config.Labels' labels
cmp '.Config.Cmd' cmd
cmp '[.Config.Env[] | select(startswith("DOCKER_MODS=") | not) | select(startswith("EXTRA=") | not)] | sort' env
grep -qx 'DOCKER_MODS=ghcr.io/example/first-mod:stable|ghcr.io/bmanhuge/plex-4k-transcode-guard:latest' < <(jq -r '.Config.Env[]' <<<"${AFTER}") || fail "DOCKER_MODS after apply"
grep -qx 'EXTRA=1' < <(jq -r '.Config.Env[]' <<<"${AFTER}") || fail "--set not applied"
[[ "$(docker inspect -f '{{.State.Status}}' "${NAME}")" == "running" ]] || fail "not running after apply"
docker ps -a --format '{{.Names}}' | grep -q "${NAME}.pre-4k-guard." && fail "backup should have been removed"
docker ps -a --format '{{.Names}}' | grep -q "${NAME}.new-4k-guard." && fail "staging container left behind"

# Idempotent: applying again does not duplicate the mod.
"${HERE}/recreate-with-mod.sh" --apply --wait 30 --keep-backup "${NAME}"
grep -qx 'DOCKER_MODS=ghcr.io/example/first-mod:stable|ghcr.io/bmanhuge/plex-4k-transcode-guard:latest' < <(docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "${NAME}") || fail "mod duplicated"
docker ps -a --format '{{.Names}}' | grep -q "${NAME}.pre-4k-guard." || fail "backup should have been kept"

# A container created without --network (NetworkMode default/bridge) must
# not produce a bogus "docker network connect bridge" line.
docker run -d --name "${NAME}-plain" alpine:3.20 sleep infinity >/dev/null
PLAIN="$("${HERE}/recreate-with-mod.sh" "${NAME}-plain" 2>&1)"
grep -q 'docker network connect' <<<"${PLAIN}" && fail "plain bridge container treated as an extra network"
"${HERE}/recreate-with-mod.sh" --apply --wait 30 "${NAME}-plain"
[[ "$(docker inspect -f '{{.State.Status}}' "${NAME}-plain")" == "running" ]] || fail "plain container not running after apply"

# Rollback restores the backup, and only this container's backup: a decoy
# whose name merely contains ours must not be picked.
docker run -d --name "x${NAME}.pre-4k-guard.99991231T235959Z" alpine:3.20 sleep infinity >/dev/null
"${HERE}/recreate-with-mod.sh" --rollback "${NAME}"
docker rm -f "x${NAME}.pre-4k-guard.99991231T235959Z" >/dev/null
[[ "$(docker inspect -f '{{.State.Status}}' "${NAME}")" == "running" ]] || fail "not running after rollback"
echo "PASS"
