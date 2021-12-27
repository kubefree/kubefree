FROM ubuntu:18.04

ADD kubefree /kubefree
ENTRYPOINT ["./kubefree"]

