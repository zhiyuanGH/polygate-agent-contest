FROM scratch
COPY polygate-worker /polygate-worker
USER 65532:65532
ENTRYPOINT ["/polygate-worker"]
