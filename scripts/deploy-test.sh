#!/usr/bin/env bash
# Deploy test: run Bosun as its compose service on this Docker host, the way a
# user runs it, and check the whole thing end to end. The Go tests drive the
# gate from outside; this is the only test of the gate inside its container:
# finding itself, the socket group from compose, starting the updater, the
# restore helper, and stopping cleanly.
#
# Runs on Linux with Docker Engine and on a Mac with Docker Desktop. CI runs it
# on Linux. It creates only bosunprobe*/bosun-probe* things and removes them.
#
# Usage: scripts/deploy-test.sh   (exits non-zero if any check fails)
set -u
REPO=$(cd "$(dirname "$0")/.." && pwd)
P=bosunprobe
PORT=5066
IMG=localhost:$PORT/bosun-probe
APP=bosun-probe-app
PASS=0
FAIL=0
ok()      { echo "PASS  $1"; PASS=$((PASS + 1)); }
bad()     { echo "FAIL  $1"; FAIL=$((FAIL + 1)); }
check()   { local d=$1; shift; if "$@" >/dev/null 2>&1; then ok "$d"; else bad "$d"; fi; }
has()     { grep -qF -- "$2" <<<"$1"; }
ver()     { [ "$(docker exec $APP cat /version 2>/dev/null)" = "$1" ]; }
running() { [ "$(docker inspect -f '{{.State.Running}}' "$1" 2>/dev/null)" = true ]; }
waitfor() { local i; for i in $(seq 1 90); do "$@" && return 0; sleep 1; done; return 1; }
compose() { docker compose -p $P --project-directory "$REPO" -f "$REPO/compose.yml" "$@"; }

cleanup() {
  compose down -v --remove-orphans >/dev/null 2>&1
  docker rm -f $APP bosun-probe-registry >/dev/null 2>&1
  docker ps -aq --filter label=bosun.managed-by=bosun-gate | xargs -r docker rm -f >/dev/null 2>&1
  docker volume rm -f bosun-probe-data >/dev/null 2>&1
  docker images --format '{{.Repository}}:{{.Tag}}' |
    grep -E "^(bosun/prev/$APP|$IMG|bosunprobe-build|${P}-bosun)" |
    xargs -r docker rmi -f >/dev/null 2>&1
}
trap cleanup EXIT
cleanup

# Build one version of the test app, push it as :latest, print its digest.
# It builds under its own tag first: tagging straight over :latest can let the
# image store drop the running container's image.
mk() {
  local d; d=$(mktemp -d)
  printf 'FROM busybox:1.36\nRUN echo %s > /version\nCMD %s\n' "$1" "$2" >"$d/Dockerfile"
  docker build -q -t "bosunprobe-build:$1" "$d" >/dev/null &&
    docker tag "bosunprobe-build:$1" $IMG:latest &&
    docker push -q $IMG:latest >/dev/null
  rm -rf "$d"
  docker inspect -f '{{range .RepoDigests}}{{println .}}{{end}}' $IMG:latest | grep "^$IMG@" | head -1 | cut -d@ -f2
}

# Call the gate's RPC as the updater would: as the gate's own user, over its
# socket. The updater cannot reach a registry on the host's localhost from
# inside its container, so this test hands the gate the digest itself.
rpc() {
  docker run --rm --user 65532:65532 -v ${P}_bosun-run:/run/bosun curlimages/curl:8.10.1 \
    -s --max-time 240 --unix-socket /run/bosun/gate.sock -X POST "http://gate/$1" \
    -H 'Content-Type: application/json' -d "$2"
}

# Exactly as the README tells a user to do it.
export DOCKER_GID
DOCKER_GID=$(docker run --rm -v /var/run/docker.sock:/s busybox:1.36 stat -c %g /s)
echo "== $(docker info -f '{{.OperatingSystem}}') · $(docker info -f '{{.Architecture}}') · engine $(docker info -f '{{.ServerVersion}}') · DOCKER_GID=$DOCKER_GID"

docker run -d --name bosun-probe-registry -p $PORT:5000 registry:2 >/dev/null
waitfor docker exec bosun-probe-registry true >/dev/null 2>&1

d1=$(mk v1 '["sleep","7200"]')
check "v1 of the test app pushed" test -n "$d1"
docker run -d --name $APP --label bosun.enable=true --label bosun.backup=true \
  --label bosun.health-timeout=5s -v bosun-probe-data:/data $IMG:latest >/dev/null
docker exec $APP sh -c 'echo before > /data/before'

clog=$(mktemp)
if ! compose up -d --build >"$clog" 2>&1; then
  echo "compose up failed:"
  tail -20 "$clog"
fi
check "gate container runs" waitfor running bosun-gate
check "gate started its updater" waitfor running bosun-updater
check "updater uses the gate image by ID" sh -c '[ "$(docker inspect -f {{.Image}} bosun-updater)" = "$(docker inspect -f {{.Image}} bosun-gate)" ]'
check "updater has no docker.sock" sh -c '! docker inspect -f "{{range .Mounts}}{{.Source}} {{.Destination}} {{end}}" bosun-updater | grep -q docker.sock'
check "gate runs as non-root 65532" sh -c '[ "$(docker inspect -f {{.Config.User}} bosun-gate)" = 65532 ]'
out=$(docker exec bosun-gate bosun status 2>&1)
check "bosun status sees the app" has "$out" "$APP"

d2=$(mk v2 '["sleep","7200"]')
out=$(rpc update "{\"name\":\"$APP\",\"digest\":\"$d2\",\"auth\":\"\"}")
echo "      update to v2: ${out:0:200}"
check "update to v2 finished" has "$out" '"status":"done"'
check "app runs v2" ver v2
check "a backup was taken" has "$out" 'backup_bytes'

docker exec $APP sh -c 'echo after > /data/after'
out=$(docker exec bosun-gate bosun rollback $APP --with-data --yes 2>&1)
echo "      rollback --with-data: ${out:0:200}"
check "rollback with data finished" has "$out" "rolled back"
check "app runs v1 again" ver v1
check "data from before the update is back" docker exec $APP test -f /data/before
check "data written after the update is gone" sh -c "! docker exec $APP test -f /data/after"

d3=$(mk broken '["false"]')
out=$(rpc update "{\"name\":\"$APP\",\"digest\":\"$d3\",\"auth\":\"\"}")
echo "      broken update: ${out:0:200}"
check "a broken version rolls back by itself" has "$out" '"status":"reverted"'
check "app still runs v1" ver v1

out=$(docker exec bosun-gate bosun check --dry-run 2>&1)
check "a round runs from the CLI through the updater and the gate" has "$out" "$APP"

compose stop >/dev/null 2>&1
check "stopping the gate removes the updater" sh -c '! docker inspect bosun-updater >/dev/null 2>&1'

if [ $FAIL -gt 0 ]; then
  echo "---- Bosun logs ----"
  compose logs --no-color --tail 40 2>/dev/null
fi
echo "== $PASS passed, $FAIL failed"
[ $FAIL -eq 0 ]
