#!/bin/sh
# Three bays of a four-service stack, created at the same time.
#
# The claim people disbelieve is the wall clock, so the demo is just the thing
# happening: three `devbay new` in parallel, each with its own hostname, and a
# request to each afterwards to show they are all serving.
#
# Recorded by demo/three-bays.tape.
set -eu

DEVBAY=${DEVBAY:-devbay}
say() { printf '\n\033[1m%s\033[0m\n' "$1"; }

cd "$(dirname "$0")/../examples/taskboard"
[ -d .git ] || {
	git init -q .
	git add -A
	git -c user.email=demo@devbay -c user.name=demo commit -qm taskboard
}
[ -f devbay.yaml ] || {
	$DEVBAY init >/dev/null 2>&1
	git add -A
	git -c user.email=demo@devbay -c user.name=demo commit -qm devbay.yaml
}

say "Three branches. All three at once."
printf '\033[2m$ devbay new add-search & devbay new fix-cart & devbay new bump-deps &\033[0m\n\n'
start=$(date +%s)
for b in add-search fix-cart bump-deps; do
	( $DEVBAY new "$b" 2>&1 | grep -E '^created' ) &
done
wait
printf '\n\033[1mall three up in %s seconds\033[0m\n' "$(( $(date +%s) - start ))"

say "Each on its own hostname, each with its own database:"
for b in add-search fix-cart bump-deps; do
	host=$($DEVBAY url "$b" | awk '/^public_url/{print $2}')
	code=$(curl -s -o /dev/null -w '%{http_code}' --resolve "${host#http://}:80:127.0.0.1" "$host/")
	printf '  %-12s %-42s %s\n' "$b" "$host" "$code"
done
printf '\n'

[ "${DEMO_KEEP:-}" = 1 ] || {
	for b in add-search fix-cart bump-deps; do
		$DEVBAY rm "$b" --force >/dev/null 2>&1 || true
	done
}
