FROM golang:1.23-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /agentline ./cmd/agentline

FROM alpine:3.22

RUN apk add --no-cache ca-certificates
COPY --from=build /agentline /usr/local/bin/agentline
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["agentline"]
CMD ["server", "--listen", "0.0.0.0:8080", "--public-url", "https://agentline.dev", "--data", "/data/agentline.db"]
