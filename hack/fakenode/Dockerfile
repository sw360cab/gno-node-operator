# Build from the repository root:
#   docker build -f hack/fakenode/Dockerfile -t fake-gno-node:v0.1.0 .
FROM golang:1.26 AS builder
WORKDIR /workspace
COPY go.mod go.sum ./
RUN go mod download
COPY hack/fakenode/ hack/fakenode/
RUN CGO_ENABLED=0 go build -a -o fakenode ./hack/fakenode

FROM gcr.io/distroless/static:nonroot
WORKDIR /
COPY --from=builder /workspace/fakenode .
USER 65532:65532
EXPOSE 26657
ENTRYPOINT ["/fakenode"]
