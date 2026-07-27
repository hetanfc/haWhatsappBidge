# Home Assistant add-on image.
#
# BUILD_FROM is injected by the Supervisor from build.yaml, which picks the base
# image matching the device architecture. No default on purpose: a hardcoded one
# would silently build for the wrong arch. To build this by hand, pass it:
#   docker build --build-arg BUILD_FROM=ghcr.io/home-assistant/amd64-base:latest .
# For plain docker/compose use Dockerfile.standalone instead.
ARG BUILD_FROM

FROM golang:1.25-alpine AS builder
ARG BUILD_VERSION=dev
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w -X main.version=${BUILD_VERSION}" -o /out/whatsapp-typing .

FROM ${BUILD_FROM}
COPY --from=builder /out/whatsapp-typing /usr/bin/whatsapp-typing
COPY run.sh /run.sh
RUN chmod a+x /run.sh /usr/bin/whatsapp-typing
CMD ["/run.sh"]
