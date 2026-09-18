#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
cd "$root"
source "$root/scripts/common.sh"
[[ $(uname -s) == Linux ]] || fail 'The development runner currently supports Linux.'
for tool in flock timeout; do need "$tool"; done

command=${1:-}
disposable=0 db_started=0 api_started=0 backend='' owner=''
case "$command" in
    dev-up|dev-down|dev-status) state="$root/.dev/dev" ;;
    test|check|smoke)
        mkdir -p "$root/.dev/runs"
        state=$(mktemp -d "$root/.dev/runs/$command.XXXXXXXX")
        disposable=1
        ;;
    clean-run)
        state=$(realpath -e -- "${2:?Provide a retained .dev/runs directory}")
        [[ $state == "$root/.dev/runs/"* && $(dirname "$state") == "$root/.dev/runs" ]] || fail 'Expected a direct child of .dev/runs.'
        disposable=1
        ;;
    *) fail 'Usage: bash scripts/dev.sh {dev-up|dev-down|dev-status|test|check|smoke|clean-run DIR}' ;;
esac
mkdir -p "$state"
exec 9>"$state/lock"
flock -w 60 9 || fail "Lifecycle command already active for $state"

cleanup() {
    local result=$? cleanup_failed=0
    trap - EXIT INT TERM
    set +e
    if [[ $command != dev-status ]]; then stop_process job || cleanup_failed=1; fi
    if [[ $disposable == 1 ]]; then
        stop_process api || cleanup_failed=1
        if [[ -n $backend ]]; then remove_database || cleanup_failed=1; fi
    elif [[ $result != 0 ]]; then
        if [[ $api_started == 1 ]]; then stop_process api || cleanup_failed=1; fi
        if [[ $db_started == 1 ]]; then stop_database || cleanup_failed=1; fi
    fi
    [[ $cleanup_failed == 0 ]] || result=1
    if [[ $disposable == 1 && $result == 0 ]]; then
        rm -rf "$state"
    elif [[ $result != 0 ]]; then
        echo "Command failed (status $result). Diagnostics: $state" >&2
        if [[ $disposable == 1 && $cleanup_failed == 0 ]]; then
            rm -f "$state/password" "$state/csrf-key" "$state/cursor-key" "$state/server" "$state/server.next" "$state/db"
        fi
    fi
    exit "$result"
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

if [[ $command == dev-down || $command == dev-status || $command == clean-run ]]; then
    if [[ ! -f $state/backend ]]; then echo 'No managed environment.'; exit 0; fi
    # Shutdown/recovery needs identity, not credentials or installed Go binaries.
    backend=$(cat "$state/backend")
    owner=$(cat "$state/owner")
    if [[ $command == dev-down ]]; then
        stop_process job
        stop_process api
        stop_database
        echo 'Development services stopped; data preserved.'
    elif [[ $command == clean-run ]]; then
        echo "Cleaning abandoned run: $state"
    else
        echo "Database backend: $backend; $(cat "$state/pg-version")"
        if database_running; then echo 'Database: running'; else echo 'Database: stopped (or stale metadata)'; fi
        if owned_running "$state/api.pid"; then echo 'API: running'; else echo 'API: stopped (or stale metadata)'; fi
        if [[ -f $state/api-origin ]]; then
            origin=$(cat "$state/api-origin")
            echo "API address: $origin"
            if owned_running "$state/api.pid" && curl --noproxy '*' -fsS --max-time 2 "$origin/readyz" >/dev/null 2>&1; then
                echo 'Readiness: ready'
            else echo 'Readiness: not ready'; fi
        fi
        echo "Logs: $state/api.log, $state/postgres.log, $state/commands.log"
    fi
    exit 0
fi

for tool in setsid python3 curl openssl go; do need "$tool"; done
init_settings
stop_process job
echo "Runtime artifacts: $state"
if [[ $command == dev-up || $command == smoke ]]; then
    # Build before touching an existing development API.
    run go build -o "$state/server.next" ./cmd/server
    run go build -o "$state/db" ./cmd/db
fi
start_database

case "$command" in
    dev-up|smoke)
        run "$state/db" migrate
        run "$state/db" seed
        stop_process api
        mv "$state/server.next" "$state/server"
        if [[ $command == smoke ]]; then
            # Smoke does not depend on interactive development overrides.
            unset DEV_CLIENT_ORIGINS
            start_api 1 0
            run python3 scripts/smoke.py "$API_PUBLIC_ORIGIN"
        else
            start_api 0 "${DEV_PORT:-8080}"
            echo "Logs: $state/api.log, $state/postgres.log, $state/commands.log"
        fi
        ;;
    test|check)
        args=()
        if [[ -n ${TEST_ARGS:-} ]]; then read -r -a args <<< "$TEST_ARGS"; fi
        if [[ $command == check ]]; then
            [[ ${#args[@]} == 0 ]] || fail 'TEST_ARGS is supported only by make test; make check always runs the full suite.'
            run go build ./...
            run go vet ./...
        fi
        run go test ./... "${args[@]}" -count=1
        if [[ $command == check ]]; then run go test -race ./... -count=1; fi
        ;;
esac
