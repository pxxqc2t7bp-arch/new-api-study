#!/usr/bin/env bash
set -u

readonly MYSQL_IMAGE='mysql:5.7.44@sha256:4bc6bc963e6d8443453676cae56536f4b8156d78bae03c0145cbe47c2aad73bb'
readonly MYSQL_PLATFORM='linux/amd64'
readonly POSTGRES_IMAGE='postgres:15.19@sha256:9b1d34adbce1dd07ee6e94b4a2cf698884b89bd44a6c9c12f5da8f3acbfe4957'
readonly POSTGRES_PLATFORM='linux/arm64'
readonly READINESS_DEADLINE_SECONDS=90
readonly READINESS_KILL_AFTER_SECONDS=$((READINESS_DEADLINE_SECONDS - 1))

repo_root="${NEW_API_TEST_ROOT:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)}"
if [[ ! -f "$repo_root/go.mod" ]]; then
  printf 'invalid repository root: %s\n' "$repo_root" >&2
  exit 2
fi

containers=()
networks=()
temp_files=()
temp_dirs=()

cleanup() {
  local status=$?
  if ((${#containers[@]} > 0)); then
    docker rm -f "${containers[@]}" >/dev/null 2>&1 || true
  fi
  if ((${#networks[@]} > 0)); then
    docker network rm "${networks[@]}" >/dev/null 2>&1 || true
  fi
  if ((${#temp_files[@]} > 0)); then
    rm -f "${temp_files[@]}" >/dev/null 2>&1 || true
  fi
  if ((${#temp_dirs[@]} > 0)); then
    rmdir "${temp_dirs[@]}" >/dev/null 2>&1 || true
  fi
  exit "$status"
}
trap cleanup EXIT HUP INT TERM

usage() {
  printf 'usage: %s -- command [args...]\n' "${BASH_SOURCE[0]##*/}" >&2
}

describe() {
  cat <<EOF
repo root: $repo_root
databases: sqlite, mysql, postgres
sqlite: temporary database file under TMPDIR, removed by cleanup
docker isolation: each server uses a unique internal Docker network and container name
ports: containers also join bridge solely for exact ephemeral 127.0.0.1 localhost mappings
readiness: mysqladmin ping and pg_isready must succeed within ${READINESS_DEADLINE_SECONDS}s wall clock
cleanup: trap is installed before any temporary file, network, or container is started
argv: the same command argv is run once per dialect
exit status: all dialects run when possible; any failed dialect produces an aggregate nonzero exit
mysql image: $MYSQL_IMAGE
mysql platform: $MYSQL_PLATFORM
postgres image: $POSTGRES_IMAGE
postgres platform: $POSTGRES_PLATFORM
readiness deadline: ${READINESS_DEADLINE_SECONDS}s
env contract: APP_PLUGIN_TEST_DIALECT, APP_PLUGIN_TEST_DSN, APP_PLUGIN_TEST_IMAGE, APP_PLUGIN_TEST_PLATFORM
dsn exclusions: TEST_MYSQL_DSN and TEST_POSTGRES_DSN are not set by this runner
go flags: GOFLAGS defaults to -p=1 when unset; user supplied GOFLAGS is preserved
runner interface: test_app_plugin_db_matrix.sh -- command [args...]
EOF
}

if [[ "${1:-}" == "--describe" ]]; then
  describe
  exit 0
fi

if [[ "${1:-}" != "--" || $# -lt 2 ]]; then
  usage
  exit 2
fi
shift
command_args=("$@")

unique_id() {
  printf 'newapi-app-plugin-%s-%s' "$1" "$$-$RANDOM"
}

docker_port() {
  local container=$1
  local port=$2
  docker port "$container" "$port/tcp" | sed -n 's/^127\.0\.0\.1://p' | head -n 1
}

run_with_deadline() {
  local deadline=$1
  shift

  local now remaining pid status
  now=$(date +%s)
  remaining=$((deadline - now))
  if ((remaining <= 0)); then
    return 124
  fi

  "$@" &
  pid=$!
  while kill -0 "$pid" 2>/dev/null; do
    now=$(date +%s)
    if ((now >= deadline)); then
      kill "$pid" >/dev/null 2>&1 || true
      wait "$pid" 2>/dev/null || true
      return 124
    fi
    sleep 0.2
  done

  wait "$pid"
  status=$?
  return "$status"
}

wait_for_mysql() {
  local container=$1
  local deadline=$(( $(date +%s) + READINESS_KILL_AFTER_SECONDS ))
  until run_with_deadline "$deadline" docker exec "$container" mysqladmin ping -h127.0.0.1 -uroot -proot --silent >/dev/null 2>&1; do
    if (( $(date +%s) >= deadline )); then
      printf 'mysql did not become ready within %ss\n' "$READINESS_DEADLINE_SECONDS" >&2
      return 1
    fi
    sleep 0.2
  done
}

wait_for_postgres() {
  local container=$1
  local deadline=$(( $(date +%s) + READINESS_KILL_AFTER_SECONDS ))
  until run_with_deadline "$deadline" docker exec "$container" pg_isready -U newapi -d newapi >/dev/null 2>&1; do
    if (( $(date +%s) >= deadline )); then
      printf 'postgres did not become ready within %ss\n' "$READINESS_DEADLINE_SECONDS" >&2
      return 1
    fi
    sleep 0.2
  done
}

run_for_dialect() {
  local dialect=$1
  local dsn=$2
  local image=$3
  local platform=$4

  printf '==> app plugin tests: %s\n' "$dialect" >&2
  (
    cd "$repo_root" || exit 1
    export APP_PLUGIN_TEST_DIALECT="$dialect"
    export APP_PLUGIN_TEST_DSN="$dsn"
    export APP_PLUGIN_TEST_IMAGE="$image"
    export APP_PLUGIN_TEST_PLATFORM="$platform"
    export GOFLAGS="${GOFLAGS:--p=1}"
    unset TEST_MYSQL_DSN
    unset TEST_POSTGRES_DSN
    "${command_args[@]}"
  )
}

final_status=0

sqlite_dir=$(mktemp -d "${TMPDIR:-/tmp}/newapi-app-plugin-sqlite.XXXXXX")
temp_dirs+=("$sqlite_dir")
sqlite_file="$sqlite_dir/app-plugin.sqlite"
temp_files+=("$sqlite_file")
run_for_dialect sqlite "$sqlite_file" "" "" || final_status=1

mysql_network=$(unique_id mysql-net)
mysql_container=$(unique_id mysql)
networks+=("$mysql_network")
containers+=("$mysql_container")
if docker network create --internal "$mysql_network" >/dev/null &&
  docker run -d --name "$mysql_container" --restart on-failure:5 --platform "$MYSQL_PLATFORM" \
    -p 127.0.0.1::3306 \
    -e MYSQL_ROOT_PASSWORD=root \
    -e MYSQL_DATABASE=newapi \
    "$MYSQL_IMAGE" --innodb-use-native-aio=0 >/dev/null &&
  wait_for_mysql "$mysql_container" &&
  docker network connect "$mysql_network" "$mysql_container"; then
  mysql_port=$(docker_port "$mysql_container" 3306)
  if [[ -z "$mysql_port" ]]; then
    printf 'mysql host port was not published on 127.0.0.1\n' >&2
    final_status=1
  else
    run_for_dialect mysql "root:root@tcp(127.0.0.1:${mysql_port})/newapi?charset=utf8mb4&parseTime=true&loc=Local" "$MYSQL_IMAGE" "$MYSQL_PLATFORM" || final_status=1
  fi
else
  final_status=1
fi

postgres_network=$(unique_id postgres-net)
postgres_container=$(unique_id postgres)
networks+=("$postgres_network")
containers+=("$postgres_container")
if docker network create --internal "$postgres_network" >/dev/null &&
  docker run -d --name "$postgres_container" --platform "$POSTGRES_PLATFORM" \
    -p 127.0.0.1::5432 \
    -e POSTGRES_USER=newapi \
    -e POSTGRES_PASSWORD=newapi \
    -e POSTGRES_DB=newapi \
    "$POSTGRES_IMAGE" >/dev/null &&
  wait_for_postgres "$postgres_container" &&
  docker network connect "$postgres_network" "$postgres_container"; then
  postgres_port=$(docker_port "$postgres_container" 5432)
  if [[ -z "$postgres_port" ]]; then
    printf 'postgres host port was not published on 127.0.0.1\n' >&2
    final_status=1
  else
    run_for_dialect postgres "host=127.0.0.1 port=${postgres_port} user=newapi password=newapi dbname=newapi sslmode=disable TimeZone=UTC" "$POSTGRES_IMAGE" "$POSTGRES_PLATFORM" || final_status=1
  fi
else
  final_status=1
fi

exit "$final_status"
