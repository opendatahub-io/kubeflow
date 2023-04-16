# Build the manager binary
#
# The Docker context is expected to be:
#
# ${PATH_TO_KUBEFLOW/KUBEFLOW repo}/components
#
ARG GOLANG_VERSION=1.17
FROM golang:${GOLANG_VERSION} as builder

WORKDIR /workspace

# Copy the Go Modules manifests
COPY notebook-controller /workspace/notebook-controller
COPY odh-notebook-controller /workspace/odh-notebook-controller

# cache deps before building and copying source so that we don't need to re-download as much
# and so that source changes don't invalidate our downloaded layer
RUN cd /workspace/odh-notebook-controller && go mod download

WORKDIR /workspace/odh-notebook-controller

RUN make build

# Use distroless as minimal base image to package the manager binary
# Refer to https://github.com/GoogleContainerTools/distroless for more details
FROM gcr.io/distroless/base:debug
WORKDIR /
COPY --from=builder /workspace/odh-notebook-controller/bin/manager .
COPY --from=builder /workspace/odh-notebook-controller/third_party/license.txt third_party/license.txt
COPY --from=builder /go/pkg/mod/github.com/hashicorp third_party/hashicorp
ENTRYPOINT ["/manager"]
