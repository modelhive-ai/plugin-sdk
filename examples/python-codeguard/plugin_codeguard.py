#!/usr/bin/env python3
"""Plugin Modelhive in Python: maschera i codici cliente aziendali.

Solo libreria standard. Parla il protocollo HTTP dei plugin
(docs/plugin-http-openapi.yaml): GET /health senza firma, POST
/v1/plugin/process firmato con HMAC-SHA256.

Due modalità, scelte da SUNDELION_PLUGIN_LISTEN:

- unix:///percorso.sock  processo locale avviato dal gateway, che imposta
  tutte le variabili SUNDELION_PLUGIN_*;
- https://0.0.0.0:8443   servizio remoto: le variabili le impostate voi, più
  SUNDELION_PLUGIN_TLS_CERT e SUNDELION_PLUGIN_TLS_KEY (certificato del
  servizio) e, se volete, CODEGUARD_PATTERNS (JSON) al posto della
  configurazione dell'istanza.
"""
import hashlib
import hmac
import json
import os
import re
import signal
import socket
import socketserver
import ssl
import sys
import time
from http.server import BaseHTTPRequestHandler, HTTPServer, ThreadingHTTPServer
from urllib.parse import urlparse

SECRET = os.environ["SUNDELION_PLUGIN_HMAC_SECRET"].encode()
LISTEN = os.environ["SUNDELION_PLUGIN_LISTEN"]
# In modalità locale il gateway passa l'istanza; le chiavi sono ID, Name, Config...
INSTANCE = json.loads(os.environ.get("SUNDELION_PLUGIN_INSTANCE") or "{}")
DEFAULT_PATTERNS = [
    {"name": "carta_fedelta", "regex": r"\bFID-?[0-9]{10}\b"},
    {"name": "codice_cliente", "regex": r"\bCLI[0-9]{8}\b"},
]
PATTERNS = (
    json.loads(os.environ["CODEGUARD_PATTERNS"]) if os.environ.get("CODEGUARD_PATTERNS")
    else (INSTANCE.get("Config") or {}).get("patterns") or DEFAULT_PATTERNS
)
COMPILED = [(p["name"], re.compile(p["regex"])) for p in PATTERNS]
MAX_BODY = 1 << 20  # il gateway non invia corpi più grandi di 1 MiB
SKEW = 300  # scarto massimo dell'orologio, in secondi


def process(req):
    """Restituisce le Ops (oggetto JSON) per una richiesta."""
    spans = {}
    for message in req.get("messages") or []:
        for text in (message.get("text") or "", message.get("tool_text") or ""):
            for name, regex in COMPILED:
                for match in regex.findall(text):
                    spans[match] = "[" + name + "]"
    if not spans:
        return {}
    return {"redact_spans": spans, "annotations": {"codeguard.masked": str(len(spans))}}


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"  # keep-alive: il gateway riusa la connessione

    def log_message(self, fmt, *args):
        sys.stderr.write("plugin-codeguard: " + fmt % args + "\n")

    def reply(self, status, payload):
        body = json.dumps(payload).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    def error(self, status, message):
        self.reply(status, {"error": {"message": message}})

    def do_GET(self):
        if self.path == "/health":  # l'unico endpoint senza firma
            self.reply(200, {"status": "ok", "plugin": "codeguard"})
        else:
            self.error(405, "method not allowed")

    def do_POST(self):
        length = int(self.headers.get("Content-Length") or 0)
        if length > MAX_BODY:
            return self.error(413, "request body too large")
        body = self.rfile.read(length)

        # Firma: HMAC-SHA256(segreto, timestamp \n metodo \n percorso \n corpo)
        timestamp = self.headers.get("X-Sundelion-Timestamp", "")
        try:
            skew = abs(time.time() - int(timestamp))
        except ValueError:
            return self.error(401, "invalid signature timestamp")
        if skew > SKEW:
            return self.error(401, "signature timestamp outside allowed skew")
        signed = (timestamp + "\n" + self.command + "\n" + self.path + "\n").encode() + body
        expected = "sha256=" + hmac.new(SECRET, signed, hashlib.sha256).hexdigest()
        if not hmac.compare_digest(expected, self.headers.get("X-Sundelion-Signature", "")):
            return self.error(401, "signature mismatch")

        if self.path != "/v1/plugin/process":
            return self.error(501, "not implemented")
        if not self.headers.get("Content-Type", "").lower().startswith("application/json"):
            return self.error(415, "content type must be application/json")
        try:
            ops = process(json.loads(body))
        except Exception as exc:  # il gateway applica failOpen
            return self.error(502, str(exc))
        self.reply(200, ops)


class UnixServer(socketserver.ThreadingMixIn, HTTPServer):
    address_family = socket.AF_UNIX
    daemon_threads = True

    def server_bind(self):
        socketserver.TCPServer.server_bind(self)
        self.server_name, self.server_port = "plugin", 0


def serve_unix(path):
    if os.path.exists(path):
        os.remove(path)
    server = UnixServer(path, Handler)
    try:
        server.serve_forever()
    finally:
        server.server_close()
        if os.path.exists(path):
            os.remove(path)


def serve_https(host, port):
    context = ssl.SSLContext(ssl.PROTOCOL_TLS_SERVER)
    context.minimum_version = ssl.TLSVersion.TLSv1_2
    context.load_cert_chain(os.environ["SUNDELION_PLUGIN_TLS_CERT"], os.environ["SUNDELION_PLUGIN_TLS_KEY"])
    server = ThreadingHTTPServer((host, port), Handler)
    server.daemon_threads = True
    server.socket = context.wrap_socket(server.socket, server_side=True)
    try:
        server.serve_forever()
    finally:
        server.server_close()


def main():
    signal.signal(signal.SIGTERM, lambda *_: sys.exit(0))  # il gateway ferma con SIGTERM
    if LISTEN.startswith("unix://"):
        serve_unix(LISTEN[len("unix://"):])
    elif LISTEN.startswith("https://"):
        u = urlparse(LISTEN)
        serve_https(u.hostname or "0.0.0.0", u.port or 8443)
    else:
        sys.exit("SUNDELION_PLUGIN_LISTEN deve essere unix:///percorso.sock o https://host:porta")


if __name__ == "__main__":
    main()
