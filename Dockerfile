# pi-token-usage: static Go binary on scratch.
# No `# syntax=` directive: that forces a network fetch of the BuildKit frontend
# image, which breaks offline builds. The built-in frontend is sufficient here.

# ---------- build ----------
FROM golang:1.26 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

# modernc.org/sqlite is pure Go, so CGO stays off and the binary is fully static.
# That is what makes the scratch final stage possible.
ENV CGO_ENABLED=0 GOOS=linux
RUN go build -trimpath -ldflags="-s -w" -o /out/pitokens ./cmd/pitokens

# Pre-create the DB directory with the runtime uid so the named volume inherits it.
# scratch has no RUN, so this has to happen here.
#
# The DB lives in a SUBDIRECTORY of the volume, not the volume root: Docker
# creates the volume's top-level directory as root:root and copies only the
# contents, so a uid-65534 process mounted at /data cannot create anything.
# A subdirectory copied in with its ownership preserved is writable.
FROM build AS staged
RUN mkdir -p /data/db && chown -R 65534:65534 /data /data/db

# ---------- runtime ----------
FROM scratch

COPY --from=build /out/pitokens /pitokens
COPY --from=staged /data /data

USER 65534:65534
EXPOSE 7777

ENV PITOKENS_DB=/data/db/index.db \
    PITOKENS_ADDR=0.0.0.0:7777 \
    TZ=UTC

# No curl in scratch, so the binary checks itself.
HEALTHCHECK --interval=30s --timeout=5s --start-period=25s --retries=3 \
  CMD ["/pitokens", "healthcheck"]

ENTRYPOINT ["/pitokens", "serve"]
