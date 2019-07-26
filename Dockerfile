FROM scratch

LABEL maintainer="estafette.io" \
      description="The ${ESTAFETTE_GIT_NAME} is a Kubernetes controller that can remove underutilized nodes more aggressively than how the cloud provider's implementation (e.g. GKE) would do."

COPY ca-certificates.crt /etc/ssl/certs/
COPY ${ESTAFETTE_GIT_NAME} /

ENTRYPOINT ["/${ESTAFETTE_GIT_NAME}"]
