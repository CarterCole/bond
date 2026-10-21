#!/bin/sh
# Install or upgrade a bond aggregator on any Linux server you can SSH into.
#
#   deploy/push.sh user@server                 # public address = the SSH host
#   deploy/push.sh user@server bond.example.com
#   deploy/push.sh user@server 203.0.113.5 --userspace
#
# It cross-compiles bondd for the server's CPU, copies it over, and installs
# it as a systemd service that starts on boot. Run it again to upgrade: the
# mesh's keys and devices live in /var/lib/bond and are kept.
#
# The server needs UDP and TCP port 7777 open to the Internet (behind a home
# router it asks the router for that with UPnP), and for the default (kernel)
# mode: root via sudo, /dev/net/tun and iptables.
set -eu

if [ $# -lt 1 ]; then
	sed -n '2,14p' "$0" | sed 's/^# \{0,1\}//'
	exit 2
fi
target=$1
public=${2:-${target#*@}}
[ $# -ge 2 ] && shift 2 || shift 1

cd "$(dirname "$0")/.."

arch=$(ssh "$target" uname -m)
case $arch in
	x86_64) goarch=amd64 ;;
	aarch64 | arm64) goarch=arm64 ;;
	armv7l | armv6l) goarch=arm ;;
	*) echo "unsupported server CPU: $arch" >&2; exit 1 ;;
esac

version=$(git describe --tags --always --dirty 2>/dev/null || echo dev)
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
echo "building bondd $version for linux/$goarch"
CGO_ENABLED=0 GOOS=linux GOARCH=$goarch go build -trimpath \
	-ldflags "-s -w -X github.com/CarterCole/bond/core/node.Version=$version" \
	-o "$out/" ./cmd/...

echo "copying to $target"
scp -q "$out/bondd" "$out/bond" "$target:/tmp/"

echo "installing the service (public address: $public)"
# shellcheck disable=SC2029  # the arguments are meant to expand here
ssh -t "$target" "sudo install -m 0755 /tmp/bondd /tmp/bond /usr/local/bin/ && rm -f /tmp/bondd /tmp/bond \
	&& (command -v iptables >/dev/null || sudo sh -c 'apt-get install -y iptables || dnf install -y iptables || true') \
	&& sudo bondd service install server --public '$public' $* \
	&& sleep 2 && echo && sudo bond code"
