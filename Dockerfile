FROM golang:1.13.5-alpine3.10 as builder
RUN apk add git --no-cache && \
    apk add pkgconfig --no-cache && \
    apk add g++ --no-cache
ADD . /src
WORKDIR /src/daemon
RUN GOOS=linux go build -a -o app .


FROM golang:1.13.5-alpine3.10
WORKDIR /app
COPY --from=builder /src/daemon/app .
EXPOSE 9091 4006 5050 9090
VOLUME /repo
ENTRYPOINT ["/app/app", "-repo", "/repo"]
