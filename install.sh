#!/bin/sh
# devbay installer.
#
#   curl -fsSL https://raw.githubusercontent.com/Clarittyai/devbay/main/install.sh | sh
#
# Downloads the release binary for this machine, checks it against the
# published checksums, and puts it somewhere on PATH. No Go toolchain, no
# sudo unless the destination needs it, nothing left behind on failure.
set -eu

REPO="Clarittyai/devbay"
BIN="devbay"

# Where it goes. First writable directory wins, so a machine with ~/.local/bin
# on PATH never needs sudo; DEVBAY_INSTALL_DIR overrides everything.
pick_dir() {
	if [ -n "${DEVBAY_INSTALL_DIR:-}" ]; then
		printf '%s' "$DEVBAY_INSTALL_DIR"
		return
	fi
	for d in "$HOME/.local/bin" "/usr/local/bin"; do
		case ":$PATH:" in *":$d:"*) ;; *) continue ;; esac
		[ -d "$d" ] && [ -w "$d" ] && { printf '%s' "$d"; return; }
	done
	# Nothing on PATH is writable. ~/.local/bin is the least surprising place
	# to create, and the caller is told to add it.
	printf '%s' "$HOME/.local/bin"
}

die() { printf '\ndevbay: %s\n' "$1" >&2; exit 1; }

need() { command -v "$1" >/dev/null 2>&1 || die "$1 is required but not installed"; }
need uname
need tar

fetch() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL "$1"
	elif command -v wget >/dev/null 2>&1; then
		wget -qO- "$1"
	else
		die "neither curl nor wget is installed"
	fi
}
fetch_to() {
	if command -v curl >/dev/null 2>&1; then
		curl -fsSL -o "$2" "$1"
	else
		wget -qO "$2" "$1"
	fi
}

os=$(uname -s | tr '[:upper:]' '[:lower:]')
case "$os" in
	linux | darwin) ;;
	*) die "no prebuilt binary for $os; build from source with: go install github.com/$REPO/cmd/$BIN@latest" ;;
esac

arch=$(uname -m)
case "$arch" in
	x86_64 | amd64) arch=amd64 ;;
	arm64 | aarch64) arch=arm64 ;;
	*) die "no prebuilt binary for $arch; build from source with: go install github.com/$REPO/cmd/$BIN@latest" ;;
esac

version="${DEVBAY_VERSION:-}"
if [ -z "$version" ]; then
	# Errors are swallowed here on purpose: "no release yet" is answered by the
	# message below, not by curl's exit code leaking into the output.
	version=$(fetch "https://api.github.com/repos/$REPO/releases/latest" 2>/dev/null |
		sed -n 's/.*"tag_name": *"\([^"]*\)".*/\1/p' | head -n 1 || true)
fi
[ -n "$version" ] || die "could not work out the latest version. Install from source instead:
    go install github.com/$REPO/cmd/$BIN@latest"

base="https://github.com/$REPO/releases/download/$version"
archive="${BIN}_${version#v}_${os}_${arch}.tar.gz"

tmp=$(mktemp -d)
trap 'rm -rf "$tmp"' EXIT INT TERM

printf 'devbay %s (%s/%s)\n' "$version" "$os" "$arch"
fetch_to "$base/$archive" "$tmp/$archive" || die "could not download $archive from $base"

# Verified, because this script is piped into a shell and the whole point of
# publishing checksums is that somebody checks them.
if fetch_to "$base/checksums.txt" "$tmp/checksums.txt" 2>/dev/null; then
	if command -v sha256sum >/dev/null 2>&1; then
		( cd "$tmp" && grep " $archive\$" checksums.txt | sha256sum -c - >/dev/null ) ||
			die "checksum mismatch for $archive; refusing to install"
	elif command -v shasum >/dev/null 2>&1; then
		( cd "$tmp" && grep " $archive\$" checksums.txt | shasum -a 256 -c - >/dev/null ) ||
			die "checksum mismatch for $archive; refusing to install"
	else
		printf '  no sha256sum or shasum; skipping checksum verification\n'
	fi
else
	printf '  checksums.txt is unavailable; skipping verification\n'
fi

tar -xzf "$tmp/$archive" -C "$tmp" "$BIN"

dir=$(pick_dir)
mkdir -p "$dir" 2>/dev/null || true
if [ -w "$dir" ]; then
	install -m 0755 "$tmp/$BIN" "$dir/$BIN"
elif command -v sudo >/dev/null 2>&1; then
	printf '  %s needs elevated permission\n' "$dir"
	sudo install -m 0755 "$tmp/$BIN" "$dir/$BIN"
else
	die "$dir is not writable and sudo is unavailable. Set DEVBAY_INSTALL_DIR to somewhere you own."
fi

printf '\ninstalled %s/%s\n' "$dir" "$BIN"
case ":$PATH:" in
	*":$dir:"*) ;;
	*) printf '\n%s is not on your PATH. Add it:\n    export PATH="%s:$PATH"\n' "$dir" "$dir" ;;
esac
printf '\nnext: devbay doctor\n'
