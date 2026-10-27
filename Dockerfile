# syntax=docker/dockerfile:1

# Build stage. CGO is off so the binary is static and can run in a distroless
# image; -trimpath keeps paths out of the binary and -s -w drops debug info, which
# matters because the kube-scheduler dependency tree makes the binary large (F15).
FROM golang:1.27 AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG VERSION=dev
RUN CGO_ENABLED=0 GOOS=linux go build \
        -trimpath \
        -ldflags "-s -w -X github.com/SakkijarvenPolka/tahrs-scheduler/internal/version.Build=${VERSION}" \
        -o /out/tahrs-scheduler ./cmd/tahrs-scheduler

# Runtime stage: distroless static has CA certificates and tzdata but no shell,
# and runs as uid 65532 by default (matches the Deployment's securityContext).
FROM gcr.io/distroless/static:nonroot

COPY --from=build /out/tahrs-scheduler /tahrs-scheduler

USER 65532:65532
ENTRYPOINT ["/tahrs-scheduler"]
