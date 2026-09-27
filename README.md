# SCDO Parallel EVM Node

> 并行分片 EVM 链 PoC — 4 shard 并行出块，异步 Debt 桥跨片结算

## 架构

```
┌─────────────────────────────────────────┐
│           JSON-RPC (hex, EVM)          │
│           MetaMask / Hardhat / UI      │
└──────────────┬──────────────────────────┘
               │
    ┌──────────┼──────────┐
    ▼          ▼          ▼
 Shard 0    Shard 1    Shard 2    Shard 3
 (goroutine)(goroutine)(goroutine)(goroutine)
    │          │          │          │
    └──────────┴──────────┴──────────┘
               │
         Debt Bridge (异步)
    跨片转账下一块结算，无锁并行
```

**核心设计**：
- 4 shard 各自独立 StateDB，goroutine 并行出块
- 地址末字节 `% 4` 路由到对应 shard
- 跨片交易走异步 Debt，下一块 credit 结算
- 2 秒出块时间
- JSON 文件持久化（每 5 块保存）

## 快速开始

```bash
go build -o scdo-node .
./scdo-node
# RPC: http://localhost:8050
# UI:  http://localhost:8050/ui
```

## MetaMask 连接

| 字段 | 值 |
|------|-----|
| Network Name | SCDO Parallel |
| RPC URL | `http://192.168.50.50:8050` |
| Chain ID | `568` (0x238) |
| Currency Symbol | SCDO |
| Block Explorer | `http://192.168.50.50:8050/ui` |

## ERC-20 Token

合约地址: `0x0000000000000000000000000000000000000010`

```
totalSupply: 6,000,000 SCDO
decimals: 18
```

Genesis 账户各 1,000,000 SCDO:
- `0x70997970c51812dc3a010c7d01b50e0d17dc79c8`
- `0x3c44cdddb6a900fa2b585dd299e03d12fa4293bc`

## RPC 方法

| 方法 | 说明 |
|------|------|
| `eth_blockNumber` | 当前高度 (hex) |
| `eth_chainId` | 0x238 (568) |
| `net_version` | 568 |
| `net_listening` | true |
| `net_peerCount` | 0x1 |
| `eth_protocolVersion` | 0x40 |
| `eth_syncing` | false |
| `eth_getBalance` | 原生余额 (hex wei) |
| `eth_getTransactionCount` | nonce (hex) |
| `eth_getCode` | 合约字节码 |
| `eth_call` | 执行合约调用 |
| `eth_sendRawTransaction` | 发送交易 (JSON/RLP) |
| `eth_getTransactionByHash` | 交易详情 |
| `eth_getTransactionReceipt` | 交易回执 |
| `eth_getBlockByNumber` | 区块信息 |
| `eth_getLogs` | 事件日志 |
| `eth_getLatestTxs` | 最近 20 笔交易 |
| `eth_gasPrice` | 0x5208 (21000) |
| `eth_estimateGas` | 0x5208 |
| `web3_clientVersion` | scdo-parallel/0.3 |

## EVM 支持

操作码: PUSH1-32, DUP1-16, SWAP1-16, ADD, MUL, SUB, POP, MSTORE, MLOAD, MSIZE, SLOAD, SSTORE, JUMP, JUMPI, PC, CODECOPY, CODESIZE, CALLDATALOAD, LOG0-4, RETURN, REVERT, STOP

预部署合约:
- `0xtestcontract` — 返回 0x42
- `0xcounter` — SSTORE 42, SLOAD, RETURN
- `0x10` — ERC-20 代币

## 文件结构

```
parallel/
├── main.go    # 节点主逻辑 + RPC handler + UI
├── evm.go     # 最小 EVM 解释器
├── rlp.go     # 最小 RLP 解码器
├── go.mod
├── state.json # 持久化状态 (自动生成)
└── scdo-node  # 编译后二进制
```

## systemd

```ini
# /etc/systemd/system/parallel-node.service
[Unit]
Description=SCDO Parallel Node
[Service]
ExecStart=/home/scdo/parallel/scdo-node
Restart=always
User=scdo
[Install]
WantedBy=multi-user.target
```

## 路线图

- [x] 4 shard 并行出块
- [x] 标准 hex RPC (MetaMask 兼容)
- [x] ERC-20 代币合约
- [x] 最小 EVM 解释器
- [x] RLP 交易解析
- [x] Web UI 仪表盘
- [x] JSON 持久化
- [ ] ECDSA 签名验证
- [ ] P2P 网络
- [ ] Merkle Patricia Trie
- [ ] Blockscout 挂载
- [ ] 跨链桥
