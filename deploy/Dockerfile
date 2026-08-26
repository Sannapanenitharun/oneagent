# Build
FROM golang:1.23-alpine AS build
WORKDIR /src
COPY . .
# CGO off and a static link, matching how the agent ships: the result runs on
# any base image, including scratch below.
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath \
    -ldflags "-s -w -X github.com/agent-i/agent/internal/version.Version=$(cat VERSION 2>/dev/null || echo dev)" \
    -o /out/agenti-server ./cmd/server

# Run
FROM alpine:3.20
# The server talks to ClickHouse over HTTPS in any real deployment, and a
# scratch image has no root certificates to verify it with.
RUN apk add --no-cache ca-certificates wget && adduser -D -u 10001 agenti
COPY --from=build /out/agenti-server /usr/local/bin/agenti-server
USER agenti
EXPOSE 4318
ENTRYPOINT ["/usr/local/bin/agenti-server"]
