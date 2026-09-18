# Shared by dev.sh. All state is private to this checkout and protected by flock.

fail() { echo "error: $*" >&2; exit 1; }

need() { command -v "$1" >/dev/null || fail "Install $1 (see README.md prerequisites)."; }

# Linux start time plus boot ID prevents stale metadata from adopting a reused PID.
process_identity() {
    local stat rest
    [[ $1 =~ ^[0-9]+$ && -r /proc/$1/stat ]] || return 1
    stat=$(cat "/proc/$1/stat" 2>/dev/null) || return 1
    rest=${stat##*) }
    local fields=($rest)
    [[ ${fields[0]} != Z ]] || return 1
    printf '%s %s %s\n' "$1" "${fields[19]}" "$(cat /proc/sys/kernel/random/boot_id)"
}

owned_running() {
    local saved pid current
    [[ -f $1 ]] || return 1
    saved=$(cat "$1")
    read -r pid _ <<< "$saved"
    current=$(process_identity "$pid") || return 1
    [[ $saved == "$current" ]]
}

start_process() {
    local name=$1 pid
    shift
    setsid "$@" </dev/null >>"$state/$name.log" 2>&1 9>&- &
    pid=$!
    # A short-lived command can finish before the parent records its identity.
    # Keep its PID for wait, but never treat that already-exited process as owned.
    process_identity "$pid" >"$state/$name.pid" || printf '%s 0 exited\n' "$pid" >"$state/$name.pid"
}

stop_process() {
    local name=$1 signal=${2:-TERM} pid
    if ! owned_running "$state/$name.pid"; then
        if [[ -f $state/$name.pid ]]; then
            echo "Removing stale $name process metadata."
            rm -f "$state/$name.pid"
        fi
        return 0
    fi
    read -r pid _ <"$state/$name.pid"
    # All managed children have their own session/process group.
    kill -s "$signal" -- "-$pid" || return 1
    for ((attempt=0; attempt<150; attempt++)); do
        if ! owned_running "$state/$name.pid"; then
            wait "$pid" 2>/dev/null || true
            rm -f "$state/$name.pid"
            return 0
        fi
        sleep 0.1
    done
    echo "Timed out stopping $name; metadata and logs retained in $state" >&2
    return 1
}

# Background + wait lets signal traps run promptly while a build/test is active.
run() {
    local pid result=0
    echo "+ $*"
    : >"$state/job.log"
    start_process job "$@"
    read -r pid _ <"$state/job.pid"
    wait "$pid" || result=$?
    rm -f "$state/job.pid"
    cat "$state/job.log"
    cat "$state/job.log" >>"$state/commands.log"
    return "$result"
}

docker() { command timeout 20 docker "$@"; }

load_settings() {
    password=$(cat "$state/password")
    owner=$(cat "$state/owner")
    backend=$(cat "$state/backend")
    export APP_ENV=development
    export CSRF_SIGNING_KEY="$(cat "$state/csrf-key")"
    export CURSOR_SIGNING_KEY="$(cat "$state/cursor-key")"
    # libpq tools must not inherit an unrelated service, host, or credentials.
    local variable
    for variable in ${!PG@}; do unset "$variable"; done
    if [[ $backend == native ]]; then
        pg_bin=$(cat "$state/pg-bin")
        [[ -x $pg_bin/postgres ]] || fail "Saved PostgreSQL binaries unavailable: $pg_bin"
        [[ $("$pg_bin/postgres" --version) == "$(cat "$state/pg-version")" ]] ||
            fail "PostgreSQL version changed; restore the recorded version or explicitly reset development data."
    elif [[ $backend != docker ]]; then
        fail "Unknown saved database backend in $state/backend"
    fi
}

init_settings() {
    [[ -f $state/backend ]] && { load_settings; return; }
    if type -P docker >/dev/null && docker info >/dev/null 2>&1; then
        backend=docker
        echo 'PostgreSQL 18 (Docker)' >"$state/pg-version"
    else
        local native_bin=''
        if command -v pg_config >/dev/null; then native_bin=$(pg_config --bindir); fi
        if [[ -z $native_bin || ! -x $native_bin/initdb || ! -x $native_bin/postgres || ! -x $native_bin/pg_isready || ! -x $native_bin/createdb || ! -x $native_bin/psql ]]; then
            fail "Start a Docker daemon accessible to this user, or install native PostgreSQL server/client binaries with pg_config."
        fi
        [[ $EUID != 0 ]] || fail "Native PostgreSQL must run as a non-root user."
        backend=native
        printf '%s\n' "$native_bin" >"$state/pg-bin"
        "$native_bin/postgres" --version >"$state/pg-version"
    fi
    # The backend marker is written last, so interrupted setup is safe to retry.
    openssl rand -hex 24 >"$state/password"
    openssl rand -hex 32 >"$state/csrf-key"
    openssl rand -hex 32 >"$state/cursor-key"
    openssl rand -hex 12 >"$state/owner"
    echo "$backend" >"$state/backend"
    load_settings
}

docker_owned() {
    [[ $(docker inspect --format '{{index .Config.Labels "stacktrace.owner"}}' "stacktrace-$owner" 2>/dev/null) == "$owner" ]]
}

database_running() {
    if [[ $backend == native ]]; then
        owned_running "$state/postgres.pid"
    else
        docker_owned && [[ $(docker inspect --format '{{.State.Running}}' "stacktrace-$owner") == true ]]
    fi
}

database_url() {
    if [[ $backend == native ]]; then
        socket_dir=$(cat "$state/socket-dir")
        local encoded
        encoded=$(python3 -c 'import sys,urllib.parse; print(urllib.parse.quote(sys.argv[1],safe=""))' "$socket_dir")
        export DATABASE_URL="postgres://stacktrace:$password@localhost/stacktrace?sslmode=disable&host=$encoded"
        export PGHOST="$socket_dir" PGPORT=5432 PGUSER=stacktrace PGPASSWORD="$password" PGDATABASE=stacktrace
    else
        local port
        port=$(docker inspect --format '{{(index (index .NetworkSettings.Ports "5432/tcp") 0).HostPort}}' "stacktrace-$owner")
        [[ $port =~ ^[0-9]+$ ]] || fail "Could not discover Docker database port."
        export DATABASE_URL="postgres://stacktrace:$password@127.0.0.1:$port/stacktrace?sslmode=disable"
    fi
    export TEST_DATABASE_URL="$DATABASE_URL"
}

start_database() {
    echo "Database backend: $backend; $(cat "$state/pg-version")"
    if database_running; then database_url; return; fi
    db_started=1
    if [[ $backend == native ]]; then
        if [[ ! -f $state/data/PG_VERSION ]]; then
            # Only an incomplete, runner-owned initdb directory can exist here.
            rm -rf "$state/data"
            run "$pg_bin/initdb" -D "$state/data" -U stacktrace --pwfile="$state/password" --auth=scram-sha-256 --no-locale --encoding=UTF8
        fi
        if [[ ! -f $state/socket-dir || ! -d $(cat "$state/socket-dir") ]]; then
            local temp_root=${TMPDIR:-/tmp}
            [[ -d /tmp/opencode ]] && temp_root=/tmp/opencode
            mktemp -d "$temp_root/stacktrace-pg.XXXXXXXX" >"$state/socket-dir"
        fi
        socket_dir=$(cat "$state/socket-dir")
        start_process postgres "$pg_bin/postgres" -D "$state/data" -k "$socket_dir" -h '' -p 5432
        database_url
        for ((attempt=0; attempt<150; attempt++)); do
            owned_running "$state/postgres.pid" || fail "PostgreSQL exited; see $state/postgres.log"
            if "$pg_bin/pg_isready" -q -t 1; then break; fi
            sleep 0.2
        done
        "$pg_bin/pg_isready" -q -t 1 || fail "PostgreSQL readiness timed out; see $state/postgres.log"
        if [[ ! -f $state/database-created ]]; then
            # A prior interruption may have occurred after CREATE DATABASE.
            if ! "$pg_bin/psql" -d postgres -Atqc "SELECT 1 FROM pg_database WHERE datname='stacktrace'" | grep -qx 1; then
                run "$pg_bin/createdb" stacktrace
            fi
            touch "$state/database-created"
        fi
    else
        need docker
        docker info >/dev/null 2>&1 || fail "Start Docker to use this state's saved backend."
        if ! docker_owned; then
            if docker inspect "stacktrace-$owner" >/dev/null 2>&1; then fail "Container ownership mismatch."; fi
            if docker volume inspect "stacktrace-$owner" >/dev/null 2>&1; then
                [[ $(docker volume inspect --format '{{index .Labels "stacktrace.owner"}}' "stacktrace-$owner") == "$owner" ]] || fail 'Volume ownership mismatch.'
            else
                docker volume create --label "stacktrace.owner=$owner" "stacktrace-$owner" >/dev/null
            fi
            export POSTGRES_PASSWORD="$password"
            # Initial image download gets a longer bound than lifecycle queries.
            command timeout 180 docker create --name "stacktrace-$owner" --label "stacktrace.owner=$owner" \
                -e POSTGRES_PASSWORD -e POSTGRES_USER=stacktrace -e POSTGRES_DB=stacktrace \
                -p 127.0.0.1::5432 --mount "type=volume,src=stacktrace-$owner,dst=/var/lib/postgresql" postgres:18 >/dev/null
            unset POSTGRES_PASSWORD
        fi
        docker start "stacktrace-$owner" >/dev/null
        database_url
        for ((attempt=0; attempt<150; attempt++)); do
            database_running || fail "PostgreSQL container exited."
            if docker exec "stacktrace-$owner" pg_isready -q -h 127.0.0.1 -U stacktrace -d stacktrace; then break; fi
            sleep 0.2
        done
        docker exec "stacktrace-$owner" pg_isready -q -h 127.0.0.1 -U stacktrace -d stacktrace || fail "PostgreSQL readiness timed out."
        docker exec "stacktrace-$owner" postgres --version
    fi
}

stop_database() {
    if [[ $backend == native ]]; then
        # A live postmaster without matching runner metadata is not proof of a
        # stopped cluster. Refuse to delete its data or socket during recovery.
        if ! owned_running "$state/postgres.pid" && [[ -f $state/data/postmaster.pid ]]; then
            local postgres_pid
            read -r postgres_pid <"$state/data/postmaster.pid"
            if process_identity "$postgres_pid" >/dev/null; then
                echo 'Live PostgreSQL PID without matching ownership metadata; refusing cleanup.' >&2
                return 1
            fi
        fi
        # SIGINT requests PostgreSQL fast shutdown (disconnect clients cleanly).
        stop_process postgres INT || return 1
        if [[ -f $state/socket-dir ]]; then
            socket_dir=$(cat "$state/socket-dir")
            rmdir "$socket_dir" 2>/dev/null || true
            rm -f "$state/socket-dir"
        fi
    elif [[ $backend == docker ]]; then
        if docker_owned; then
            docker logs "stacktrace-$owner" >"$state/postgres.log" 2>&1 || return 1
            docker stop --time 10 "stacktrace-$owner" >/dev/null || return 1
        elif docker inspect "stacktrace-$owner" >/dev/null 2>&1; then
            echo 'Container ownership mismatch; refusing to stop it.' >&2
            return 1
        elif ! docker info >/dev/null 2>&1; then
            echo 'Docker unavailable; resource cleanup could not be confirmed.' >&2
            return 1
        fi
    fi
}

remove_database() {
    stop_database || return 1
    if [[ $backend == docker ]]; then
        if docker_owned; then docker rm "stacktrace-$owner" >/dev/null || return 1; fi
        if docker volume inspect "stacktrace-$owner" >/dev/null 2>&1; then
            [[ $(docker volume inspect --format '{{index .Labels "stacktrace.owner"}}' "stacktrace-$owner") == "$owner" ]] || return 1
            docker volume rm "stacktrace-$owner" >/dev/null || return 1
        fi
    fi
    rm -rf "$state/data"
}

configure_api() {
    local port=$1
    [[ $port =~ ^[0-9]+$ && ${#port} -le 5 ]] && ((10#$port >= 1 && 10#$port <= 65535)) || fail 'DEV_PORT must be between 1 and 65535.'
    # Keep the public hostname same-site with the default localhost frontend.
    export HTTP_ADDR="127.0.0.1:$port" API_PUBLIC_ORIGIN="http://localhost:$port"
    export CLIENT_ORIGINS=${DEV_CLIENT_ORIGINS:-http://localhost:5173}
}

api_listening() {
    local pid
    read -r pid _ <"$state/api.pid"
    # Readiness must belong to our process, even if another service races us for
    # a selected port. Linux exposes the listening socket inode in /proc.
    python3 - "$pid" "${HTTP_ADDR##*:}" <<'PY'
import os, pathlib, sys
try:
    process = pathlib.Path('/proc') / sys.argv[1]
    sockets = {os.readlink(fd) for fd in (process / 'fd').iterdir()}
    port = int(sys.argv[2])
    for line in (process / 'net/tcp').read_text().splitlines()[1:]:
        fields = line.split()
        if int(fields[1].split(':')[1], 16) == port and fields[3] == '0A':
            if f'socket:[{fields[9]}]' in sockets:
                sys.exit(0)
except (FileNotFoundError, ProcessLookupError):
    pass
sys.exit(1)
PY
}

start_api() {
    local dynamic=$1 port=$2 retries=1
    [[ $dynamic == 1 ]] && retries=5
    for ((retry=0; retry<retries; retry++)); do
        if [[ $dynamic == 1 ]]; then
            port=$(python3 -c 'import socket; s=socket.socket(); s.bind(("127.0.0.1",0)); print(s.getsockname()[1])')
        fi
        configure_api "$port"
        api_started=1
        start_process api "$state/server"
        for ((attempt=0; attempt<150; attempt++)); do
            owned_running "$state/api.pid" || break
            if api_listening && curl --noproxy '*' -fsS --max-time 1 "$API_PUBLIC_ORIGIN/readyz" >/dev/null 2>&1; then
                if owned_running "$state/api.pid"; then
                    echo "$API_PUBLIC_ORIGIN" >"$state/api-origin"
                    echo "API ready: $API_PUBLIC_ORIGIN"
                    return
                fi
            fi
            sleep 0.2
        done
        stop_process api || return 1
        # Only a dynamic bind collision justifies another startup attempt.
        [[ $dynamic == 1 ]] && tail -n 5 "$state/api.log" | grep -q 'address already in use' || break
    done
    fail "API failed to become ready; see $state/api.log"
}
