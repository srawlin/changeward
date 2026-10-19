# syntax=docker/dockerfile:1
# pg_query_go needs cgo; build against musl and ship on Alpine.
FROM golang:1.27-alpine AS build
RUN apk add --no-cache build-base git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=1 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/changegate ./cmd/changegate

FROM alpine:3.20
RUN apk add --no-cache ca-certificates git && adduser -D -u 10001 changegate
COPY --from=build /out/changegate /usr/local/bin/changegate
USER changegate
WORKDIR /work
ENTRYPOINT ["changegate"]
