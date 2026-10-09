# Container image of Squared. The base images are pinned by digest, thus
# the same source gives the same image. Build with "make image".

FROM docker.io/library/golang:1.27.2-alpine@sha256:85dc1069ac644ea3c527b177303a406eb3358192816cd7f9e5848eb658851673 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download && go mod verify
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -buildvcs=false \
      -ldflags="-s -w -buildid= -X main.version=${VERSION}" \
      -o /out/squared ./cmd/squared

FROM gcr.io/distroless/static-debian13:nonroot@sha256:e2e927ec666bae08560abb3c55d0659eceabb657f56b6782ab500a9fc7f555e3
COPY --from=build /out/squared /usr/local/bin/squared
COPY catalog /etc/squared/catalog
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/usr/local/bin/squared"]
CMD ["serve"]
