# Bakes the config into the image (Fargate has no config mounts).
FROM grafana/tempo:2.9.0
COPY tempo.yaml /etc/tempo.yaml
CMD ["-config.file=/etc/tempo.yaml", "-config.expand-env=true"]
