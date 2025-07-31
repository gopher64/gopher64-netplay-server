FROM golang:1.24 as builder
WORKDIR /workspace

COPY . .
RUN go mod download
RUN CGO_ENABLED=0 go build -a -o gopher64-netplay-server .

FROM registry.access.redhat.com/ubi10/ubi-micro:latest
WORKDIR /

COPY --from=builder /workspace/gopher64-netplay-server .

ENTRYPOINT ["/gopher64-netplay-server"]
