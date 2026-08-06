# syntax=docker/dockerfile:1

FROM golang:1.26 AS builder

WORKDIR /workspace
ARG VERSION=dev

COPY go.mod go.sum ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

RUN CGO_ENABLED=0 GOOS=linux go build \
    -buildvcs=false \
    -trimpath \
    -ldflags "-s -w -X main.version=${VERSION}" \
    -o /out/openstack-gateway-controller \
    ./cmd/openstack-gateway-controller

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=builder /out/openstack-gateway-controller /openstack-gateway-controller

USER 65532:65532
ENTRYPOINT ["/openstack-gateway-controller"]
