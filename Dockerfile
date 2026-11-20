# One image, three roles: `runner-scaling guard`, `scaler` and `docker-policy`. Static binary on distroless, no shell.
FROM golang:1.27.4-alpine@sha256:70bd7ddea8b6cbe5d6943cde279d6b67feb10cc924fb9100d1d8757c1575b280 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
ARG VERSION=dev
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=${VERSION}" -o /out/runner-scaling ./cmd/runner-scaling
# Socket directories, group-writable for gid 65532, so a fresh named volume mounted there is usable without root.
RUN mkdir -p /out/run/runner-scaling /out/run/docker-policy

FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
COPY --from=build /out/runner-scaling /usr/local/bin/runner-scaling
COPY --from=build --chown=65532:65532 --chmod=0775 /out/run /run
ENTRYPOINT ["/usr/local/bin/runner-scaling"]
