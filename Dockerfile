FROM --platform=$BUILDPLATFORM golang:1.27.3-bookworm AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=devel
ARG COMMIT=unknown
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH go build -trimpath -ldflags="-s -w -X main.version=${VERSION} -X main.commit=${COMMIT}" -o /gocacheport ./cmd/gocacheport

FROM scratch
COPY --from=build /etc/ssl/certs/ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
COPY --from=build /gocacheport /gocacheport
USER 10001:10001
EXPOSE 8080 9090
ENTRYPOINT ["/gocacheport"]
CMD ["serve", "--dir=/cache"]
