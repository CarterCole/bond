# The bond aggregator as a container.
#
#   docker build -f deploy/docker/Dockerfile -t bond .
#   docker run -d --name bond -p 7777:7777/udp -p 7777:7777/tcp \
#       -v bond-state:/data -e BOND_PUBLIC=<address devices can reach> bond
#   docker exec bond bond code          # the join code
#
# It runs in userspace mode: no privileges, no TUN device, no iptables. The
# container reaches the Internet with ordinary sockets on behalf of the mesh.
# For kernel-mode forwarding (faster, real ICMP) run bondd on the host instead.

FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/CarterCole/bond/core/node.Version=${VERSION}" \
      -o /out/ ./cmd/...

FROM alpine:3.22
RUN adduser -D -H -u 10001 bond && mkdir /data && chown bond /data
COPY --from=build /out/bondd /out/bond /usr/local/bin/
USER bond
VOLUME /data
EXPOSE 7777/udp 7777/tcp
ENV BOND_PUBLIC=""
# The status API stays on loopback inside the container; `docker exec bond bond status` reaches it.
ENTRYPOINT ["/bin/sh", "-c", "exec bondd server --userspace --state /data ${BOND_PUBLIC:+--public \"$BOND_PUBLIC\"} \"$@\"", "--"]
