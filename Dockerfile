FROM --platform=linux/arm64 golang:1.23-alpine AS builder

WORKDIR /app
COPY . .
RUN apk add make
RUN make install
RUN make build-linux-arm64

FROM --platform=linux/arm64 alpine:3.19.0
WORKDIR /app
COPY --from=builder /app/bin/linux/server-arm64 ./server
COPY bin/linux/helper-arm64 ./bin/linux/helper-arm64
RUN chmod +x server

ENV ARG "--port 8080"
CMD ["./server ${ARG}"]
ENTRYPOINT ["sh", "-c"]