#!/bin/sh
# Exercise the real image locally and in CI. The caller owns the supplied image;
# this script cleans only its own disposable containers and persistent volume.
set -eu
IMG="${1:-hfdesk:ci}"
PREFIX="hfdesk-smoke-$(date +%s)-$$"
CONTAINER="$PREFIX-app"
VOLUME="$PREFIX-data"
SHARED_VOLUME="$PREFIX-shared"
cleanup() {
    code=$?
    trap - EXIT
    if docker container inspect "$CONTAINER" >/dev/null 2>&1; then
        docker logs "$CONTAINER" || code=1
        docker rm -f "$CONTAINER" >/dev/null || code=1
    fi
    if docker volume inspect "$VOLUME" >/dev/null 2>&1; then
        docker volume rm "$VOLUME" >/dev/null || code=1
    fi
    if docker volume inspect "$SHARED_VOLUME" >/dev/null 2>&1; then
        docker volume rm "$SHARED_VOLUME" >/dev/null || code=1
    fi
    exit "$code"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

ACCOUNTS=$(docker run --rm --entrypoint /bin/sh "$IMG" -c 'sha256sum /etc/passwd /etc/group')
DEFAULT_UID=$(docker run --rm --entrypoint /bin/sh "$IMG" -c 'printf %s "$HFDESK_UID"')
DEFAULT_GID=$(docker run --rm --entrypoint /bin/sh "$IMG" -c 'printf %s "$HFDESK_GID"')
assert_accounts() {
    actual=$(docker exec "$CONTAINER" sha256sum /etc/passwd /etc/group)
    if [ "$actual" != "$ACCOUNTS" ]; then
        printf 'Account databases changed!\nImage:\n%s\nRuntime:\n%s\n' "$ACCOUNTS" "$actual" >&2
        exit 1
    fi
}
start_app() {
    docker run -d --name "$CONTAINER" "$@" "$IMG" --no-open --port 8080 >/dev/null
    tries=0
    until docker exec "$CONTAINER" wget -q -O /dev/null http://127.0.0.1:8080/api/health; do
        tries=$((tries + 1))
        if [ "$tries" -ge 30 ] || [ "$(docker inspect -f '{{.State.Running}}' "$CONTAINER")" != true ]; then
            echo 'HFDesk failed to become healthy' >&2
            exit 1
        fi
        sleep 1
    done
}
assert_identity() {
    # Check the real HFDesk PID 1, not the identity of a docker-exec shell.
    # Read /proc/1/exe as the app UID: an exec-root process without CAP_SYS_PTRACE
    # cannot inspect that symlink after the root-to-nonroot privilege drop.
    docker exec --user "$1:$2" "$CONTAINER" /bin/sh -ec '
        test "$(readlink /proc/1/exe)" = /usr/local/bin/hfdesk
        test "$(awk "/^Uid:/ {print \$2, \$3, \$4, \$5}" /proc/1/status)" = "$1 $1 $1 $1"
        test "$(awk "/^Gid:/ {print \$2, \$3, \$4, \$5}" /proc/1/status)" = "$2 $2 $2 $2"
    ' sh "$1" "$2"
    assert_accounts
}
# The per-account coordination carrier resolves its base from the operating
# system account. The image creates no user or group, sets no USER, and runs
# the app under a numeric uid, so the documented NAS configuration has no
# /etc/passwd entry for the app at all. The cache rebuild takes that
# coordination lease before it touches the filesystem, so it exercises the
# resolution without any Hub access and without downloading a model.
assert_coordination_lease() {
    response=$(docker exec --user "$1:$2" "$CONTAINER" wget -q -T 60 -O - \
        --header='Content-Type: application/json' --post-data='{"clean":false}' \
        http://127.0.0.1:8080/api/cache/rebuild 2>&1) || {
        printf 'FAIL coordination lease under %s:%s: %s\n' "$1" "$2" "$response" >&2
        dump_rebuild_response "$1:$2"
        exit 1
    }
    if ! printf '%s\n' "$response" | grep -q '"success":true'; then
        printf 'FAIL coordination lease under %s:%s: %s\n' "$1" "$2" "$response" >&2
        dump_rebuild_response "$1:$2"
        exit 1
    fi
    # A successful answer alone does not prove the account was resolved: a
    # carrier the app cannot establish is reported as a degraded coordination
    # and the request still succeeds. The lease objects on disk are what prove
    # it: they live in a private directory owned by the app account, under the
    # data root that is the account's private base in the container.
    #
    # The objects are located by the account that owns them, so the assertion
    # is about this account's carrier and not about an arbitrary lock file
    # that happens to exist under the data root. The carrier is private: its
    # own directory, and no access to it or to its objects for any other
    # account. The mode is asserted as that property rather than as one exact
    # number, so the assertion states what the carrier must guarantee instead
    # of how it happens to be spelled today.
    if ! docker exec --user "$1:$2" "$CONTAINER" /bin/sh -ec '
        uid=$1
        dirs=
        for object in $(find /data -type f -name "*.lock" 2>/dev/null); do
            [ "$(stat -c %u "$object" 2>/dev/null)" = "$uid" ] || continue
            dir=$(dirname "$object")
            case " $dirs " in
                *" $dir "*) ;;
                *) dirs="$dirs $dir" ;;
            esac
        done
        test -n "$dirs"
        for dir in $dirs; do
            test "$(stat -c %u "$dir")" = "$uid"
            case "$(stat -c %a "$dir")" in
                *00) ;;
                *) exit 1 ;;
            esac
            for object in "$dir"/*.lock; do
                test -f "$object"
                test "$(stat -c %u "$object")" = "$uid"
                case "$(stat -c %a "$object")" in
                    *00) ;;
                    *) exit 1 ;;
                esac
            done
        done
    ' sh "$1"; then
        printf 'FAIL coordination carrier under %s:%s: no private per-account carrier under /data\n' "$1" "$2" >&2
        exit 1
    fi
}
# Print the coordination carrier directory of one account: the directory that
# holds the lease objects this account owns.
#
# The lookup is scoped to the carrier's own location by the ownership of its
# objects. An unrelated object elsewhere under the data root therefore cannot
# satisfy it, and a carrier that belongs to another account is never mistaken
# for this account's own. A coordination that degraded to the in-process half
# leaves no object on disk, so an empty answer is the degraded case.
coordination_carrier_path() {
    docker exec --user "$1:$2" "$CONTAINER" /bin/sh -ec '
        uid=$1
        for object in $(find /data -type f -name "*.lock" 2>/dev/null); do
            [ "$(stat -c %u "$object" 2>/dev/null)" = "$uid" ] || continue
            dirname "$object"
        done | sort -u
        exit 0
    ' sh "$1"
}
# BusyBox wget drops the body of an error response, so the failed request is
# repeated verbatim to show the server's own reason in the job log. The
# connection is held open while the answer is read: a client that closes it
# right away cancels the request, and the server then reports the cancelled
# request instead of the reason the request failed.
dump_rebuild_response() {
    docker exec --user "$1" "$CONTAINER" /bin/sh -ec '
        command -v nc >/dev/null 2>&1 || exit 0
        { printf "POST /api/cache/rebuild HTTP/1.0\r\nContent-Type: application/json\r\nContent-Length: 15\r\n\r\n{\"clean\":false}"; sleep 3; } | nc 127.0.0.1 8080
    ' >&2 || true
}
stop_app() {
    docker stop -t 15 "$CONTAINER" >/dev/null
    test "$(docker inspect -f '{{.State.ExitCode}}' "$CONTAINER")" = 0
    docker rm "$CONTAINER" >/dev/null
}
# Stop the container of a shared data root run.
#
# The app's own state directory under a shared data root belongs to the
# account that ran first, so the next account cannot persist its job snapshot
# there and its shutdown reports that error. That is the state directory of
# the app and not the coordination carrier, which this case does not cover:
# the container is only stopped so the next account can run against the same
# data root.
stop_app_shared() {
    docker stop -t 15 "$CONTAINER" >/dev/null || true
    docker rm -f "$CONTAINER" >/dev/null || true
}

# Account immutability and actual process identity: default, arbitrary, one-ID
# overrides, reserved UID/GID collisions, and the deliberate root opt-in.
for mode in default arbitrary uid-only gid-only uid-collision gid-collision collision root; do
    case "$mode" in
        default) uid=$DEFAULT_UID; gid=$DEFAULT_GID; start_app ;;
        arbitrary) uid=1234; gid=1235; start_app -e PUID="$uid" -e PGID="$gid" ;;
        uid-only) uid=1234; gid=$DEFAULT_GID; start_app -e PUID="$uid" ;;
        gid-only) uid=$DEFAULT_UID; gid=1235; start_app -e PGID="$gid" ;;
        uid-collision) uid=65534; gid=$DEFAULT_GID; start_app -e PUID="$uid" ;;
        gid-collision) uid=$DEFAULT_UID; gid=100; start_app -e PGID="$gid" ;;
        collision) uid=65534; gid=100; start_app -e PUID="$uid" -e PGID="$gid" ;;
        root) uid=0; gid=0; start_app -e PUID=0 -e PGID=0 ;;
    esac
    assert_identity "$uid" "$gid"
    stop_app
    echo "PASS root startup $mode ($uid:$gid): HFDesk PID 1, unchanged accounts, graceful SIGTERM"
done

# The root privilege drop must pin HOME=/data even when the target UID has no
# NSS entry (documented NAS IDs). su-exec sets HOME from the target passwd entry
# and falls back to "/" when there is none, so the app must re-pin it. Read HOME
# from the real app process (/proc/1/environ), not a docker-exec shell, which
# would report its own environment.
#
# The same numeric UID must also be able to take the per-account coordination
# lease. The lease base is resolved from the operating system account, and a
# numeric UID without an NSS entry has no account record to resolve it from,
# so this is the configuration that broke the image's downloads. Two numeric
# accounts of one image must not share a carrier either: the container gives
# every UID the same HOME, so a shared carrier would not be a per-account one.
#
# These two probes run in separate containers with separate data roots, so
# they establish that the carrier path is derived per account. That two
# accounts really get separate carriers over one shared data root is the
# shared data root case below.
previous_carrier=''
for probe in 1026 568; do
    start_app -e PUID="$probe" -e PGID=100
    # exec as the app UID: an exec-root shell without CAP_SYS_PTRACE cannot read
    # /proc/1/environ once PID 1 has dropped to a different UID.
    docker exec --user "$probe:100" "$CONTAINER" /bin/sh -ec '
        tr "\0" "\n" < /proc/1/environ | grep -qx "HOME=/data"
    '
    # The premise of this case: the app UID really has no account record, so
    # the coordination base cannot come from /etc/passwd.
    docker exec "$CONTAINER" /bin/sh -ec "! getent passwd $probe"
    assert_identity "$probe" 100
    assert_coordination_lease "$probe" 100
    carrier=$(coordination_carrier_path "$probe" 100)
    if [ -z "$carrier" ]; then
        printf 'FAIL coordination carrier: PUID=%s has no carrier under /data\n' "$probe" >&2
        exit 1
    fi
    if [ -n "$previous_carrier" ] && [ "$carrier" = "$previous_carrier" ]; then
        printf 'FAIL coordination carrier: PUID=%s shares the carrier %s with another account\n' "$probe" "$carrier" >&2
        exit 1
    fi
    previous_carrier=$carrier
    stop_app
    echo "PASS root startup NSS-less PUID=$probe: PID 1 HOME=/data, coordination carrier lease"
done
# Two numeric accounts must be able to work against ONE shared data root. The
# data root is world-writable so that every account can create its own state
# there, and the per-account coordination carrier is one of those states: a
# private directory of a single account under the shared cache area of the
# data root.
#
# The account that runs first must not tighten the shared cache area. The
# coordination carrier is created with the private mode of its own leaf, and a
# shared parent created with that mode becomes a private directory of the
# first account: the next account can no longer resolve the app's cache under
# that root, cannot create a carrier of its own, and its coordination request
# is refused. The data root is world-writable precisely so that any account
# can create its own state there.
docker volume create "$SHARED_VOLUME" >/dev/null
shared_state=''
first_carrier=''
for probe in 1026 568; do
    start_app -e PUID="$probe" -e PGID=100 -v "$SHARED_VOLUME:/data"
    assert_identity "$probe" 100
    # The coordination request is the one that takes the cross-process lease.
    # It must be answered for every account that shares the data root.
    assert_coordination_lease "$probe" 100
    carrier=$(coordination_carrier_path "$probe" 100)
    if [ -z "$carrier" ]; then
        printf 'FAIL shared data root: PUID=%s has no coordination carrier of its own\n' "$probe" >&2
        exit 1
    fi
    if [ -n "$first_carrier" ] && [ "$carrier" = "$first_carrier" ]; then
        printf 'FAIL shared data root: PUID=%s shares the carrier %s with another account\n' "$probe" "$carrier" >&2
        exit 1
    fi
    first_carrier=$carrier
    # The account must be able to reach the app's cache under the shared cache
    # area, not only the data root itself.
    if ! docker exec --user "$probe:100" "$CONTAINER" /bin/sh -ec '
        test -x "$(dirname "$HF_HOME")"
        test -d "$HF_HOME"
        test -x "$HF_HOME"
    '; then
        printf 'FAIL shared data root: PUID=%s cannot reach the app cache under the shared cache area\n' "$probe" >&2
        exit 1
    fi
    # The shared cache area keeps the mode and the ownership the first account
    # found: the next account must not take it over either.
    state=$(docker exec "$CONTAINER" /bin/sh -ec 'stat -c "%u:%g:%a" "$(dirname "$HF_HOME")"') || state=''
    if [ -z "$state" ]; then
        printf 'FAIL shared data root: the shared cache area of PUID=%s cannot be inspected\n' "$probe" >&2
        exit 1
    fi
    if [ -z "$shared_state" ]; then
        shared_state=$state
    elif [ "$state" != "$shared_state" ]; then
        printf 'FAIL shared data root: the shared cache area changed from %s to %s\n' "$shared_state" "$state" >&2
        exit 1
    fi
    stop_app_shared
    echo "PASS shared data root PUID=$probe: own carrier $carrier, shared cache area $state"
done
# The shared cache area must not be private to the account that created it:
# every account has to be able to create its own state there, and the data
# root is world-writable for exactly that reason. A private shared area locks
# every other account out of the data root.
shared_mode=${shared_state##*:}
case "$shared_mode" in
    *00)
        printf 'FAIL shared data root: the shared cache area is private to one account (%s)\n' "$shared_state" >&2
        exit 1
        ;;
esac
for ids in 568:568 65534:100; do
    start_app --user "$ids" -e PUID=4321 -e PGID=4322
    assert_identity "${ids%:*}" "${ids#*:}"
    stop_app
    echo "PASS non-root startup $ids: caller identity overrides PUID/PGID, unchanged accounts"
done
# The same coordination resolution on the direct non-root exec path (docker run
# --user, k8s runAsUser), where the app inherits the image's HOME=/data and no
# privilege drop happens. uid 568 has no NSS entry here either. The container
# has no network at all, so this case also proves the exercised path needs
# neither the public Hub nor a model download.
start_app --network none --user 568:568
docker exec "$CONTAINER" /bin/sh -ec '! getent passwd 568'
assert_identity 568 568
assert_coordination_lease 568 568
stop_app
echo 'PASS non-root startup NSS-less 568:568: PID 1 identity, coordination carrier lease'

# No NSS entry, writable fresh persistent /data, read-only root, no capabilities,
# no privilege escalation. Save actual settings, inspect durable paths, recreate.
docker run --rm --entrypoint /bin/sh "$IMG" -ec '
    ! getent passwd 568
    ! getent group 568
    test "$HOME" = /data
    test "$XDG_CONFIG_HOME" = /data/.config
    test "$HF_HOME" = /data/.cache/huggingface
'
docker volume create "$VOLUME" >/dev/null
for pass in write restart; do
    start_app --user 568:568 --read-only --cap-drop=ALL --security-opt=no-new-privileges -v "$VOLUME:/data"
    assert_identity 568 568
    docker exec "$CONTAINER" /bin/sh -ec '
        test "$(awk "/^NoNewPrivs:/ {print \$2}" /proc/1/status)" = 1
        test "$(awk "/^CapEff:/ {print \$2}" /proc/1/status)" = 0000000000000000
    '
    if [ "$pass" = write ]; then
        response=$(docker exec "$CONTAINER" wget -q -O - --header='Content-Type: application/json' \
            --post-data='{"connections":7,"localDir":"/data/Models"}' http://127.0.0.1:8080/api/settings)
        printf '%s\n' "$response" | grep -q '"message":"Settings saved"'
    fi
    settings=$(docker exec "$CONTAINER" wget -q -O - http://127.0.0.1:8080/api/settings)
    printf '%s\n' "$settings" | grep -q '"connections":7'
    printf '%s\n' "$settings" | grep -q '"localDir":"/data/Models"'
    printf '%s\n' "$settings" | grep -q '"cacheDir":"/data/.cache/huggingface"'
    docker exec "$CONTAINER" /bin/sh -ec '
        test ! -e /data/hfdesk.json
        test "$(stat -c %u:%g /data/.config/HFDesk/hfdesk.json)" = 568:568
        test "$(stat -c %a /data/.config/HFDesk/hfdesk.json)" = 600
    '
    stop_app
    # A graceful real-app shutdown also persists the job snapshot without NSS.
    docker run --rm --user 568:568 --read-only --cap-drop=ALL --security-opt=no-new-privileges \
        -v "$VOLUME:/data" --entrypoint /bin/sh "$IMG" -ec '
            test -f /data/.config/HFDesk/jobs_state.json
            test "$(stat -c %u:%g /data/.config/HFDesk/jobs_state.json)" = 568:568
        '
    echo "PASS hardened NSS-less app $pass: settings and job state persist on /data"
done

# UMASK changes ordinary app-created files, including the direct non-root path.
for mode in root nonroot; do
    if [ "$mode" = root ]; then
        set --
    else
        set -- --user 568:568
    fi
    docker run --rm "$@" -e UMASK=002 -e HFDESK_BIN=/bin/sh "$IMG" -ec '
        touch /data/mask-file; mkdir /data/mask-dir
        test "$(stat -c %a /data/mask-file)" = 664
        test "$(stat -c %a /data/mask-dir)" = 775
    '
done
for probe in UMASK=9999 UMASK=7777 PUID=abc PGID=abc; do
    code=0
    docker run --rm -e "$probe" -e HFDESK_BIN=/bin/sh "$IMG" -c true || code=$?
    test "$code" = 64
done
code=0
docker run --rm --entrypoint /bin/sh "$IMG" -c '
    rmdir /data && ln -s /tmp /data && exec /usr/local/bin/entrypoint.sh
' || code=$?
test "$code" = 65

# Root startup must never traverse any planted path inside /data. Compare both
# target ownership and account bytes; also leave an ordinary child owned by root.
docker run --rm --entrypoint /bin/sh -e PUID=1234 -e PGID=1235 -e HFDESK_BIN=/bin/sh "$IMG" -ec '
    mkdir /tmp/victim /data/.cache
    touch /tmp/victim/file /data/old-file
    ln -s /etc /data/.config
    ln -s /usr/local/bin /data/.cache/huggingface
    ln -s /tmp/victim /data/Models
    before=$(stat -c "%u:%g:%a" /etc /etc/passwd /etc/group /usr/local/bin /usr/local/bin/hfdesk /tmp/victim /tmp/victim/file /data/old-file)
    accounts=$(sha256sum /etc/passwd /etc/group)
    /usr/local/bin/entrypoint.sh -ec "test \"\$(id -u):\$(id -g)\" = 1234:1235"
    test "$(stat -c %u:%g /data)" = 1234:1235
    test "$before" = "$(stat -c "%u:%g:%a" /etc /etc/passwd /etc/group /usr/local/bin /usr/local/bin/hfdesk /tmp/victim /tmp/victim/file /data/old-file)"
    test "$accounts" = "$(sha256sum /etc/passwd /etc/group)"
'
echo 'PASS UMASK, validation, fixed-root refusal, and inside-/data symlink/nonrecursive safety'
echo "PASS Docker runtime smoke: $IMG"
