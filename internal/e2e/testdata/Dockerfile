# The Envoy image of the end-to-end test, with its configuration. The front
# proxy and the test upstream run from it, with envoy.yaml and upstream.yaml.
# Dependabot updates the pinned digest.
FROM envoyproxy/envoy:v1.40.2@sha256:8de97e654644827d62afdbf7e1d3dc9fee01a8690d410dd1c63a30dc6b6cecbb
COPY envoy.yaml upstream.yaml /etc/envoy/
