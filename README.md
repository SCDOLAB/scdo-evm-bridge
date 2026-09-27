# SCDO Parallel EVM

A home-grown PoW blockchain with native EVM support and 4-shard parallel execution.

## Features

- **PoW consensus** — keccak256 mining, adjustable difficulty, 2s block time
- **4-shard parallel execution** — transactions routed by address, cross-shard debt settlement
- **EVM compatible** — 45+ opcodes including Shanghai (PUSH0) and Cancun (MCOPY, BASEFEE)
- **Precompiles** — ecrecover(0x01), sha256(0x02), ripemd160(0x03), identity(0x04), BLS pairing(0x08)
- **Smart contracts** — CALL/DELEGATECALL/STATICCALL/CREATE/CREATE2, ERC-20 precompile at 0x10
- **P2P networking** — peer discovery, broadcast, startup state sync
- **MetaMask ready** — ChainID 0x238 (568), standard eth_* JSON-RPC
- **Web UI** — dark-themed block explorer at /ui

## Quick Start

```bash
go build -o scdo-node .
./scdo-node
```

Node listens on :8050. Add to MetaMask:
- RPC URL: http://<node-ip>:8050
- Chain ID: 0x238 (568)
- Symbol: SCDO

## RPC Methods

eth_chainId, eth_blockNumber, eth_getBalance, eth_gasPrice, eth_sendRawTransaction,
eth_call, eth_getCode, eth_estimateGas, eth_getTransactionReceipt, net_version,
+ faucet, /health, /blocks, /ui, /state

## Architecture

```
main.go   — node core: PoW mining, 4 shards, P2P, RPC, Web UI
evm.go    — EVM interpreter: 45+ opcodes, precompiles, CALL/CREATE
rlp.go    — RLP decoder for transactions
```

## Network

Running on 4 home machines: .50, .90 (Linux), .114, .71 (Windows).