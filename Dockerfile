# Build stage
FROM golang:1.25-alpine AS builder

RUN apk add --no-cache git

ARG VERSION=dev
WORKDIR /src
COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w -X github.com/verge-io/csi-vergeos/pkg/driver.DriverVersion=${VERSION}" -o /csi-vergeos ./cmd/csi-vergeos/

# Runtime stage
FROM alpine:3.21

# NFS and mount utilities needed by the node plugin.
# xfsprogs: mkfs.xfs; xfsprogs-extra: xfs_growfs (fsType: xfs).
RUN apk add --no-cache \
    nfs-utils \
    e2fsprogs \
    xfsprogs \
    xfsprogs-extra \
    blkid \
    util-linux

COPY --from=builder /csi-vergeos /usr/local/bin/csi-vergeos

ENTRYPOINT ["/usr/local/bin/csi-vergeos"]
