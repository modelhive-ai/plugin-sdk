# Code guard in Python

Plugin che maschera nei prompt i codici della carta fedeltà e i codici
cliente, scritto in Python con la sola libreria standard. Non usa l'SDK Go:
implementa direttamente il protocollo HTTP dei plugin
(`docs/plugin-http-openapi.yaml`). La guida completa è il capitolo 10 di
`docs/Modelhive-Sviluppo-plugin.pdf`.

| File | Contenuto |
|---|---|
| `plugin_codeguard.py` | Il plugin: `/health`, `/v1/plugin/process`, verifica della firma HMAC. |
| `manifest-remote.yaml` | Manifest per il servizio remoto in HTTPS. |
| `manifest-local.yaml` | Manifest per il processo avviato dal gateway (host con Python 3). |
| `Dockerfile` | Immagine del servizio remoto, porta 8443. |

## Servizio remoto

```sh
docker build -t codeguard:0.1.0 .
docker run -d -p 8443:8443 \
  -e SUNDELION_PLUGIN_HMAC_SECRET=<segreto> \
  -e SUNDELION_PLUGIN_TLS_CERT=/tls/tls.crt -e SUNDELION_PLUGIN_TLS_KEY=/tls/tls.key \
  -v /etc/codeguard/tls:/tls:ro codeguard:0.1.0
```

Nel gateway: cartella `com.example.codeguard-remote/` con
`manifest-remote.yaml` rinominato `manifest.yaml` nella cartella dei plugin,
**Reload plugins**, nuova istanza con l'URL del servizio e lo stesso segreto.

## Processo locale

Cartella `com.example.codeguard-py/` con `manifest-local.yaml` rinominato
`manifest.yaml` e lo script in `bin/plugin_codeguard.py` (eseguibile). Il
gateway avvia lo script e gli passa socket, segreto e configurazione.
