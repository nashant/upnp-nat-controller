FROM --platform=$BUILDPLATFORM golang:1.27.4@sha256:205c7636d74e0ffbe3d007162df7d74f859f5a8c8322dfb9f907977686c126cf AS build
ARG TARGETOS TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY api/ api/
COPY cmd/ cmd/
COPY internal/ internal/
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w" -o /manager ./cmd

FROM gcr.io/distroless/static:nonroot@sha256:25ac8f0c3b2334a9eb05452266bd4697864a8c8ef0ef71e5e6480d7f92868329
COPY --from=build /manager /manager
USER 65532:65532
ENTRYPOINT ["/manager"]
