FROM golang:1.21-alpine3.19

RUN apk update
RUN apk add --no-cache ffmpeg sed gcc musl-dev sudo git tzdata

# _nvr unpriv user creation.
RUN adduser -D -s /sbin/nologin -h /home/_nvr _nvr

ARG osnvr_version
RUN cd /home/_nvr/ && \
	sudo -u _nvr git clone --branch "$osnvr_version" --depth 1 https://codeberg.org/Curid/os-nvr.git
RUN cd /home/_nvr/os-nvr && \
	sudo -u _nvr /usr/local/go/bin/go mod vendor

# Create cache directory.
RUN sudo -u _nvr mkdir "/home/_nvr/os-nvr/.cache"

# Set environment variables.
RUN sudo -u _nvr go env -w "GOPATH=/home/_nvr/os-nvr/.cache/GOPATH"
RUN sudo -u _nvr go env -w "GOCACHE=/home/_nvr/os-nvr/.cache/GOCACHE"

ADD ./utils/docker/init.sh /init.sh

USER root:root
ENTRYPOINT /init.sh

EXPOSE 2020


