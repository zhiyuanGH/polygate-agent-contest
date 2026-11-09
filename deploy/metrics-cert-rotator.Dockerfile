FROM scratch
COPY polygate-metrics-cert-rotator /polygate-metrics-cert-rotator
USER 65532:65532
ENTRYPOINT ["/polygate-metrics-cert-rotator"]
