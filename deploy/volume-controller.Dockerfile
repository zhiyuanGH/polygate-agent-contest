FROM scratch
COPY polygate-volume-controller /polygate-volume-controller
USER 65532:65532
ENTRYPOINT ["/polygate-volume-controller"]
