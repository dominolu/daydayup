FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/gateway ./cmd/gateway

FROM alpine:3.22 AS runtime
RUN apk add --no-cache ca-certificates && mkdir -p /app/data && chown -R 65532:65532 /app
WORKDIR /app
COPY --from=build /out/gateway /app/gateway
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/app/gateway"]
CMD ["-config", "/app/config.json"]
