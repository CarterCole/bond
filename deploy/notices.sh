#!/bin/sh
# Print the licenses of the third-party modules compiled into the given
# binaries. Their licenses ask for these notices to travel with the binaries,
# so `make release` puts them in every archive as NOTICE.
#
#   deploy/notices.sh <binary>...
set -eu

if [ $# -eq 0 ]; then
	sed -n '2,6p' "$0" | sed 's/^# \{0,1\}//'
	exit 2
fi

cat <<'EOF'
Bond is licensed under the GNU Affero General Public License, version 3 only
(see LICENSE). It is built with the third-party modules below, each under its
own license.
EOF

banner() {
	printf '\n%s\n%s\n%s\n\n' "================================================================" "$1" "================================================================"
}

# The Go standard library and runtime are compiled in too.
# (Homebrew keeps Go's LICENSE beside GOROOT rather than in it.)
goroot=$(go env GOROOT)
for f in "$goroot/LICENSE" "$goroot/../LICENSE" ""; do
	[ -z "$f" ] && { echo "cannot find Go's LICENSE near $goroot" >&2; exit 1; }
	[ -f "$f" ] && break
done
banner "The Go standard library ($(go env GOVERSION)) — LICENSE"
cat "$f"

go version -m "$@" | awk '$1 == "dep" { print $2, $3 }' | sort -u |
	while read -r mod ver; do
		dir=$(go mod download -json "$mod@$ver" | sed -n 's/^[[:space:]]*"Dir": "\(.*\)",*$/\1/p')
		found=
		for f in $(ls "$dir" | grep -i -E '^(licen[cs]e|copying|notice)(\.(txt|md))?$'); do
			banner "$mod $ver — $f"
			cat "$dir/$f"
			found=1
		done
		[ -n "$found" ] || { echo "no license file in $mod $ver" >&2; exit 1; }
	done
