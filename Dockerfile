FROM scratch

LABEL maintainer="estafette.io" \
      description="The estafette-k8s-node-compactor is a Kubernetes controller that can remove underutilized nodes more aggressively than how the cloud provider's implementation (e.g. GKE) would do."

COPY ca-certificates.crt /etc/ssl/certs/
COPY estafette-k8s-node-compactor /

ENTRYPOINT ["/estafette-k8s-node-compactor"]
