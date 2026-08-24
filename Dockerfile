FROM alpine:3.21
RUN apk add --no-cache ca-certificates wget \
    && addgroup -S yyb \
    && adduser -S -G yyb -h /app yyb

WORKDIR /app
COPY yyb-go /app/yyb-go
COPY resource /tmp/resource-src
RUN chmod +x /app/yyb-go

USER yyb
EXPOSE 9001
ENTRYPOINT ["./yyb-go", "-host", "0.0.0.0", "-port", "9001", "-resource-root", "/app/resource", "-db", "/app/data/db/yyb.db"]
