# syntax=docker/dockerfile:1

# Multi-stage: build the static gateway binary and its OAuth CLI, ship both in a
# minimal runtime image. The final image is non-root, listens on 0.0.0.0:8080,
# persists usage data in /data, and takes all credentials through env
# (ONEGW_KEYS, ONEGW_PROVIDER_*_KEY, ONEGW_ADMIN_PASSWORD) — no secrets baked
# into the image.

# golang:1.25-alpine has no .git → buildvcs skips stamping, same as the release
# workflow's binary; VERSION is injected by the release workflow so the
# binary (and `onegw update`) reports its real release. Local builds: dev.
ARG VERSION=dev
FROM golang:1.25-alpine AS build
ARG VERSION
# Bounded build parallelism (repo rule: cap Go build jobs; default 2). Override
# with --build-arg GO_BUILD_JOBS=N on a beefier builder.
ARG GO_BUILD_JOBS=2
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 go build -p "${GO_BUILD_JOBS}" -trimpath \
    -ldflags "-s -w -X onegw/internal/update.version=${VERSION}" \
    -o /out/onegw ./cmd/onegw \
 && CGO_ENABLED=0 go build -p "${GO_BUILD_JOBS}" -trimpath \
    -o /out/onegw-oauth ./cmd/onegw-oauth

# --- Runtime stage -----------------------------------------------------------
FROM alpine:3.20
# ca-certificates: the gateway dials https upstreams; tzdata: SQLite/rollup
# timestamps; curl: advertised healthcheck probe.
RUN apk add --no-cache ca-certificates tzdata curl \
    && addgroup -S onegw && adduser -S -G onegw -h /data -s /sbin/nologin onegw \
    && mkdir -p /data && chown onegw:onegw /data
COPY --from=build /out/onegw /usr/local/bin/onegw
# The standalone OAuth CLI used to be missing from the image, so the documented
# `docker exec -it onegw onegw-oauth login …` failed with "executable file not
# found in $PATH". Both entry points ship now; they run the same code
# (internal/oauthcmd), so either form works inside the container.
COPY --from=build /out/onegw-oauth /usr/local/bin/onegw-oauth
COPY docker/onegw.default.toml /etc/onegw/onegw.toml
# The dashboard's provider/combos editor saves straight into this file, and the
# write is atomic (temp file in the same directory + rename). The directory
# therefore has to be writable by the non-root user, or every in-page save
# answers 500 "temp file: open /etc/onegw/.onegw-config-*.toml: permission
# denied" — the bare `docker run` path did exactly that until this line. A named
# volume mounted here inherits this ownership, so edits also survive a recreate.
RUN chown onegw:onegw /etc/onegw /etc/onegw/onegw.toml

# Runtime config; /data holds usage.db (bind-mount or named volume it).
ENV ONEGW_CONFIG=/etc/onegw/onegw.toml \
    ONEGW_DATA_DIR=/data
WORKDIR /data
USER onegw
EXPOSE 8080

# No VOLUME ["/data"] on purpose: a VOLUME declaration makes every plain
# `docker run` (without -v) allocate an ANONYMOUS volume, and the documented
# update path — pull a new image then recreate the container — would start
# the new container on a FRESH anonymous volume while the old data survives
# only as an orphaned volume: usage.db silently re-homes. Persistence is
# explicit instead: docker-compose.yml mounts the onegw-data named volume,
# and plain `docker run` users pass -v <name>:/data themselves (see README).

HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
    CMD curl -fsS http://127.0.0.1:8080/ >/dev/null || exit 1
# Probes the unauthenticated dashboard root — /admin/* is password-gated
# (X-Admin-Password), so a header probe would break with a mounted config
# that sets a different password.

# In-container `onegw update` is check-only by design (internal/update/docker.go):
# the filesystem belongs to the image, so it reports the newer release and prints
# host-side guidance — docker pull ghcr.io/freepeak/onegw:<tag> + recreate. The
# release workflow stamps VERSION so the check compares against the real tag;
# local builds report "dev".

ENTRYPOINT ["/usr/local/bin/onegw"]
