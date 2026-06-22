# backitup — TODOS

## ~~Client credential lifecycle (rotation / bootstrap)~~ ✅ DONE — v0.1.0.0
Shipped in `feat/client-credential-rotation`. `POST /clients/{id}/rotate` reissues SSH keypair + bearer token atomically. Old credentials invalidated immediately; new secrets surfaced once in the UI. Compare-and-swap version column guards against concurrent rotation races.
