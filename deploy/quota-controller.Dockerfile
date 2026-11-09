FROM scratch
COPY --chmod=0755 polygate-quota-controller /polygate-quota-controller
COPY ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
USER 65532:65532
ENTRYPOINT ["/polygate-quota-controller"]
