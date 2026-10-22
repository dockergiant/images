FROM --platform=$BUILDPLATFORM alpine:3 AS ui
ARG BUILDARCH
ARG TAILWIND_VERSION=4.3.3
ARG TAILWIND_SHA256_ARM64=71ea4be79c9de9827545682df3e040053fb535d37c71ed2cfdedf9385a0868e0
ARG TAILWIND_SHA256_X64=a04d34ceacc8f52cbe8920ad846cdeb61d3d0021dba32db0d1f77c9d9fad7a6c
RUN apk add --no-cache curl libstdc++ libgcc
RUN set -eu; \
    case "$BUILDARCH" in \
      arm64) f=tailwindcss-linux-arm64-musl; sha=$TAILWIND_SHA256_ARM64 ;; \
      amd64) f=tailwindcss-linux-x64-musl; sha=$TAILWIND_SHA256_X64 ;; \
      *) echo "unsupported arch $BUILDARCH" >&2; exit 1 ;; \
    esac; \
    base="https://github.com/tailwindlabs/tailwindcss/releases/download/v$TAILWIND_VERSION"; \
    curl -fsSL --retry 5 --retry-all-errors --retry-delay 3 -o "/tmp/$f" "$base/$f"; \
    echo "$sha  /tmp/$f" | sha256sum -c -; \
    install -m 0755 "/tmp/$f" /usr/local/bin/tailwindcss
WORKDIR /src
COPY ui/build.sh ui/build.sh
COPY ui/src ui/src
COPY ui/vendor ui/vendor
RUN mkdir -p ui/dist && sh ui/build.sh /usr/local/bin/tailwindcss

FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
ARG TARGETARCH
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
COPY --from=ui /src/ui/dist ui/dist
RUN CGO_ENABLED=0 GOOS=linux GOARCH=$TARGETARCH go build -trimpath -ldflags='-s -w' -o /out/inspector ./cmd/inspector

FROM alpine:3
COPY --from=build /out/inspector /inspector
EXPOSE 8080 8126 4318
ENTRYPOINT ["/inspector"]
