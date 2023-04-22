# Build the manager binary
FROM --platform=${TARGETPLATFORM} golang:1.19 as builder

WORKDIR /workspace
# Copy the Go Modules manifests
COPY go.mod go.mod
COPY go.sum go.sum
# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN go env -w GO111MODULE=on
RUN go env -w GOPROXY=https://goproxy.cn,direct
RUN go mod download

# Copy the go source
COPY main.go main.go

# Build
ARG TARGETARCH
ARG TARGETOS
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build -a -o manager main.go

# Use distroless as minimal base image to package the manager binary
FROM --platform=${TARGETPLATFORM} jibutech/ubi8-minimal:latest
WORKDIR /
COPY --from=builder /workspace/manager .

ENTRYPOINT ["/manager"]
