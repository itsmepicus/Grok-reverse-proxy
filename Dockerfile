FROM golang:1.23-alpine AS build
WORKDIR /src
COPY go.mod ./
COPY cmd ./cmd
COPY internal ./internal
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/grok-reverse-proxy ./cmd/grok-reverse-proxy

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/grok-reverse-proxy /usr/local/bin/grok-reverse-proxy
EXPOSE 8080
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/grok-reverse-proxy"]
