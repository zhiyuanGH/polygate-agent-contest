FROM scratch
COPY --chmod=0755 polygate-api /polygate-api
COPY ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
USER 65532:65532
ENTRYPOINT ["/polygate-api"]
