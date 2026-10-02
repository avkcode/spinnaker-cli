# Build
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
ARG COMMIT=none
RUN CGO_ENABLED=0 go build -trimpath \
    -ldflags="-s -w \
      -X github.com/avkcode/spinnaker-cli/cmd.Version=${VERSION} \
      -X github.com/avkcode/spinnaker-cli/cmd.Commit=${COMMIT}" \
    -o /sc .

# Run
#
# distroless static: the binary is CGO-free and needs only CA certificates, which
# the base image provides. Running as nonroot means the in-cluster service account
# path still works when this image is used as a Job inside the cluster.
FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /sc /usr/local/bin/sc
USER nonroot:nonroot
ENTRYPOINT ["/usr/local/bin/sc"]
