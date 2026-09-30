# scdo-shard0-parallel-legacy: retired SCDO shard 0 parallel-node (archived)

> **Retired (2026-09-28). Not what runs shard 0 today.** This is the old `parallel-node` code. It still has chain ID 568 in `main.go` and was stopped on 2026-09-28. Shard 0 now runs a core-geth (Ethash PoW) fork with chain ID 5680: see [SCDOLAB/scdo-shard0](https://github.com/SCDOLAB/scdo-shard0). This repository is archived and kept for reference only.

> **Compliance notice.** The public shard 0 endpoint and the explorer at
> [scdoscan.io](https://scdoscan.io) are operated by **9Y9 PTY LTD** (3/251 Blackburn Rd, Mount Waverley VIC 3149 (Melbourne), Australia;
> ACN 600 445 118, ABN 19 600 445 118; see the [compliance page](https://scdoscan.io/compliance.html)). 9Y9 PTY LTD is registered with
> [AUSTRAC VASP register](https://online.apps.austrac.gov.au/vaspr) as a Digital Currency Exchange provider,
> registration **DCE100714503-001** (valid until 14 March 2029), and is a member of the
> Australian Financial Complaints Authority (**AFCA member 124589**).
> Registration does not mean AUSTRAC endorses or approves 9Y9 PTY LTD, SCDO or any
> product or service. This repository is open-source software kept for reference.

`parallel-node` is the node that runs **SCDO shard 0**: a single-node, EVM-compatible
chain with Ethereum-style JSON-RPC, so standard wallets (MetaMask, ethers.js, web3.js)
can connect to it. It is a small, dependency-light Go program (one binary, about 2,500
lines). It is separate from the go-scdo PoW shards 1-4.

| | |
|---|---|
| Chain ID | **5680** (`0x1630`) |
| Currency | SCDO, **18 decimals** (1 SCDO = 10^18 wei) |
| Gas price | **10^10 wei** (10 gwei), flat 21000 gas per transaction, so the fee is 0.00021 SCDO |
| Block time | **2 s** |
| Public RPC | `https://scdoscan.io/rpc/0` |
| Explorer | <https://scdoscan.io> |
| Client version | `scdo-parallel/0.5.0` (this source; the live node reported `0.4` when this snapshot was taken) |

## Add to MetaMask

Manually: *Networks > Add network > Add a network manually*:

| Field | Value |
|---|---|
| Network name | SCDO Shard 0 |
| RPC URL | `https://scdoscan.io/rpc/0` |
| Chain ID | `5680` |
| Currency symbol | `SCDO` |
| Block explorer | `https://scdoscan.io` |

Or from a web page:

```js
await window.ethereum.request({
  method: 'wallet_addEthereumChain',
  params: [{
    chainId: '0x1630',
    chainName: 'SCDO Shard 0',
    nativeCurrency: { name: 'SCDO', symbol: 'SCDO', decimals: 18 },
    rpcUrls: ['https://scdoscan.io/rpc/0'],
    blockExplorerUrls: ['https://scdoscan.io'],
  }],
});
```

Quick check:

```bash
curl -s -X POST https://scdoscan.io/rpc/0 -H 'Content-Type: application/json' \
  -d '{"jsonrpc":"2.0","method":"eth_chainId","params":[],"id":1}'
# {"id":1,"jsonrpc":"2.0","result":"0x1630"}
```

## JSON-RPC methods

The list below comes from `handleRPC` in `main.go`. Batch requests (JSON arrays) are supported.
Anything else returns `-32601 method not found`.

| Method | Notes |
|---|---|
| `eth_chainId`, `net_version` | `0x1630` / `"5680"` |
| `web3_clientVersion` | `scdo-parallel/0.5.0` |
| `eth_blockNumber` | |
| `eth_getBlockByNumber`, `eth_getBlockByHash` | Tags: `latest`, `pending`, `safe`, `finalized` (all mean head), `earliest`, or a hex number; full-tx flag supported |
| `eth_getBlockTransactionCountByNumber`, `eth_getBlockTransactionCountByHash` | |
| `eth_getTransactionByHash` | Also returns pending pool transactions (with null block fields) |
| `eth_getTransactionByBlockNumberAndIndex` | |
| `eth_getTransactionReceipt` | `logs` is always empty, `logsBloom` is zero |
| `eth_getBalance`, `eth_getTransactionCount`, `eth_getCode` | Latest state only (the block argument is ignored, except `pending` for the nonce) |
| `eth_call` | Read-only execution with the built-in interpreter at the latest state |
| `eth_sendRawTransaction` | Signed legacy (EIP-155), EIP-2930 and EIP-1559 transactions. Chain ID 5680 is required and gasPrice / maxFeePerGas must be at least 10^10 wei. Unsigned JSON transactions are rejected |
| `eth_gasPrice` | `0x2540be400` (10^10 wei) |
| `eth_maxPriorityFeePerGas` | `0x0` |
| `eth_estimateGas` | Always `0x5208` (21000) |
| `eth_feeHistory` | Minimal (base fee 0) |
| `eth_getLogs` | **Stub: always returns `[]`** |
| `eth_syncing` (`false`), `eth_mining` (`true`), `eth_hashrate`, `eth_coinbase`, `eth_accounts` (`[]`), `eth_protocolVersion`, `net_listening`, `net_peerCount` | Static or informational |
| `scdo_nativeCurrency` | Non-standard: symbol, decimals, gas price, fee, faucet amount, `decimalsMigrationBlock` |
| `eth_getLatestTxs` | Non-standard: the 20 most recent transactions |

Other HTTP endpoints on the same port: `GET /health` (height, head hash, state root, version),
`GET /metrics` (Prometheus text), `GET /blocks` (last 10 blocks), `GET /state` (all balances),
`GET /faucet?addr=0x...` (test funds, rate-limited: 1 claim per address per 24 h and
per IP per hour), `GET /ui` (demo page).

## Known limits

Please read these before you build on shard 0:

- **Single node.** One node produces all blocks. There is no peer-to-peer block sync or
  consensus between nodes (`SCDO_PEERS` only pings peers). Availability and ordering depend
  on the operator. The header PoW seal (16-bit keccak) is a formality, not a security mechanism.
- **No event logs.** `eth_getLogs` always returns `[]` and receipts contain no logs, so dApps
  and indexers that depend on events will not work yet.
- **Legacy gas only.** Every transaction is charged a flat 21000 gas at 10^10 wei
  (0.00021 SCDO), whatever gas limit or price it sets. Blocks have no base fee. EIP-1559
  transactions are accepted, but `maxFeePerGas` is only checked against the minimum and the
  priority fee is ignored. `eth_estimateGas` always returns 21000.
- **Limited smart-contract support.** Deployment stores the runtime code. The contract address
  is the first 20 bytes of `keccak256` of the ASCII string `<lowercase 0x sender><decimal nonce>`, which is **not** the Ethereum `CREATE` address.
  Transactions sent *to* a contract do not execute its code (only value moves). Contract
  state can be read with `eth_call` through a partial EVM interpreter. A built-in demo token
  at `0x…0010` supports `balanceOf`, `totalSupply`, `decimals` and `transfer`.
- **Latest state only.** Historical balance/nonce/code queries by block number are not supported.
- **Decimals migration.** Before block 59754 on the live chain, amounts were in 8-decimal
  units (1e-8 SCDO). `scdo_nativeCurrency.decimalsMigrationBlock` reports the boundary, and
  transactions in earlier blocks keep their original values.
- **Simple storage.** State is one JSON file that is rewritten every block. Headers and txs
  are append-only files. This is fine for the current load but will not scale to large state.
- **Internal partitions.** `numShards = 4` means internal execution partitions inside this
  node (transfers between partitions settle in the next block). They are unrelated to the
  go-scdo shards 1-4.
- The `/ui` demo's "Transfer ERC20" button still uses the removed unsigned-transaction
  form, so it returns an error. Use a wallet instead.

## Build and run

Requires Go 1.26+ (from `go.mod`; with `GOTOOLCHAIN=auto` an older `go` downloads it).

```bash
make build            # CGO_ENABLED=0 go build -trimpath -o parallel-node .
make run              # starts a local chain in ./data on :8050
# or
SCDO_DATADIR=$PWD/data ./parallel-node
```

Environment variables:

| Variable | Default | Meaning |
|---|---|---|
| `SCDO_DATADIR` | `/root/parallel` | Directory for `state.json`, `headers.dat`, `txs.jsonl`, `faucet.json` |
| `SCDO_COINBASE` | derived address | Fee recipient (0x + 40 hex) |
| `SCDO_PEERS` | none | Comma-separated peer URLs (health ping only) |

The node listens on `:8050` on all interfaces with CORS `*`. In production, run it behind a
reverse proxy that sets `X-Real-IP` (used by the faucet rate limit) and firewall port
8050. `parallel-node.service` is a hardened systemd unit (dedicated `scdo` user,
`/var/lib/scdo-parallel`).

**Fresh chains.** When no `state.json` exists, the node creates a demo genesis that funds a
few placeholder addresses. Two of them are the well-known Hardhat/Anvil test accounts
(`0x7099…79c8`, `0x3c44…93bc`), whose private keys are public. Change the genesis list in
`main()` before you run any network that other people use.

Never commit chain data: `.gitignore` excludes `state.json`, `headers.dat`, `txs.jsonl`
and `faucet.json` (faucet.json holds client IP addresses).

## Security

See [SECURITY.md](SECURITY.md). Report vulnerabilities to admin@apeccapital.org.

## License

MIT © 2026 9Y9 PTY LTD. See [LICENSE](LICENSE).
