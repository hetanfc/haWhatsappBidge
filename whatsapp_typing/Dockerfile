# Home Assistant add-on image.
# BUILD_FROM is injected by the Supervisor builder; the default keeps a plain
# `docker build .` working for testing (see Dockerfile.standalone for compose).
ARG BUILD_FROM=ghcr.io/home-assistant/amd64-base:latest

FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY *.go ./
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/whatsapp-typing .

FROM ${BUILD_FROM}
COPY --from=builder /out/whatsapp-typing /usr/bin/whatsapp-typing
COPY run.sh /run.sh
RUN chmod a+x /run.sh /usr/bin/whatsapp-typing
CMD ["/run.sh"]
