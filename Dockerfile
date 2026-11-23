# One image for all pk commands (bench, ci, fluxsim, forge-init).
#
# Multi-arch: the web and Go stages run on the build platform; Go
# cross-compiles with CGO off. Only the small runtime stage runs on the
# target platform. Works for linux/amd64 and linux/arm64 (Apple silicon).
#
#   docker build -t platform-kit:dev .
#   docker buildx build --platform linux/amd64,linux/arm64 -t platform-kit:dev .

FROM --platform=$BUILDPLATFORM node:22-alpine AS web
WORKDIR /src/web
COPY web/package.json web/package-lock.json ./
RUN --mount=type=cache,target=/root/.npm npm ci --no-audit --no-fund
COPY api/openapi.yaml /src/api/openapi.yaml
COPY web/ ./
RUN npm run build

FROM --platform=$BUILDPLATFORM golang:1.24-alpine AS go
ARG TARGETOS
ARG TARGETARCH
ARG VERSION=dev
ENV GOTOOLCHAIN=local CGO_ENABLED=0
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=web /src/web/dist ./web/dist
RUN --mount=type=cache,target=/root/.cache/go-build \
    GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" -o /out/pk ./cmd/pk

FROM alpine:3.22
RUN apk add --no-cache git ca-certificates tzdata \
 && adduser -D -u 1000 -h /home/pk pk \
 && mkdir -p /data /creds && chown pk:pk /data /creds \
 && git config --system user.name pk && git config --system user.email pk@pk.local \
 && git config --system --add safe.directory '*'
COPY --from=go /out/pk /usr/local/bin/pk
USER pk
ENV PK_DATA=/data PK_ADDR=0.0.0.0:8080
VOLUME ["/data"]
EXPOSE 8080
ENTRYPOINT ["pk"]
CMD ["bench"]
