# Built for the k3s cluster on the Pi. The Makefile's build-pi target still
# produces a bare binary for the systemd deployment; both are the same code,
# and this file exists alongside it during the migration.

FROM --platform=$BUILDPLATFORM golang:1.26 AS build
WORKDIR /src

# No third-party dependencies beyond modernc.org/sqlite, but copying the
# module files first still lets the download layer cache across source edits.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

# CGO_ENABLED=0 is what makes the FROM scratch stage below possible, and it
# costs nothing here: modernc.org/sqlite is pure Go, which is the whole
# reason it was chosen over mattn/go-sqlite3.
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/weight-tracker .

# scratch rather than distroless or alpine: a static binary with templates and
# static assets embedded needs nothing else. No shell, no package manager, no
# libc, nothing to patch.
#
# Note there is deliberately no tzdata in this image — main.go imports
# time/tzdata so the zone database travels inside the binary.
FROM scratch

# The app writes SQLite files here; the deployment mounts the host's existing
# /var/lib/weight-tracker over it.
WORKDIR /var/lib/weight-tracker

COPY --from=build /out/weight-tracker /weight-tracker

# Non-root by numeric UID — scratch has no /etc/passwd to name a user in. 65532
# is the conventional "nonroot" id, matching distroless.
USER 65532:65532

EXPOSE 8090
ENTRYPOINT ["/weight-tracker"]
CMD ["-addr", ":8090", "-db", "/var/lib/weight-tracker/weight-tracker.db"]
