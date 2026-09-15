# dellfanctl container image.
#
# Built for running dellfanctl on appliance-style Linux distros (TrueNAS
# SCALE and similar) where installing a binary + systemd unit directly onto
# the host doesn't survive an OS update. See deploy/truenas/README.md.
#
# linux/amd64 only - Dell PowerEdge/iDRAC hardware this talks to is x86_64.

FROM golang:1.24-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build \
    -trimpath \
    -ldflags "-s -w -X dellfanctl/internal/version.Version=${VERSION}" \
    -o /out/dellfanctl \
    ./cmd/dellfanctl

FROM debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
      ipmitool \
      smartmontools \
      ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/dellfanctl /usr/local/bin/dellfanctl

# Config lives on a mounted volume (a persistent dataset on TrueNAS) rather
# than being baked into the image; see deploy/truenas/README.md for the
# `discover` step that populates it.
VOLUME ["/etc/dellfanctl"]

ENTRYPOINT ["/usr/local/bin/dellfanctl"]
CMD ["run", "--config", "/etc/dellfanctl/config.yaml"]
