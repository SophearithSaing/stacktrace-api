#!/usr/bin/env bash
set -Eeuo pipefail
umask 077

root=$(cd -- "$(dirname -- "${BASH_SOURCE[0]}")/.." && pwd -P)
cd "$root"
source "$root/scripts/common.sh"
[[ $(uname -s) == Linux ]] || fail 'The development runner currently supports Linux.'
for tool in flock timeout; do need "$tool"; done

command=${1:-}
disposable=0 db_started=0 api_started=0 backend='' owner='' response_ready=0
if [[ $command == live-response ]]; then
    # No noninteractive approval, inherited key, or shell tracing on the paid path.
    set +x
    unset TOGETHER_API_KEY
    [[ -t 0 && -t 1 ]] || fail 'live-response requires an interactive terminal; no provider call made.'
    echo 'One isolated local @golang response using Together meta-llama/Llama-3.3-70B-Instruct-Turbo.'
    echo 'Review seed/personas.json (Go version 1) and current Together model/pricing before proceeding.'
    echo 'https://docs.together.ai/docs/serverless-models  https://www.together.ai/pricing'
    echo 'One 132,096-token budget grant; no replenishment or second execution. Not a hard dollar/billing cap.'
    echo 'Synthetic question: @golang How would you keep a Go worker loop bounded and shut it down gracefully?'
    echo 'The disposable database is removed afterward; safe failure diagnostics are retained. Not MVP acceptance.'
    read -r -p 'Type LIVE to approve this paid test, or anything else to cancel: ' approval
    [[ $approval == LIVE ]] || fail 'Cancelled; no provider call made.'
fi
case "$command" in
    dev-up|dev-down|dev-status) state="$root/.dev/dev" ;;
    test|check|smoke|live-response|response-check)
        mkdir -p "$root/.dev/runs"
        state=$(mktemp -d "$root/.dev/runs/$command.XXXXXXXX")
        disposable=1
        ;;
    clean-run)
        state=$(realpath -e -- "${2:?Provide a retained .dev/runs directory}")
        [[ $state == "$root/.dev/runs/"* && $(dirname "$state") == "$root/.dev/runs" ]] || fail 'Expected a direct child of .dev/runs.'
        disposable=1
        ;;
    *) fail 'Usage: bash scripts/dev.sh {dev-up|dev-down|dev-status|test|check|smoke|live-response|response-check|clean-run DIR}' ;;
esac
mkdir -p "$state"
exec 9>"$state/lock"
flock -w 60 9 || fail "Lifecycle command already active for $state"

cleanup() {
    local result=$? cleanup_failed=0
    trap - EXIT INT TERM
    set +e
    unset TOGETHER_API_KEY together_key
    # Revoke publication authority before interrupting an in-flight paid call.
    if [[ $response_ready == 1 ]]; then
        timeout 35 "$state/admin" agent pause --all >>"$state/pause.log" 2>&1 || cleanup_failed=1
    fi
    if [[ $command != dev-status ]]; then stop_process job || cleanup_failed=1; fi
    if [[ $response_ready == 1 && $cleanup_failed == 0 && -f $state/response-fixture.json && ! -f $state/response-report.json ]]; then
        # Join the worker before snapshotting ambiguous accounting and removing data.
        timeout 80 python3 -E -B scripts/immediate_response.py "$state" report >"$state/cleanup-report.log" 2>&1 || cleanup_failed=1
    fi
    if [[ $disposable == 1 ]]; then
        stop_process api || cleanup_failed=1
        if [[ -n $backend ]]; then
            # Failed response cleanup must retain its database for recovery.
            if [[ $response_ready == 0 || $cleanup_failed == 0 ]]; then remove_database || cleanup_failed=1; fi
        fi
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
            rm -f "$state/password" "$state/csrf-key" "$state/cursor-key" "$state/server" "$state/server.next" "$state/db" "$state/worker" \
                 "$state/worker.test" "$state/admin" "$state/execution-command.json" "$state/execution-fixture.json" "$state/generation-fixture.json" "$state/response-policy.json"
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
# Managed verification never inherits live provider credentials or a harness entry.
unset TOGETHER_API_KEY STACKTRACE_GENERATION_SMOKE
stop_process job
echo "Runtime artifacts: $state"
if [[ $command == dev-up || $command == smoke || $command == live-response || $command == response-check ]]; then
    # Build before touching an existing development API.
    run go build -o "$state/server.next" ./cmd/server
    run go build -o "$state/db" ./cmd/db
fi
if [[ $command == smoke ]]; then
    run go build -o "$state/worker" ./cmd/worker
    run go test -c -o "$state/worker.test" ./cmd/worker
    run go build -o "$state/admin" ./cmd/admin
    export STACKTRACE_ADMIN="$state/admin"
fi
if [[ $command == live-response || $command == response-check ]]; then
    run go build -o "$state/worker" ./cmd/worker
    run go build -o "$state/admin" ./cmd/admin
    if [[ $command == response-check ]]; then run go test -c -o "$state/worker.test" ./cmd/worker; fi
fi
start_database

case "$command" in
    live-response|response-check)
        run "$state/db" migrate
        run "$state/db" seed
        response_ready=1
        run "$state/worker" check
        mv "$state/server.next" "$state/server"
        unset DEV_CLIENT_ORIGINS
        start_api 1 0
        run python3 -E -B scripts/immediate_response.py "$state" setup
        worker_result=0 verification_result=0
        if [[ $command == live-response ]]; then
            IFS= read -rsp 'Together API key (hidden; worker only): ' together_key
            echo
            [[ -n $together_key ]] || fail 'Empty key; no provider call made.'
            # Recheck freshness after the operator prompt, without delivering the key.
            run python3 -E -B scripts/immediate_response.py "$state" preflight
            : >"$state/job.log"
            # Open the private terminal before setsid detaches the owned worker.
            # Descriptor 3 bypasses job.log/commands.log; no provider body is saved.
            TOGETHER_API_KEY="$together_key" start_process job timeout --signal=TERM --kill-after=15s 90 "$state/worker" execute --provider-diagnostics 3>/dev/tty
            unset together_key
            read -r worker_pid _ <"$state/job.pid"
            wait "$worker_pid" || worker_result=$?
            rm -f "$state/job.pid"
            cat "$state/job.log"
            cat "$state/job.log" >>"$state/commands.log"
        else
            STACKTRACE_GENERATION_SMOKE="$state/execution-command.json" run "$state/worker.test" -test.run='^TestGenerationSmokeHarness$' -test.count=1 -test.timeout=90s || worker_result=$?
        fi
        run python3 -E -B scripts/immediate_response.py "$state" verify || verification_result=$?
        [[ $worker_result == 0 && $verification_result == 0 ]] || fail 'Immediate response incomplete; no retry or new grant was made.'
        ;;
    dev-up|smoke)
        if [[ $command == smoke ]]; then
            # SQL fixtures are disposable-smoke-only, not an operator enable command.
            if [[ $backend == native ]]; then smoke_sql=("$pg_bin/psql");
            else smoke_sql=(docker exec -i "stacktrace-$owner" psql -U stacktrace -d stacktrace); fi
            run python3 -B scripts/smoke_generation.py "$state" unmigrated "${smoke_sql[@]}"
        fi
        run "$state/db" migrate
        if [[ $command == smoke ]]; then
            run "$state/worker" check
            run python3 -B scripts/smoke_generation.py "$state" empty "${smoke_sql[@]}"
        fi
        run "$state/db" seed
        if [[ $command == smoke ]]; then
            run "$state/worker" check
            run python3 -B scripts/smoke_generation.py "$state" seeded "${smoke_sql[@]}"
        fi
        stop_process api
        mv "$state/server.next" "$state/server"
        if [[ $command == smoke ]]; then
            # Smoke does not depend on interactive development overrides.
            unset DEV_CLIENT_ORIGINS
            start_api 1 0
            run python3 scripts/smoke.py "$API_PUBLIC_ORIGIN" setup "$state/smoke-fixture.json"
            run python3 -B scripts/smoke_generation.py "$state" http "${smoke_sql[@]}"
            stop_process api
            start_api 1 0
            run python3 scripts/smoke.py "$API_PUBLIC_ORIGIN" verify "$state/smoke-fixture.json"
            run python3 -B scripts/smoke_generation.py "$state" verify "${smoke_sql[@]}"
            run python3 -B scripts/smoke_generation.py "$state" execute "${smoke_sql[@]}"
            stop_process api
            start_api 1 0
            run python3 -B scripts/smoke_generation.py "$state" execute-verify "${smoke_sql[@]}"
            run python3 -B scripts/smoke_generation.py "$state" serve "${smoke_sql[@]}"
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
