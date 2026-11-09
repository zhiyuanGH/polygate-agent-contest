FROM scratch
COPY --chmod=0755 polygate-source-build-worker /polygate-source-build-worker
USER 65532:65532
ENTRYPOINT ["/polygate-source-build-worker"]
