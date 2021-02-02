FROM golang:1.14 AS builder

WORKDIR /go/src/github.com/octops/agones-discover-openmatch

COPY . .

RUN make build && chmod +x /go/src/github.com/octops/agones-discover-openmatch/bin/agones-openmatch

FROM alpine

WORKDIR /app

COPY --from=builder /go/src/github.com/octops/agones-discover-openmatch/bin/agones-openmatch /app/
COPY --from=builder /go/src/github.com/octops/agones-discover-openmatch/client.key /tls/crt/tls.key
COPY --from=builder /go/src/github.com/octops/agones-discover-openmatch/client.crt /tls/crt/tls.crt
COPY --from=builder /go/src/github.com/octops/agones-discover-openmatch/ca.crt /tls/ca/tls-ca.crt
ENTRYPOINT ["./agones-openmatch"]
