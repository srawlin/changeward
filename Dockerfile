# syntax=docker/dockerfile:1
# pg_query_go needs cgo; build against musl and ship on Alpine.
FROM golang:1.27-alpine AS build
RUN apk add --no-cache build-base git
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=1 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/changeward ./cmd/changeward

FROM alpine:3.20
RUN apk add --no-cache ca-certificates git kubectl && adduser -D -u 10001 changeward
COPY --from=build /out/changeward /usr/local/bin/changeward
USER changeward
WORKDIR /work
ENTRYPOINT ["changeward"]
