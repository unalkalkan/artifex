# Artifex: one container. git is installed because the server runs it for
# every project repository (design decision 8).
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=1 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/artifex ./cmd/artifex

FROM debian:bookworm-slim
RUN apt-get update \
 && apt-get install -y --no-install-recommends git ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd -g 10001 artifex && useradd -r -u 10001 -g artifex -d /data -M artifex \
 && install -d -o 10001 -g 10001 -m 0750 /data
COPY --from=build /out/artifex /usr/local/bin/artifex
USER 10001:10001
ENV ARTIFEX_CONFIG=/config/config.yaml
VOLUME /data
EXPOSE 8090
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["artifex", "healthcheck"]
ENTRYPOINT ["artifex"]
CMD ["serve"]
