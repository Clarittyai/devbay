#!/bin/sh
# Two bays, one cookie jar -- the argument for per-bay hostnames, run for real.
#
# curl keys cookies by host and ignores the port, which is the rule a browser
# follows and the reason two bays on 127.0.0.1:3000 and 127.0.0.1:3001 share one
# session. Nothing here is simulated: these are real requests to two running
# bays, and the jar is a real jar.
#
# Recorded by demo/cookie-jar.tape. Run it directly to check it still tells the
# truth before you record it.
set -eu

DEVBAY=${DEVBAY:-devbay}
say() { printf '\n\033[1m%s\033[0m\n' "$1"; }
run() { printf '\033[2m$ %s\033[0m\n' "$*"; sh -c "$*"; }

cd "$(dirname "$0")/../examples/cookie-isolation"
[ -d .git ] || {
	git init -q .
	git add -A
	git -c user.email=demo@devbay -c user.name=demo commit -qm cookie-isolation
}

say "Two bays of the same app."
run "$DEVBAY new alpha >/dev/null 2>&1 || true"
run "$DEVBAY new beta  >/dev/null 2>&1 || true"

A=$($DEVBAY url alpha | awk '/^url/{print $2}')
B=$($DEVBAY url beta  | awk '/^url/{print $2}')

say "On ports, the way you would run them without devbay:"
JAR=/tmp/one-jar        # a single cookie jar, as one browser is
rm -f "$JAR"
run "curl -s -c $JAR -b $JAR $A/login"
printf '\n'
run "curl -s -c $JAR -b $JAR $B/"
printf '\n\033[31m   beta is reporting alpha'"'"'s session. One jar, two bays.\033[0m\n'

say "On their own hostnames, which is what devbay gives them:"
JAR2=/tmp/same-jar     # the same situation, different addresses
rm -f "$JAR2"
HA=$($DEVBAY url alpha | awk '/^public_url/{print $2}')
HB=$($DEVBAY url beta  | awk '/^public_url/{print $2}')
run "curl -s -c $JAR2 -b $JAR2 --resolve ${HA#http://}:80:127.0.0.1 $HA/login"
printf '\n'
run "curl -s -c $JAR2 -b $JAR2 --resolve ${HB#http://}:80:127.0.0.1 $HB/"
printf '\n\033[32m   beta sees no cookie at all. The browser keeps them apart.\033[0m\n\n'

rm -f "$JAR" "$JAR2"
[ "${DEMO_KEEP:-}" = 1 ] || {
	$DEVBAY rm alpha --force >/dev/null 2>&1 || true
	$DEVBAY rm beta --force >/dev/null 2>&1 || true
}
