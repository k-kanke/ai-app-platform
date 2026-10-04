FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/control-plane ./cmd/control-plane

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/control-plane /control-plane
USER 65532:65532
ENTRYPOINT ["/control-plane"]
