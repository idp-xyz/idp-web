FROM scratch
COPY idp-web /idp-web
# no Docker socket, no files to write: the service runs as an unprivileged user
USER 65532:65532
ENTRYPOINT ["/idp-web"]
