# GoReleaser builds this image from the binaries it has already compiled: the
# build context holds them as <os>/<arch>/tessera, and there is nothing to
# compile or run here. Dependabot updates the pinned base image digest.
FROM gcr.io/distroless/static-debian13:nonroot@sha256:aafe993665bf6200db38a02c11a9351e782a84b6a45e7f33b6695638c88e3931
ARG TARGETPLATFORM
COPY $TARGETPLATFORM/tessera /usr/bin/tessera
ENV TESSERA_CONFIG=/etc/tessera/config.yaml
EXPOSE 9001 9090
USER 65532:65532
ENTRYPOINT ["/usr/bin/tessera"]
CMD ["serve"]
