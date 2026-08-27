# ---- build stage ----
FROM golang:1.22-alpine AS build

WORKDIR /src

# Dependencies first so the layer is cached across source edits.
COPY go.mod go.sum ./
RUN go mod download

COPY . .

ARG TARGET=api
RUN CGO_ENABLED=0 GOOS=linux go build \
      -trimpath -ldflags="-s -w" \
      -o /out/app ./cmd/${TARGET}

# ---- runtime stage ----
FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/app /app
# migrations are embedded in the binary; nothing else is needed at runtime.

USER nonroot:nonroot
EXPOSE 8080
ENTRYPOINT ["/app"]
