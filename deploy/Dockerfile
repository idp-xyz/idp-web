FROM scratch
COPY idp-web /idp-web
# the PDF reader: a process of its own that the service starts for each PDF, with limits it applies to itself
# (ADR-0001 D31); the service looks for it next to its own executable
COPY idp-web-pdf /idp-web-pdf
# no Docker socket, no files to write: the service runs as an unprivileged user
USER 65532:65532
ENTRYPOINT ["/idp-web"]
