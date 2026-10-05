#!/bin/sh
# Exercise the real image locally and in CI. The caller owns the supplied image;
# this script cleans only its own disposable containers and persistent volume.
set -eu
IMG="${1:-hfdesk:ci}"
PREFIX="hfdesk-smoke-$(date +%s)-$$"
CONTAINER="$PREFIX-app"
VOLUME="$PREFIX-data"
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
stop_app() {
    docker stop -t 15 "$CONTAINER" >/dev/null
    test "$(docker inspect -f '{{.State.ExitCode}}' "$CONTAINER")" = 0
    docker rm "$CONTAINER" >/dev/null
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
for ids in 568:568 65534:100; do
    start_app --user "$ids" -e PUID=4321 -e PGID=4322
    assert_identity "${ids%:*}" "${ids#*:}"
    stop_app
    echo "PASS non-root startup $ids: caller identity overrides PUID/PGID, unchanged accounts"
done

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
