FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/independence-score ./cmd/independence-score

FROM alpine:3.20
# Binding UDP/TCP port 53 needs CAP_NET_BIND_SERVICE, so this runs as root,
# same as other DNS containers (e.g. Pi-hole); it owns no other privilege.
COPY --from=build /out/independence-score /usr/local/bin/independence-score
COPY ownership-map.yaml /etc/independence-score/ownership-map.yaml

ENV LISTEN_ADDR=:53 \
    UPSTREAM_DNS=1.1.1.1:53 \
    DB_PATH=/data/independence-score.sqlite \
    OWNERSHIP_MAP_PATH=/etc/independence-score/ownership-map.yaml \
    DASHBOARD_ADDR=:8080

VOLUME ["/data"]
EXPOSE 53/udp 53/tcp 8080/tcp

ENTRYPOINT ["/usr/local/bin/independence-score"]
