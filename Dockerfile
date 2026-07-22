# Builder runs on the NATIVE build arch (via $BUILDPLATFORM) and cross-compiles
# to the target arch below. This avoids emulating the Go toolchain under QEMU,
# which crashes when building amd64 images on an arm64 host.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS builder

# Provided automatically by buildx from --platform (e.g. linux/amd64).
ARG TARGETOS
ARG TARGETARCH

RUN apk add --no-cache git

WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
# Pin templ to the version in go.mod so the generated code matches the runtime.
RUN go install github.com/a-h/templ/cmd/templ@$(go list -m -f '{{.Version}}' github.com/a-h/templ)
COPY . .
RUN templ generate
# CGO_ENABLED=0 works because the app uses pure-Go modernc.org/sqlite; this
# produces a static binary and lets Go cross-compile without a C toolchain.
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -ldflags "-X 'crabspy/web.Commit=$(git rev-parse --short HEAD 2>/dev/null || echo docker)'" -o crabspy ./cmd/main.go

FROM alpine:latest

WORKDIR /app
COPY --from=builder /app/crabspy .
RUN mkdir -p /app/data

EXPOSE 3012

CMD ["./crabspy"]
