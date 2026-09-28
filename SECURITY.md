# Security Policy

## Reporting a vulnerability

Please report security issues privately by email to **admin@apeccapital.org**.
Do not open a public GitHub issue for anything that could put user funds or the
network at risk.

Please include:

- a description of the issue and its impact,
- steps to reproduce (a JSON-RPC request, signed transaction or script),
- the version you tested (`web3_clientVersion`, e.g. `scdo-parallel/0.5.0`).

We aim to acknowledge reports within 3 business days (Australia/Melbourne time) and will
keep you updated until the issue is resolved. Please give us reasonable time to fix
the issue before you disclose it.

## Scope

- This repository (the shard 0 node: JSON-RPC, transaction validation, block production, faucet).
- The public endpoint `https://scdoscan.io/rpc/0`.

Out of scope: denial of service by traffic volume, third-party wallets, and social engineering.

## Supported versions

Only the latest release on `main` receives fixes.
