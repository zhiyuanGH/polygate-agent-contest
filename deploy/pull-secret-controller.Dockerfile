FROM scratch
COPY polygate-pull-secret-controller /polygate-pull-secret-controller
USER 65532:65532
ENTRYPOINT ["/polygate-pull-secret-controller"]
