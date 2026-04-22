#!/usr/bin/env bash
# scripts/gen-certs.sh — generate self-signed TLS certs for local dev
set -euo pipefail

CERT_DIR="${1:-certs}"
mkdir -p "$CERT_DIR"

openssl req -x509 -newkey rsa:4096 -sha256 -days 3650 -nodes \
  -keyout "$CERT_DIR/server.key" \
  -out    "$CERT_DIR/server.crt" \
  -subj   "/CN=localhost" \
  -addext "subjectAltName=DNS:localhost,IP:127.0.0.1"

echo "✅  Certs written to $CERT_DIR/"

