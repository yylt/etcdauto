# syntax = docker/dockerfile:1.4

ARG ETCD_IMAGE=quay.io/coreos/etcd:v3.5.32

FROM --platform=$BUILDPLATFORM docker.io/library/golang:1.26.5 AS builder

ARG TARGETOS
ARG TARGETARCH
ARG TARGETPLATFORM

WORKDIR /app

COPY . .

ENV GOPROXY=https://goproxy.cn,direct
ENV GOARCH=${TARGETARCH}

RUN BUILD_PLATFORMS=${TARGETPLATFORM} make build

FROM ${ETCD_IMAGE} as ETCD

FROM docker.io/library/debian:12.15-slim

ARG TARGETPLATFORM

COPY --from=builder /app/bin/${TARGETPLATFORM}/etcdcluster /usr/bin/
COPY --from=builder /app/bin/${TARGETPLATFORM}/ecsnode /usr/bin/

COPY --from=ETCD /usr/local/bin/etcd /usr/bin/
COPY --from=ETCD /usr/local/bin/etcdctl /usr/bin/


USER 65534:65534
ENTRYPOINT ["etcdcluster"]
