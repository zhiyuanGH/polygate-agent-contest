FROM scratch
COPY polygate-serving-csr-approver /polygate-serving-csr-approver
USER 65532:65532
ENTRYPOINT ["/polygate-serving-csr-approver"]
