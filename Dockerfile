FROM golang:1.24-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/checkout ./cmd && \
    CGO_ENABLED=0 go build -o /out/gatesync ./cmd/gatesync

FROM alpine:3.20
RUN apk add --no-cache ca-certificates tzdata
COPY --from=build /out/checkout /out/gatesync /usr/local/bin/
EXPOSE 8080 9090
ENTRYPOINT ["checkout"]
