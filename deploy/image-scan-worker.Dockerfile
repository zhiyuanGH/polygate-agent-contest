FROM scratch
COPY --chmod=0755 polygate-image-scan-worker /polygate-image-scan-worker
COPY ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
USER 65532:65532
ENTRYPOINT ["/polygate-image-scan-worker"]
