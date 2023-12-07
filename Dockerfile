FROM golang:1.20 as builder

ADD . /go/src/kubefree

WORKDIR /go/src/kubefree/cmd/kubefree-controller

RUN go install . && \
    CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build . 


FROM ubuntu:18.04

COPY --from=builder /go/src/kubefree/cmd/kubefree-controller/kubefree-controller /kubefree

ENTRYPOINT ["./kubefree"]