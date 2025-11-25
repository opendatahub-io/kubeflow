# Build the manager binary
#
# The Docker context is expected to be:
#
# ${PATH_TO_KUBEFLOW/KUBEFLOW repo}/components
#

# Build arguments
ARG SOURCE_CODE=.
ARG GOLANG_VERSION=1.25

# Use ubi9/go-toolset as base image
# https://catalog.redhat.com/software/containers/ubi9/go-toolset/61e5c00b4ec9945c18787690
# image sets GOTOOLCHAIN=local so it won't end up downloading if go.mod requests different version
FROM registry.access.redhat.com/ubi9/go-toolset:${GOLANG_VERSION} as builder
ARG TARGETOS
ARG TARGETARCH

## Build args to be used at this step
ARG SOURCE_CODE

# Set building workdir
WORKDIR /workspace

# Copy the Go Modules manifests
COPY ${SOURCE_CODE}/notebook-controller ./notebook-controller
COPY ${SOURCE_CODE}/odh-notebook-controller ./odh-notebook-controller

# Update building workdir
WORKDIR /workspace/odh-notebook-controller

## Build the kf-notebook-controller
USER root

# the GOARCH has not a default value to allow the binary be built according to the host where the command
# was called. For example, if we call make docker-build in a local env which has the Apple Silicon M1 SO
# the docker BUILDPLATFORM arg will be linux/arm64 when for Apple x86 it will be linux/amd64. Therefore,
# by leaving it empty we can ensure that the container and binary shipped on it will have the same platform.
RUN if [ -z ${CACHITO_ENV_FILE} ]; then go mod download; else source ${CACHITO_ENV_FILE}; fi && \
  CGO_ENABLED=1 GOOS=${TARGETOS:-linux} GOARCH=${TARGETARCH} go build -tags strictfipsruntime -a -o ./bin/manager main.go

# Use ubi9/ubi-minimal as base image
FROM registry.access.redhat.com/ubi9/ubi-minimal:latest

## Install additional packages
RUN microdnf install -y shadow-utils &&\
    microdnf clean all

## Create a non-root user with UID 1001
RUN useradd --uid 1001 --create-home --user-group --system rhods

## Set workdir directory to user home
WORKDIR /home/rhods

## Copy odh-notebook-controller-manager binary from builder stage
COPY --from=builder /workspace/odh-notebook-controller/bin/manager /manager
COPY --from=builder /workspace/odh-notebook-controller/third_party/license.txt third_party/license.txt

## Switch to a non-root user
USER 1001:0

ENTRYPOINT [ "/manager" ]
