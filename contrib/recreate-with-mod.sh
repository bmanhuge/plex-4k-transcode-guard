#!/usr/bin/env bash
# recreate-with-mod.sh: recreate an existing Docker container with an extra
# LinuxServer.io Docker Mod appended to DOCKER_MODS, preserving every other
# setting that `docker inspect` exposes. Intended for containers that were
# created with `docker run` (no compose file) and cannot simply be
# re-deployed.
#
# The container's environment cannot be changed in place, so the only way to
# add a mod is to create a new container. This script:
#   1. reads the running container's configuration with `docker inspect`
#   2. rebuilds an equivalent `docker run` command (same image ID, name,
#      env, binds, ports, networks, restart policy, devices, limits, ...)
#      with DOCKER_MODS extended
#   3. refuses to continue when it meets a setting it cannot reproduce
#   4. with --apply: creates the new container under a temporary name while
#      the old one is still running, compares the two `docker inspect`
#      documents (HostConfig, Config, exposed ports; everything except the
#      intended environment change and the auto-generated hostname) and
#      refuses on any difference; only then stops the old container, renames
#      it to a backup, renames and starts the new one, waits for it to be
#      healthy/running, and removes the backup (or rolls back on failure)
#
# Usage:
#   recreate-with-mod.sh [options] CONTAINER
#   recreate-with-mod.sh --rollback CONTAINER
#
# Options:
#   --mod REF            mod to append (default ghcr.io/bmanhuge/plex-4k-transcode-guard:latest)
#   --set KEY=VALUE      add or replace an environment variable (repeatable)
#   --unset KEY          drop an environment variable (repeatable)
#   --image REF          use this image instead of the container's exact image ID
#   --apply              actually recreate (default is to print the command)
#   --show-secrets       print env values verbatim (default masks TOKEN/KEY/PASS/SECRET/CLAIM/proxy)
#   --wait SECONDS       how long to wait for the new container to be healthy (default 120)
#   --keep-backup        keep the stopped old container instead of removing it
#   --force              apply even when the inspect comparison shows differences
#   --rollback           restore the most recent backup of CONTAINER
#
# Requires: bash 4.4+ (empty arrays under set -u), docker, jq.
set -euo pipefail

MOD="ghcr.io/bmanhuge/plex-4k-transcode-guard:latest"
APPLY=false
SHOW_SECRETS=false
WAIT=120
KEEP_BACKUP=false
FORCE=false
ROLLBACK=false
IMAGE_OVERRIDE=""
declare -a SET_ENV=() UNSET_ENV=()
BACKUP_SUFFIX=".pre-4k-guard."

usage() { sed -n '2,38p' "$0" | sed 's/^# \{0,1\}//'; }
mask_arg() { # print one argument shell-quoted, masking secret-looking env values
    local a="$1"
    if [[ "${SHOW_SECRETS}" != true && "${a}" =~ ^[A-Za-z_][A-Za-z0-9_]*=(.*)$ ]] && [[ "${a%%=*}" =~ (TOKEN|KEY|PASS|SECRET|CLAIM|_proxy|_PROXY) ]]; then
        a="${a%%=*}=<masked>"
    fi
    printf '%q' "${a}"
}
log() { printf '[recreate] %s\n' "$*" >&2; }
die() { printf '[recreate] ERROR: %s\n' "$*" >&2; exit 1; }

while [[ $# -gt 0 ]]; do
    case "$1" in
        --mod) MOD="$2"; shift 2 ;;
        --set) SET_ENV+=("$2"); shift 2 ;;
        --unset) UNSET_ENV+=("$2"); shift 2 ;;
        --image) IMAGE_OVERRIDE="$2"; shift 2 ;;
        --apply) APPLY=true; shift ;;
        --show-secrets) SHOW_SECRETS=true; shift ;;
        --wait) WAIT="$2"; shift 2 ;;
        --keep-backup) KEEP_BACKUP=true; shift ;;
        --force) FORCE=true; shift ;;
        --rollback) ROLLBACK=true; shift ;;
        -h|--help) usage; exit 0 ;;
        --) shift; break ;;
        -*) die "unknown option $1" ;;
        *) break ;;
    esac
done
[[ $# -eq 1 ]] || { usage; exit 2; }
NAME="${1#/}"

(( BASH_VERSINFO[0] > 4 || (BASH_VERSINFO[0] == 4 && BASH_VERSINFO[1] >= 4) )) || die "bash 4.4 or newer is required"
command -v docker >/dev/null || die "docker not found"
command -v jq >/dev/null || die "jq not found"

if [[ "${ROLLBACK}" == true ]]; then
    BACKUP="$(docker ps -a --format '{{.Names}}' | grep -E "^$(printf '%s' "${NAME}${BACKUP_SUFFIX}" | sed 's/[.[\*^$]/\\&/g')[0-9TZ]+$" | sort | tail -1 || true)"
    [[ -n "${BACKUP}" ]] || die "no backup container named ${NAME}${BACKUP_SUFFIX}* found"
    log "rolling back ${NAME} to ${BACKUP}"
    if docker inspect "${NAME}" >/dev/null 2>&1; then
        docker rm -f "${NAME}" >/dev/null
    fi
    docker rename "${BACKUP}" "${NAME}"
    docker start "${NAME}" >/dev/null
    log "restored ${NAME} from ${BACKUP}"
    exit 0
fi

INSPECT="$(docker inspect "${NAME}" 2>/dev/null)" || die "container ${NAME} not found"
INSPECT="$(jq '.[0]' <<<"${INSPECT}")"
IMAGE_ID="$(jq -r '.Image' <<<"${INSPECT}")"
IMAGE_REF="$(jq -r '.Config.Image' <<<"${INSPECT}")"
IMAGE="${IMAGE_OVERRIDE:-${IMAGE_ID}}"
IMAGE_INSPECT="$(docker image inspect "${IMAGE}" 2>/dev/null | jq '.[0]')" || die "image ${IMAGE} not found locally"
CONTAINER_ID="$(jq -r '.Id' <<<"${INSPECT}")"

# ---- refuse settings this script does not reproduce ----------------------
refuse_if() { # jq expression, description
    if [[ "$(jq -r "$1" <<<"${INSPECT}")" == "true" ]]; then die "cannot reproduce: $2 (run with docker directly)"; fi
}
refuse_if '(.HostConfig.DeviceRequests // []) | length > 0' "GPU/device requests"
refuse_if '(.HostConfig.Links // []) | length > 0' "legacy --link"
refuse_if '(.HostConfig.VolumesFrom // []) | length > 0' "--volumes-from"
refuse_if '(.HostConfig.StorageOpt // {}) | length > 0' "--storage-opt"
refuse_if '(.HostConfig.DeviceCgroupRules // []) | length > 0' "device cgroup rules"
refuse_if '.HostConfig.AutoRemove == true' "--rm containers"
refuse_if '(.HostConfig.Isolation // "") | (. != "" and . != "default")' "non-default isolation"
refuse_if '(.HostConfig.Annotations // {}) | length > 0' "OCI annotations"
refuse_if '(.HostConfig.CpuRealtimePeriod // 0) != 0 or (.HostConfig.CpuRealtimeRuntime // 0) != 0' "cpu realtime settings"
refuse_if '((.HostConfig.BlkioWeightDevice // []) | length > 0) or ((.HostConfig.BlkioDeviceReadBps // []) | length > 0) or ((.HostConfig.BlkioDeviceWriteBps // []) | length > 0) or ((.HostConfig.BlkioDeviceReadIOps // []) | length > 0) or ((.HostConfig.BlkioDeviceWriteIOps // []) | length > 0)' "blkio device limits"
# Anonymous volumes would be lost on recreation.
ANON="$(jq -r '[.Mounts[] | select(.Type == "volume" and ((.Name | length) == 64))] | length' <<<"${INSPECT}")"
[[ "${ANON}" == "0" ]] || die "container uses ${ANON} anonymous volume(s); their data would be orphaned"
# A custom healthcheck differing from the image cannot be carried over 1:1.
if [[ "$(jq -c '.Config.Healthcheck // null' <<<"${INSPECT}")" != "$(jq -c '.Config.Healthcheck // null' <<<"${IMAGE_INSPECT}")" ]]; then
    die "container has a healthcheck that differs from the image's; add it to this script before continuing"
fi

# ---- assemble the docker run command --------------------------------------
# ARGS holds only the options and the image/command; the verb and name are
# prefixed when the command is printed or executed.
declare -a ARGS=()
add() { ARGS+=("$@"); }
# add_each FLAG JQ_PATH: one FLAG per element of the array at JQ_PATH.
add_each() { local v; while IFS= read -r v; do [[ -n "${v}" ]] && add "$1" "${v}"; done < <(jq -r "$2 // [] | .[]" <<<"${INSPECT}"); }
# res JQ_PATH FLAG: FLAG VALUE unless the value is 0, null or empty.
res() { local v; v="$(jq -r "$1 // 0" <<<"${INSPECT}")"; [[ "${v}" == "0" || "${v}" == "null" || -z "${v}" ]] || add "$2" "${v}"; }

# Hostname only when it was set explicitly (default is the id prefix).
HOSTNAME_VAL="$(jq -r '.Config.Hostname' <<<"${INSPECT}")"
[[ "${HOSTNAME_VAL}" == "${CONTAINER_ID:0:12}" ]] || add --hostname "${HOSTNAME_VAL}"
DOMAIN="$(jq -r '.Config.Domainname // ""' <<<"${INSPECT}")"; [[ -z "${DOMAIN}" ]] || add --domainname "${DOMAIN}"
USER_VAL="$(jq -r '.Config.User // ""' <<<"${INSPECT}")"; IMG_USER="$(jq -r '.Config.User // ""' <<<"${IMAGE_INSPECT}")"
[[ "${USER_VAL}" == "${IMG_USER}" ]] || add --user "${USER_VAL}"
WD="$(jq -r '.Config.WorkingDir // ""' <<<"${INSPECT}")"; IMG_WD="$(jq -r '.Config.WorkingDir // ""' <<<"${IMAGE_INSPECT}")"
[[ "${WD}" == "${IMG_WD}" ]] || add --workdir "${WD}"
MAC="$(jq -r '.Config.MacAddress // .NetworkSettings.MacAddress // ""' <<<"${INSPECT}")"
if [[ "$(jq -r '.Config.MacAddress // ""' <<<"${INSPECT}")" != "" ]]; then add --mac-address "${MAC}"; fi

# Terminal settings (a detached container normally has neither).
[[ "$(jq -r '.Config.Tty // false' <<<"${INSPECT}")" != "true" ]] || add --tty
[[ "$(jq -r '.Config.OpenStdin // false' <<<"${INSPECT}")" != "true" ]] || add --interactive

# Restart policy.
RP_NAME="$(jq -r '.HostConfig.RestartPolicy.Name // "no"' <<<"${INSPECT}")"
RP_MAX="$(jq -r '.HostConfig.RestartPolicy.MaximumRetryCount // 0' <<<"${INSPECT}")"
if [[ -n "${RP_NAME}" && "${RP_NAME}" != "no" ]]; then
    if [[ "${RP_NAME}" == "on-failure" && "${RP_MAX}" != "0" ]]; then add --restart "on-failure:${RP_MAX}"; else add --restart "${RP_NAME}"; fi
fi

# Network mode and additional networks.
NETMODE="$(jq -r '.HostConfig.NetworkMode' <<<"${INSPECT}")"
add --network "${NETMODE}"
declare -a EXTRA_NETWORKS=()
# Older engines record the CLI's literal "default" while the attachment is
# keyed "bridge" in NetworkSettings.Networks.
FIRST_NET="${NETMODE}"
[[ "${FIRST_NET}" != "default" ]] || FIRST_NET="bridge"
while IFS=$'\x1f' read -r netname aliases ipv4 ipv6; do
    [[ -n "${netname}" ]] || continue
    if [[ "${netname}" == "${FIRST_NET}" ]]; then
        if [[ -n "${ipv4}" ]]; then add --ip "${ipv4}"; fi
        if [[ -n "${ipv6}" ]]; then add --ip6 "${ipv6}"; fi
        for a in ${aliases}; do add --network-alias "${a}"; done
    else
        EXTRA_NETWORKS+=("${netname}"$'\x1f'"${aliases}"$'\x1f'"${ipv4}"$'\x1f'"${ipv6}")
    fi
done < <(jq -r '.NetworkSettings.Networks | to_entries[] | [.key, ((.value.Aliases // []) | map(select(test("^[0-9a-f]{12}$") | not)) | join(" ")), (.value.IPAMConfig.IPv4Address // ""), (.value.IPAMConfig.IPv6Address // "")] | join("\u001f")' <<<"${INSPECT}")

# Ports.
[[ "$(jq -r '.HostConfig.PublishAllPorts // false' <<<"${INSPECT}")" != "true" ]] || add -P
while IFS= read -r ep; do [[ -n "${ep}" ]] && add --expose "${ep}"; done < <(jq -r --argjson img "$(jq '.Config.ExposedPorts // {}' <<<"${IMAGE_INSPECT}")" '.Config.ExposedPorts // {} | keys[] | select($img[.] == null)' <<<"${INSPECT}")
while IFS=$'\x1f' read -r cport hostip hostport; do
    [[ -n "${cport}" ]] || continue
    if [[ -n "${hostip}" ]]; then add -p "${hostip}:${hostport}:${cport}"; else add -p "${hostport}:${cport}"; fi
done < <(jq -r '.HostConfig.PortBindings // {} | to_entries[] | .key as $c | .value[] | [$c, (.HostIp // ""), .HostPort] | join("\u001f")' <<<"${INSPECT}")

# Bind mounts, named volumes and tmpfs.
add_each -v '.HostConfig.Binds'
while IFS=$'\x1f' read -r mtype src dst ro prop nocopy tsize tmode; do
    [[ -n "${mtype}" ]] || continue
    spec="type=${mtype},target=${dst}"
    [[ -z "${src}" ]] || spec+=",source=${src}"
    [[ "${ro}" != "true" ]] || spec+=",readonly"
    [[ -z "${prop}" ]] || spec+=",bind-propagation=${prop}"
    [[ "${nocopy}" != "true" ]] || spec+=",volume-nocopy"
    [[ -z "${tsize}" || "${tsize}" == "0" ]] || spec+=",tmpfs-size=${tsize}"
    [[ -z "${tmode}" || "${tmode}" == "0" ]] || spec+=",tmpfs-mode=$(printf '%o' "${tmode}")"
    add --mount "${spec}"
done < <(jq -r '.HostConfig.Mounts // [] | .[] | [.Type, (.Source // ""), .Target, ((.ReadOnly // false) | tostring), (.BindOptions.Propagation // ""), ((.VolumeOptions.NoCopy // false) | tostring), ((.TmpfsOptions.SizeBytes // 0) | tostring), ((.TmpfsOptions.Mode // 0) | tostring)] | join("\u001f")' <<<"${INSPECT}")
while IFS=$'\x1f' read -r dst opts; do [[ -n "${dst}" ]] && { if [[ -n "${opts}" ]]; then add --tmpfs "${dst}:${opts}"; else add --tmpfs "${dst}"; fi; }; done < <(jq -r '.HostConfig.Tmpfs // {} | to_entries[] | [.key, .value] | join("\u001f")' <<<"${INSPECT}")

# Devices, capabilities, security.
while IFS=$'\x1f' read -r h c perms; do [[ -n "${h}" ]] && add --device "${h}:${c}:${perms}"; done < <(jq -r '.HostConfig.Devices // [] | .[] | [.PathOnHost, .PathInContainer, .CgroupPermissions] | join("\u001f")' <<<"${INSPECT}")
add_each --cap-add '.HostConfig.CapAdd'
add_each --cap-drop '.HostConfig.CapDrop'
add_each --security-opt '.HostConfig.SecurityOpt'
[[ "$(jq -r '.HostConfig.Privileged' <<<"${INSPECT}")" != "true" ]] || add --privileged
[[ "$(jq -r '.HostConfig.ReadonlyRootfs' <<<"${INSPECT}")" != "true" ]] || add --read-only
[[ "$(jq -r '.HostConfig.Init // false' <<<"${INSPECT}")" != "true" ]] || add --init
add_each --group-add '.HostConfig.GroupAdd'
for mode in IpcMode PidMode UTSMode UsernsMode CgroupnsMode; do
    v="$(jq -r ".HostConfig.${mode} // \"\"" <<<"${INSPECT}")"
    case "${mode}:${v}" in
        *:"" ) ;;
        IpcMode:private|IpcMode:shareable|CgroupnsMode:private|CgroupnsMode:host) ;; # daemon defaults
        IpcMode:*) add --ipc "${v}" ;;
        PidMode:*) add --pid "${v}" ;;
        UTSMode:*) add --uts "${v}" ;;
        UsernsMode:*) add --userns "${v}" ;;
    esac
done
RUNTIME="$(jq -r '.HostConfig.Runtime // ""' <<<"${INSPECT}")"; [[ -z "${RUNTIME}" || "${RUNTIME}" == "runc" ]] || add --runtime "${RUNTIME}"

# Resources.
res '.HostConfig.Memory' --memory
res '.HostConfig.MemoryReservation' --memory-reservation
res '.HostConfig.MemorySwap' --memory-swap
res '.HostConfig.MemorySwappiness' --memory-swappiness
NANO="$(jq -r '.HostConfig.NanoCpus // 0' <<<"${INSPECT}")"
[[ "${NANO}" == "0" || "${NANO}" == "null" ]] || add --cpus "$(jq -r '.HostConfig.NanoCpus / 1000000000' <<<"${INSPECT}")"
res '.HostConfig.CpuShares' --cpu-shares
res '.HostConfig.CpuQuota' --cpu-quota
res '.HostConfig.CpuPeriod' --cpu-period
CPUSET="$(jq -r '.HostConfig.CpusetCpus // ""' <<<"${INSPECT}")"; [[ -z "${CPUSET}" ]] || add --cpuset-cpus "${CPUSET}"
CPUSETM="$(jq -r '.HostConfig.CpusetMems // ""' <<<"${INSPECT}")"; [[ -z "${CPUSETM}" ]] || add --cpuset-mems "${CPUSETM}"
res '.HostConfig.PidsLimit' --pids-limit
[[ "$(jq -r '.HostConfig.OomKillDisable // false' <<<"${INSPECT}")" != "true" ]] || add --oom-kill-disable
res '.HostConfig.OomScoreAdj' --oom-score-adj
SHM="$(jq -r '.HostConfig.ShmSize // 0' <<<"${INSPECT}")"; [[ "${SHM}" == "0" || "${SHM}" == "67108864" ]] || add --shm-size "${SHM}"
res '.HostConfig.BlkioWeight' --blkio-weight
while IFS=$'\x1f' read -r n s h; do [[ -n "${n}" ]] && add --ulimit "${n}=${s}:${h}"; done < <(jq -r '.HostConfig.Ulimits // [] | .[] | [.Name, .Soft, .Hard] | join("\u001f")' <<<"${INSPECT}")
while IFS=$'\x1f' read -r k v; do [[ -n "${k}" ]] && add --sysctl "${k}=${v}"; done < <(jq -r '.HostConfig.Sysctls // {} | to_entries[] | [.key, .value] | join("\u001f")' <<<"${INSPECT}")

# DNS, hosts, logging, stop behaviour.
add_each --dns '.HostConfig.Dns'
add_each --dns-search '.HostConfig.DnsSearch'
add_each --dns-option '.HostConfig.DnsOptions'
add_each --add-host '.HostConfig.ExtraHosts'
LOGTYPE="$(jq -r '.HostConfig.LogConfig.Type // ""' <<<"${INSPECT}")"
if [[ -n "${LOGTYPE}" && "${LOGTYPE}" != "json-file" ]] || [[ "$(jq -r '.HostConfig.LogConfig.Config // {} | length' <<<"${INSPECT}")" != "0" ]]; then
    [[ -z "${LOGTYPE}" ]] || add --log-driver "${LOGTYPE}"
    while IFS=$'\x1f' read -r k v; do [[ -n "${k}" ]] && add --log-opt "${k}=${v}"; done < <(jq -r '.HostConfig.LogConfig.Config // {} | to_entries[] | [.key, .value] | join("\u001f")' <<<"${INSPECT}")
fi
STOPSIG="$(jq -r '.Config.StopSignal // ""' <<<"${INSPECT}")"; IMG_STOPSIG="$(jq -r '.Config.StopSignal // ""' <<<"${IMAGE_INSPECT}")"
[[ "${STOPSIG}" == "${IMG_STOPSIG}" ]] || add --stop-signal "${STOPSIG}"
STOPT="$(jq -r '.Config.StopTimeout // ""' <<<"${INSPECT}")"; [[ -z "${STOPT}" || "${STOPT}" == "null" ]] || add --stop-timeout "${STOPT}"

# Labels that are not from the image.
while IFS=$'\x1f' read -r k v; do [[ -n "${k}" ]] && add --label "${k}=${v}"; done < <(jq -r --argjson img "$(jq '.Config.Labels // {}' <<<"${IMAGE_INSPECT}")" '.Config.Labels // {} | to_entries[] | select($img[.key] != .value) | [.key, .value] | join("\u001f")' <<<"${INSPECT}")

# Environment: only variables that differ from the image, with DOCKER_MODS
# extended, then --set/--unset applied.
mapfile -t ENV_LINES < <(jq -r --argjson img "$(jq '.Config.Env // []' <<<"${IMAGE_INSPECT}")" '.Config.Env // [] | .[] | select(. as $e | ($img | index($e)) == null)' <<<"${INSPECT}")
declare -A ENVMAP=()
declare -a ENVORDER=()
for line in "${ENV_LINES[@]}"; do
    k="${line%%=*}"; v="${line#*=}"
    [[ -n "${ENVMAP[${k}]+x}" ]] || ENVORDER+=("${k}")
    ENVMAP["${k}"]="${v}"
done
if [[ -n "${ENVMAP[DOCKER_MODS]+x}" ]]; then
    current="${ENVMAP[DOCKER_MODS]}"
    if [[ "|${current}|" == *"|${MOD}|"* ]]; then
        log "DOCKER_MODS already contains ${MOD}; leaving it unchanged"
    elif [[ -z "${current}" ]]; then
        ENVMAP[DOCKER_MODS]="${MOD}"
    else
        ENVMAP[DOCKER_MODS]="${current}|${MOD}"
    fi
else
    ENVORDER+=(DOCKER_MODS); ENVMAP[DOCKER_MODS]="${MOD}"
fi
for kv in "${SET_ENV[@]}"; do
    k="${kv%%=*}"; v="${kv#*=}"
    [[ -n "${ENVMAP[${k}]+x}" ]] || ENVORDER+=("${k}")
    ENVMAP["${k}"]="${v}"
done
for k in "${UNSET_ENV[@]}"; do unset "ENVMAP[${k}]"; done
for k in "${ENVORDER[@]}"; do [[ -n "${ENVMAP[${k}]+x}" ]] && add -e "${k}=${ENVMAP[${k}]}"; done

# Entrypoint/command only when they differ from the image.
if [[ "$(jq -c '.Config.Entrypoint' <<<"${INSPECT}")" != "$(jq -c '.Config.Entrypoint' <<<"${IMAGE_INSPECT}")" ]]; then
    EP="$(jq -r '.Config.Entrypoint // [] | join(" ")' <<<"${INSPECT}")"; add --entrypoint "${EP}"
fi
add "${IMAGE}"
if [[ "$(jq -c '.Config.Cmd' <<<"${INSPECT}")" != "$(jq -c '.Config.Cmd' <<<"${IMAGE_INSPECT}")" ]]; then
    while IFS= read -r c; do add "${c}"; done < <(jq -r '.Config.Cmd // [] | .[]' <<<"${INSPECT}")
fi

print_cmd() {
    # One flag (with its value) per line, secrets masked unless --show-secrets.
    local lines=("docker run -d --name $(printf '%q' "${NAME}")") a i n=${#ARGS[@]}
    for (( i = 0; i < n; i++ )); do
        a="${ARGS[i]}"
        if [[ "${a}" == -* && $(( i + 1 )) -lt "${n}" && "${ARGS[i+1]}" != -* ]]; then
            lines+=("$(printf '%q %s' "${a}" "$(mask_arg "${ARGS[i+1]}")")")
            i=$(( i + 1 ))
        else
            lines+=("$(mask_arg "${a}")")
        fi
    done
    printf '%s \\\n  ' "${lines[@]}" | sed '$ s/ \\$//'; echo
    for n in "${EXTRA_NETWORKS[@]+"${EXTRA_NETWORKS[@]}"}"; do
        IFS=$'\x1f' read -r netname aliases ipv4 ipv6 <<<"${n}"
        printf 'docker network connect'; [[ -z "${ipv4}" ]] || printf ' --ip %q' "${ipv4}"; [[ -z "${ipv6}" ]] || printf ' --ip6 %q' "${ipv6}"
        for a in ${aliases}; do printf ' --alias %q' "${a}"; done
        printf ' %q %q\n' "${netname}" "${NAME}"
    done
}

log "image: ${IMAGE} (container was created from ${IMAGE_REF})"
if [[ "${APPLY}" != true ]]; then
    log "dry run; the following would be executed (add --apply to run it):"
    print_cmd
    exit 0
fi

# ---- apply ----------------------------------------------------------------
STAMP="$(date -u +%Y%m%dT%H%M%SZ)"
BACKUP="${NAME}${BACKUP_SUFFIX}${STAMP}"
STAGING="${NAME}.new-4k-guard.${STAMP}"

# 1. Create (do not start) the new container under a staging name while the
#    old one keeps running, then compare both inspect documents. Anything
#    this script failed to carry over shows up as a difference and stops the
#    process before the running container is touched.
if ! NEW_ID="$(docker create --name "${STAGING}" "${ARGS[@]}")"; then
    docker rm -f "${STAGING}" >/dev/null 2>&1 || true
    die "docker create failed"
fi
log "staged ${STAGING} (${NEW_ID:0:12}); comparing with ${NAME}"
NORMALIZE='
  def prune: walk(if type == "object" then with_entries(select(.value != null and .value != [] and .value != {} and .value != "" and .value != false and .value != 0))
                  elif type == "array" then map(select(. != null)) else . end);
  def netmode: if . == "default" then "bridge" else . end;
  {
    hostconfig: (.HostConfig | del(.ConsoleSize, .ContainerIDFile) | .NetworkMode |= netmode | .Binds |= ((. // []) | sort) | .Mounts |= ((. // []) | sort_by(.Target)) | prune),
    config: (.Config | del(.Hostname, .Image, .Env, .Cmd, .Entrypoint, .AttachStdin, .AttachStdout, .AttachStderr) | prune),
    cmd: [.Config.Entrypoint, .Config.Cmd],
    exposed: (.Config.ExposedPorts // {} | keys),
    env: ([.Config.Env[] | select(startswith("DOCKER_MODS=") | not)] | sort)
  }'
OLD_NORM="$(jq -S --argjson changed "$(printf '%s\n' "${SET_ENV[@]+"${SET_ENV[@]}"}" "${UNSET_ENV[@]+"${UNSET_ENV[@]}"}" | sed 's/=.*//' | grep -v '^$' | jq -R . | jq -s .)" "${NORMALIZE} | .env |= map(select((split(\"=\")[0]) as \$k | (\$changed | index(\$k)) == null))" <<<"${INSPECT}")"
NEW_NORM="$(docker inspect "${STAGING}" | jq -S --argjson changed "$(printf '%s\n' "${SET_ENV[@]+"${SET_ENV[@]}"}" "${UNSET_ENV[@]+"${UNSET_ENV[@]}"}" | sed 's/=.*//' | grep -v '^$' | jq -R . | jq -s .)" ".[0] | ${NORMALIZE} | .env |= map(select((split(\"=\")[0]) as \$k | (\$changed | index(\$k)) == null))")"
if ! DIFF="$(diff -u <(printf '%s\n' "${OLD_NORM}") <(printf '%s\n' "${NEW_NORM}"))"; then
    printf '%s\n' "${DIFF}" >&2
    if [[ "${FORCE}" != true ]]; then
        docker rm -f "${STAGING}" >/dev/null 2>&1 || true
        die "the staged container differs from ${NAME} beyond the intended environment change (see diff above); nothing was changed. Fix the script or re-run with --force"
    fi
    log "differences found but --force given; continuing"
else
    log "staged container matches ${NAME} (only the intended environment change differs)"
fi

# 2. Swap: stop and rename the old one, rename and start the new one.
log "stopping ${NAME}"
docker stop "${NAME}" >/dev/null
log "renaming ${NAME} -> ${BACKUP}"
docker rename "${NAME}" "${BACKUP}"
docker rename "${STAGING}" "${NAME}"

rollback() {
    log "rolling back: removing new container and restoring ${BACKUP}"
    docker rm -f "${NAME}" >/dev/null 2>&1 || true
    docker rename "${BACKUP}" "${NAME}"
    docker start "${NAME}" >/dev/null
}
if ! docker start "${NAME}" >/dev/null; then
    rollback; die "docker start failed"
fi
log "started ${NEW_ID:0:12}"
for n in "${EXTRA_NETWORKS[@]+"${EXTRA_NETWORKS[@]}"}"; do
    IFS=$'\x1f' read -r netname aliases ipv4 ipv6 <<<"${n}"
    cargs=(docker network connect)
    [[ -z "${ipv4}" ]] || cargs+=(--ip "${ipv4}")
    [[ -z "${ipv6}" ]] || cargs+=(--ip6 "${ipv6}")
    for a in ${aliases}; do cargs+=(--alias "${a}"); done
    cargs+=("${netname}" "${NAME}")
    if ! "${cargs[@]}"; then rollback; die "connecting network ${netname} failed"; fi
done

log "waiting up to ${WAIT}s for ${NAME} to be healthy"
deadline=$(( $(date +%s) + WAIT ))
while :; do
    state="$(docker inspect -f '{{.State.Status}}' "${NAME}" 2>/dev/null || echo missing)"
    health="$(docker inspect -f '{{if .State.Health}}{{.State.Health.Status}}{{else}}none{{end}}' "${NAME}" 2>/dev/null || echo none)"
    if [[ "${state}" == "running" && ( "${health}" == "healthy" || "${health}" == "none" ) ]]; then
        if [[ "${health}" == "none" ]]; then sleep 5; state="$(docker inspect -f '{{.State.Status}}' "${NAME}")"; fi
        [[ "${state}" == "running" ]] && break
    fi
    if [[ "${state}" != "running" && "${state}" != "created" ]] || (( $(date +%s) >= deadline )); then
        docker logs --tail 40 "${NAME}" 2>&1 || true
        rollback; die "new container did not become healthy (state=${state} health=${health})"
    fi
    sleep 3
done
log "${NAME} is ${state} (health: ${health})"
if [[ "${KEEP_BACKUP}" == true ]]; then
    log "backup kept as ${BACKUP} (stopped); remove it with: docker rm ${BACKUP}"
else
    docker rm "${BACKUP}" >/dev/null
    log "backup ${BACKUP} removed"
fi
docker inspect -f '{{range .Config.Env}}{{println .}}{{end}}' "${NAME}" | grep '^DOCKER_MODS=' >&2 || true
