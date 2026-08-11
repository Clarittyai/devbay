#!/bin/sh
# Measures devbay against the only fair baseline: what `docker compose up`
# does with the same repository on the same machine. A stack that does not
# work under compose cannot be devbay's fault, and counting it as one sends
# the work in the wrong direction.
#
# Prints: <stack> compose=<state> devbay=<state> [detail]
set -u
DEVBAY=${DEVBAY:-$(cd "$(dirname "$0")/.." && pwd)/bin/devbay}
src=$1
name=$(basename "$src")
work=${DEVBAY_CORPUS_WORK:-/tmp/devbay-corpus}/$name
rm -rf "$work"; mkdir -p "$work"; cp -r "$src"/. "$work"/ 2>/dev/null
cd "$work" || exit 1

serves() { # $1 = host header, $2 = port
  for i in 1 2 3 4 5 6 7 8 9 10; do
    c=$(curl -s -o /dev/null -m 8 -w '%{http_code}' ${1:+-H "Host: $1"} "http://127.0.0.1:$2/" 2>/dev/null)
    case "$c" in 2*|3*|401|403) echo "$c"; return 0;; esac
    sleep 3
  done
  echo "$c"; return 1
}

# --- baseline -------------------------------------------------------------
cport=$(grep -Eo "^ *-? *'?[0-9]+:[0-9]+'?" compose.yaml docker-compose.yaml docker-compose.yml 2>/dev/null \
        | grep -Eo "[0-9]+:" | tr -d ':' | sort -n | head -1)
[ -z "$cport" ] && cport=80
# devbay's proxy owns :80, and most of these stacks publish :80 -- so the
# baseline has to run without it or every one of them reports a port clash
# that has nothing to do with the stack. devbay recreates it on the next bay.
docker rm -f devbay-proxy >/dev/null 2>&1
docker compose up -d >/dev/null 2>&1
cstate=$(serves "" "$cport" >/dev/null 2>&1 && echo up || echo down)
cdetail=$(docker compose ps -a --format '{{.Service}}:{{.State}}' 2>/dev/null | tr '\n' ',' | cut -c1-70)
docker compose down -v --remove-orphans >/dev/null 2>&1

# --- devbay ---------------------------------------------------------------
git init -q . 2>/dev/null; git add -A 2>/dev/null
git -c user.email=a@t -c user.name=a commit -qm init 2>/dev/null
d=""
if ! $DEVBAY init >/dev/null 2>&1 || [ ! -f devbay.yaml ]; then
  d="init-failed"
else
  val=$($DEVBAY validate 2>&1)
  if echo "$val" | grep -q "^  error"; then
    d="invalid:$(echo "$val"|grep '^  error'|head -1|cut -c9-70)"
  else
    $DEVBAY approve --yes >/dev/null 2>&1
    git add -A 2>/dev/null; git -c user.email=a@t -c user.name=a commit -qm m 2>/dev/null
    boot=$($DEVBAY new t 2>&1)
    if [ $? -ne 0 ]; then
      d="boot-failed:$(echo "$boot"|grep -i '^error'|head -1|cut -c1-90)"
    else
      host=$($DEVBAY url t 2>/dev/null | grep -o '[a-z0-9.-]*\.localhost' | head -1)
      code=$(serves "$host" 80)
      case "$code" in 2*|3*|401|403) d="up-$code";; *) d="no-serve-$code";; esac
    fi
    $DEVBAY rm t --force >/dev/null 2>&1
  fi
fi
echo "$name compose=$cstate devbay=$d ${cdetail}"
