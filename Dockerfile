FROM golang:1.25-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -trimpath -ldflags="-s -w" -o /out/b2-share-broker ./cmd/b2-share-broker

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/b2-share-broker /usr/local/bin/b2-share-broker
USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/b2-share-broker"]
