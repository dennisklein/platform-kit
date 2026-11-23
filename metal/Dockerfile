# Operator image: static build into scratch (docs-assistant pattern). The CLI is a host
# tool and is not shipped in the image. No healthcheck yet — the manager grows its health
# endpoints with the controller-runtime manager; add the self-healthcheck flag then.
FROM golang:1.26-alpine AS build
ARG TAG=dev
WORKDIR /src
COPY go.mod ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${TAG}" -o /manager ./cmd/manager

FROM scratch
COPY --from=build /manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
