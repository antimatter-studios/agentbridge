FROM golang:1.25-alpine AS builder
WORKDIR /src
COPY . .
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /agent-bridge .

FROM scratch
COPY --from=builder /agent-bridge /agent-bridge
ENTRYPOINT ["/agent-bridge"]
