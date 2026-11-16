# Servizio remoto: il gateway lo chiama in HTTPS su :8443.
FROM python:3.13-slim
WORKDIR /app
COPY plugin_codeguard.py .
USER 65532:65532
EXPOSE 8443
ENV SUNDELION_PLUGIN_LISTEN=https://0.0.0.0:8443
CMD ["python3", "/app/plugin_codeguard.py"]
