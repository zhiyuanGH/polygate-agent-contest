FROM scratch
COPY --chmod=0755 polygate-imagegc /polygate-imagegc
COPY ca-certificates.crt /etc/ssl/certs/ca-certificates.crt
USER 65532:65532
ENTRYPOINT ["/polygate-imagegc"]
