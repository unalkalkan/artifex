# Artifex: the server, and the browser that draws screenshots. git is
# installed because the server runs it for every project repository (design
# decision 8).
FROM golang:1.26-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=1 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/artifex ./cmd/artifex

# The renderer (design decision 24): the same binary with Chromium, run as a
# container of its own with no volumes and no secrets, so a page that broke
# out of the browser would find nothing. Build it with --target renderer.
FROM debian:bookworm-slim AS renderer
RUN apt-get update \
 && apt-get install -y --no-install-recommends chromium fonts-liberation fonts-noto-color-emoji ca-certificates \
 && rm -rf /var/lib/apt/lists/* \
 && groupadd -g 10002 renderer && useradd -r -u 10002 -g renderer -d /tmp -M renderer
COPY --from=build /out/artifex /usr/local/bin/artifex
USER 10002:10002
EXPOSE 8091
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s CMD ["artifex", "healthcheck", "-url", "http://127.0.0.1:8091/healthz"]
ENTRYPOINT ["artifex"]
# Chromium's own sandbox needs user namespaces, which containers lack; the
# container is the sandbox.
CMD ["renderer", "-no-sandbox"]

# The server. Last, so a plain `docker build` builds it.
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
