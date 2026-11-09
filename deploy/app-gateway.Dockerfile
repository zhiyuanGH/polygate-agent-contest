FROM scratch
COPY polygate-app-gateway /polygate-app-gateway
USER 65532:65532
ENTRYPOINT ["/polygate-app-gateway"]
