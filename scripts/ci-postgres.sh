#!/usr/bin/env bash
# Postgres efímero para jobs de CI en runners propios (sin "services:" de Actions).
#
#   tools/ci-postgres.sh start [db]   levanta Postgres y exporta PG_DSN / TEST_POSTGRES_DSN
#   tools/ci-postgres.sh stop          lo detiene y borra sus datos
#
# Orden de preferencia (usa lo primero que exista):
#   1. CI_POSTGRES_ADMIN_DSN: un Postgres ya provisto en el runner; se crea una BD
#      desechable (nunca se toca otra) y `stop` la borra.
#   2. docker o podman: contenedor postgres:17-alpine en un puerto libre.
#   3. Binarios locales (initdb/pg_ctl, p. ej. /usr/lib/postgresql/*/bin): un
#      cluster temporal en $RUNNER_TEMP, escuchando solo en 127.0.0.1.
# Si no hay ninguno, falla con un mensaje claro (nunca se salta en silencio).
#
# En GitHub Actions escribe las variables en $GITHUB_ENV para los pasos siguientes
# y deja el estado en $RUNNER_TEMP/ci-postgres para que `stop` (if: always()) limpie.
set -euo pipefail

cmd="${1:-}"
db="${2:-ci_test}"
root="${RUNNER_TEMP:-${TMPDIR:-/tmp}}/ci-postgres"
state="$root/state.env"
kv_state() { printf '%s=%q\n' "$1" "$2" >> "$state"; }
export_var() {
  echo "$1=$2"
  [ -n "${GITHUB_ENV:-}" ] && echo "$1=$2" >> "$GITHUB_ENV" || true
}
free_port() { python3 -c 'import socket;s=socket.socket();s.bind(("127.0.0.1",0));print(s.getsockname()[1])'; }
wait_ready() { # $1 = comando que sale 0 cuando Postgres responde
  for _ in $(seq 1 60); do
    if eval "$1" >/dev/null 2>&1; then return 0; fi
    sleep 1
  done
  echo "ci-postgres: Postgres no respondió en 60 s" >&2
  return 1
}
# docker/podman solo cuentan si el demonio responde (el CLI suelto no basta).
container_runtime() {
  for c in docker podman; do
    if command -v "$c" >/dev/null 2>&1 && "$c" info >/dev/null 2>&1; then command -v "$c"; return 0; fi
  done
  return 1
}
find_pg_bin() {
  for d in "$(dirname "$(command -v initdb 2>/dev/null || echo /nonexistent/x)")" /usr/lib/postgresql/*/bin /usr/pgsql-*/bin /opt/homebrew/opt/postgresql*/bin; do
    [ -x "$d/initdb" ] && [ -x "$d/pg_ctl" ] && { echo "$d"; return 0; }
  done
  return 1
}

start() {
  mkdir -p "$root"
  : > "$state"
  if [ -n "${CI_POSTGRES_ADMIN_DSN:-}" ]; then
    name="ci_$(date +%s)_$$"
    psql "$CI_POSTGRES_ADMIN_DSN" -v ON_ERROR_STOP=1 -c "CREATE DATABASE \"$name\"" >/dev/null
    kv_state mode external; kv_state admin "$CI_POSTGRES_ADMIN_DSN"; kv_state name "$name"
    dsn="$(python3 - "$CI_POSTGRES_ADMIN_DSN" "$name" <<'PY'
import sys, urllib.parse as u
p = u.urlsplit(sys.argv[1]); print(u.urlunsplit((p.scheme, p.netloc, "/" + sys.argv[2], p.query, "")))
PY
)"
  elif rt="$(container_runtime)"; then
    port="$(free_port)"
    cid="$("$rt" run -d --rm -e POSTGRES_PASSWORD=postgres -e "POSTGRES_DB=$db" -p "127.0.0.1:$port:5432" postgres:17-alpine)"
    kv_state mode container; kv_state rt "$rt"; kv_state cid "$cid"
    wait_ready "\"$rt\" exec \"$cid\" pg_isready -U postgres"
    dsn="postgres://postgres:postgres@127.0.0.1:$port/$db?sslmode=disable"
  elif bin="$(find_pg_bin)"; then
    port="$(free_port)"
    data="$root/data"; rm -rf "$data"; mkdir -p "$data"
    run=""
    if [ "$(id -u)" = 0 ]; then # initdb no corre como root
      id postgres >/dev/null 2>&1 || { echo "ci-postgres: root sin usuario 'postgres'" >&2; exit 1; }
      chown -R postgres "$root"; run="runuser -u postgres --"
    fi
    $run "$bin/initdb" -D "$data" -U postgres --auth=trust -E UTF8 >/dev/null
    $run "$bin/pg_ctl" -D "$data" -o "-p $port -c listen_addresses=127.0.0.1 -c unix_socket_directories=$root -c fsync=off -c synchronous_commit=off -c full_page_writes=off" -l "$root/pg.log" -w start >/dev/null
    $run "$bin/createdb" -h 127.0.0.1 -p "$port" -U postgres "$db"
    kv_state mode local; kv_state bin "$bin"; kv_state data "$data"; kv_state run "$run"
    dsn="postgres://postgres@127.0.0.1:$port/$db?sslmode=disable"
  else
    cat >&2 <<'MSG'
ci-postgres: este runner no tiene dónde levantar Postgres. Opciones:
  - exportar CI_POSTGRES_ADMIN_DSN apuntando a un Postgres del VPS (se crea una BD desechable),
  - instalar docker/podman, o
  - instalar los binarios de Postgres (initdb, pg_ctl, createdb).
MSG
    exit 1
  fi
  export_var PG_DSN "$dsn"
  export_var MIGRATIONS_DSN "$dsn"
  # Formato clave=valor que usan algunos tests del kernel.
  kv="$(python3 - "$dsn" <<'PY'
import sys, urllib.parse as u
p = u.urlsplit(sys.argv[1])
pw = u.unquote(p.password or "")
# Con contraseña vacía "password= dbname=x" se lee como password="dbname=x": se omite.
pwpart = f" password='{pw}'" if pw else ""
print(f"host={p.hostname} port={p.port or 5432} user={u.unquote(p.username or 'postgres')}{pwpart} dbname={p.path.lstrip('/')} sslmode=disable")
PY
)"
  export_var TEST_POSTGRES_DSN "$kv"
  echo "ci-postgres: listo (${dsn%%\?*})" >&2
}

stop() {
  [ -f "$state" ] || exit 0
  # shellcheck disable=SC1090
  . "$state"
  case "${mode:-}" in
    external) psql "$admin" -c "DROP DATABASE IF EXISTS \"$name\" WITH (FORCE)" >/dev/null 2>&1 || true ;;
    container) "$rt" rm -f "$cid" >/dev/null 2>&1 || true ;;
    local) ${run:-} "$bin/pg_ctl" -D "$data" -m immediate stop >/dev/null 2>&1 || true ;;
  esac
  rm -rf "$root"
}

case "$cmd" in
  start) start ;;
  stop) stop ;;
  *) echo "uso: $0 start [db] | stop" >&2; exit 2 ;;
esac
