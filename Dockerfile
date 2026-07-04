FROM golang:1.25-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/b2-share-broker ./cmd/b2-share-broker
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/b2-share-transcoder ./cmd/b2-share-transcoder

FROM alpine:3.22

RUN apk add --no-cache ca-certificates ffmpeg \
	&& addgroup -S app \
	&& adduser -S -G app -u 65532 app
COPY --from=build /out/b2-share-broker /usr/local/bin/b2-share-broker
COPY --from=build /out/b2-share-transcoder /usr/local/bin/b2-share-transcoder
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/b2-share-broker"]
