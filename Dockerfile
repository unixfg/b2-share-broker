FROM golang:1.25-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/b2-share-broker ./cmd/b2-share-broker
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/b2-share-transcoder ./cmd/b2-share-transcoder
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/b2-share-processor ./cmd/b2-share-processor

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        gnupg \
        curl \
    && install -d /etc/apt/keyrings \
    && curl -fsSL https://repo.jellyfin.org/jellyfin_team.gpg.key \
        | gpg --dearmor -o /etc/apt/keyrings/jellyfin.gpg \
    && echo "deb [signed-by=/etc/apt/keyrings/jellyfin.gpg] https://repo.jellyfin.org/debian bookworm main" \
        > /etc/apt/sources.list.d/jellyfin.list \
    && apt-get update \
    && apt-get install -y --no-install-recommends jellyfin-ffmpeg7 \
    && rm -rf /var/lib/apt/lists/* \
    && ln -sf /usr/lib/jellyfin-ffmpeg/ffmpeg /usr/bin/ffmpeg \
    && ln -sf /usr/lib/jellyfin-ffmpeg/ffprobe /usr/bin/ffprobe \
    && groupadd -r app -g 65532 \
    && useradd -r -g app -u 65532 -s /usr/sbin/nologin app
COPY --from=build /out/b2-share-broker /usr/local/bin/b2-share-broker
COPY --from=build /out/b2-share-transcoder /usr/local/bin/b2-share-transcoder
COPY --from=build /out/b2-share-processor /usr/local/bin/b2-share-processor
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/b2-share-broker"]
