FROM scratch
COPY --chmod=0755 polygate-sourcegc /polygate-sourcegc
USER 65532:65532
ENTRYPOINT ["/polygate-sourcegc"]
