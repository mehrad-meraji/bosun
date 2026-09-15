FROM --platform=$BUILDPLATFORM golang:1.27 AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /out/bosun . \
 && mkdir -m 0700 -p /out/run/bosun \
 && mkdir -m 0700 -p /out/var/lib/bosun \
 && mkdir -m 0700 -p /out/var/lib/bosun-backups

# Distroless: no shell, no package manager. nonroot is UID 65532.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/bosun /usr/local/bin/bosun
# The shared run folder. A new named volume copies this owner and mode.
COPY --from=build --chown=65532:65532 /out/run/bosun /run/bosun
# Gate-only state; never given to the updater.
COPY --from=build --chown=65532:65532 /out/var/lib/bosun /var/lib/bosun
# Gate-only backups; never given to the updater.
COPY --from=build --chown=65532:65532 /out/var/lib/bosun-backups /var/lib/bosun-backups
ENTRYPOINT ["/usr/local/bin/bosun"]
CMD ["gate"]
