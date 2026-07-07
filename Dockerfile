# syntax = docker/dockerfile:1.4

ARG ETCD_IMAGE=gcr.io/etcd-development/etcd:v3.5.32

FROM --platform=$BUILDPLATFORM golang:1.26 AS builder

ARG TARGETOS
ARG TARGETARCH
ARG TARGETPLATFORM

WORKDIR /

COPY . .

ENV GOPROXY=https://goproxy.cn,direct
ENV GOARCH=${TARGETARCH}

RUN BUILD_PLATFORMS=${TARGETPLATFORM} make build

FROM ${ETCD_IMAGE} as ETCD

FROM debian:12-slim

ARG TARGETPLATFORM

COPY --from=builder /bin/${TARGETPLATFORM}/etcdcluster /usr/bin/
COPY --from=builder /bin/${TARGETPLATFORM}/ecsnode /usr/bin/

COPY --from=ETCD /usr/local/bin/etcd /usr/bin/
COPY --from=ETCD /usr/local/bin/etcdctl /usr/bin/


USER 65534:65534
ENTRYPOINT ["etcdcluster"]
