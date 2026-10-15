# build stage
FROM golang:1.26-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -o /out/press ./cmd/press

# run stage — scratch: the binary is static and reads only the environment.
FROM scratch
COPY --from=build /out/press /press
COPY --from=build /etc/passwd /etc/passwd
USER 65532:65532
EXPOSE 8080
ENTRYPOINT ["/press"]
