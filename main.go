package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"golang.org/x/crypto/sha3"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	numShards = 4
	blockTime = 2 * time.Second
	gasPrice  = 21000
	stateFile = "/home/scdo/parallel/state.json"
)

func keccak256(data []byte) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write(data)
	return h.Sum(nil)
}

type Account struct {
	Balance uint64            `json:"balance"`
	Nonce   uint64            `json:"nonce"`
	Code    []byte            `json:"code,omitempty"`
	Storage map[string]*big.Int `json:"storage,omitempty"`
}

type Tx struct {
	From   string `json:"from"`
	To     string `json:"to"`
	Amount uint64 `json:"amount"`
	Nonce  uint64 `json:"nonce"`
	Data   string `json:"data,omitempty"`
}

type Debt struct {
	ToAddr string
	Amount uint64
}

type TxInfo struct {
	Hash string
	From string
	To   string
	Data string
	Block uint64
	Status uint64
}

type Receipt struct {
	TxHash  string
	BlockNum uint64
	From    string
	To      string
	Status  uint64
	GasUsed uint64
	Logs    []LogEntry
}

type Node struct {
	mu       sync.Mutex
	shards   []*Shard
	pool     []Tx
	blocks   uint64
	receipts map[string]*Receipt
	erc20Bal map[string]*big.Int
	txHistory []TxInfo
	stateRoot string
	peers     []string
}

type Shard struct {
	ID       int
	Accounts map[string]*Account
	Debts    []Debt
}

func NewNode() *Node {
	n := &Node{receipts: make(map[string]*Receipt), erc20Bal: make(map[string]*big.Int)}
	n.peers = []string{"http://192.168.50.90:8050", "http://192.168.50.114:8050", "http://192.168.50.71:8050"}
	for i := 0; i < numShards; i++ {
		n.shards = append(n.shards, &Shard{ID: i, Accounts: make(map[string]*Account)})
	}
	return n
}

func (n *Node) computeStateRoot() string {
	h := sha3.NewLegacyKeccak256()
	for _, s := range n.shards {
		for addr, acc := range s.Accounts {
			h.Write([]byte(addr))
			b := make([]byte, 8)
			binary.BigEndian.PutUint64(b, acc.Balance)
			h.Write(b)
			binary.BigEndian.PutUint64(b, acc.Nonce)
			h.Write(b)
			h.Write(acc.Code)
		}
	}
	return fmt.Sprintf("0x%x", h.Sum(nil))
}

func (n *Node) broadcastTx(tx Tx) {
	for _, peer := range n.peers {
		body, _ := json.Marshal(map[string]interface{}{
			"jsonrpc": "2.0", "method": "eth_sendRawTransaction",
			"params": []interface{}{tx}, "id": 1,
		})
		go func(p string) {
			http.Post(p, "application/json", bytes.NewBuffer(body))
		}(peer)
	}
}

func (n *Node) syncPeers() {
	for {
		time.Sleep(5 * time.Second)
		for _, peer := range n.peers {
			resp, err := http.Post(peer, "application/json",
				bytes.NewBufferString(`{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}`))
			if err != nil { continue }
			var result struct {
				Result string `json:"result"`
			}
			json.NewDecoder(resp.Body).Decode(&result)
			resp.Body.Close()
			// If peer is ahead, we could sync blocks here
		}
	}
}

func (n *Node) syncFromPeer() {
	for _, peer := range n.peers {
		resp, err := http.Get(peer + "/state")
		if err != nil { continue }
		var state struct {
			Blocks   uint64                        `json:"blocks"`
			Balances map[string]uint64             `json:"balances"`
			Nonces   map[string]uint64             `json:"nonces"`
			Erc20    map[string]*big.Int           `json:"erc20"`
		}
		json.NewDecoder(resp.Body).Decode(&state)
		resp.Body.Close()
		if state.Blocks > n.blocks {
			n.mu.Lock()
			for addr, bal := range state.Balances {
				sid := n.ShardOf(addr)
				if sid >= 0 && sid < numShards {
					if n.shards[sid].Accounts[addr] == nil {
						n.shards[sid].Accounts[addr] = &Account{}
					}
					n.shards[sid].Accounts[addr].Balance = bal
					n.shards[sid].Accounts[addr].Nonce = state.Nonces[addr]
				}
			}
			for addr, bal := range state.Erc20 {
				n.erc20Bal[addr] = bal
			}
			n.blocks = state.Blocks
			n.stateRoot = n.computeStateRoot()
			n.mu.Unlock()
			fmt.Printf("Synced from %s: now at block %d\n", peer, state.Blocks)
			return
		}
	}
}

func (n *Node) ShardOf(addr string) int {
	if len(addr) < 3 { return 0 }
	if addr == "0xtestcontract" || addr == "0xcounter" { return 0 }
	// Standard 20-byte address: 0x + 40 hex chars
	// Use last byte to determine shard (0-3)
	if len(addr) >= 42 {
		last := addr[len(addr)-1]
		return int(last) % numShards
	}
	return int(addr[2]) % numShards
}

func (n *Node) AddFunded(addr string, balance uint64) {
	s := n.shards[n.ShardOf(addr)]
	s.Accounts[addr] = &Account{Balance: balance}
}

func (n *Node) AddTx(tx Tx) {
	n.mu.Lock()
	n.pool = append(n.pool, tx)
	n.mu.Unlock()
}

func txHash(tx Tx) string {
	return "0x" + fmt.Sprintf("%x", append([]byte(tx.From+tx.To), byte(tx.Amount), byte(tx.Nonce)))
}

func (n *Node) solvePoW() uint64 {
	// Simple PoW: find nonce such that SHA256(blockNumber + nonce) has N leading zero bits
	target := uint64(16) // difficulty: 16 leading zero bits
	for nonce := uint64(0); nonce < 1000000; nonce++ {
		h := keccak256([]byte(fmt.Sprintf("%d:%d", n.blocks, nonce)))
			leadingZeros := 0
			for _, b := range h {
				if b == 0 { leadingZeros += 8 } else { break }
			}
			if uint64(leadingZeros) >= target { return nonce }
	}
	return 0
}

func (n *Node) MineBlock() {
	n.mu.Lock()
	txs := n.pool
	n.pool = nil
	n.mu.Unlock()

	for _, s := range n.shards {
		for _, d := range s.Debts {
			targetShard := n.ShardOf(d.ToAddr)
			target := n.shards[targetShard]
			if acc, ok := target.Accounts[d.ToAddr]; ok {
				acc.Balance += d.Amount
			} else {
				target.Accounts[d.ToAddr] = &Account{Balance: d.Amount}
			}
		}
		s.Debts = nil
	}

	byShard := make([][]Tx, numShards)
	for _, tx := range txs {
		sid := n.ShardOf(tx.From)
		byShard[sid] = append(byShard[sid], tx)
	}

	var receipts []Receipt
	var wg sync.WaitGroup
	for s := 0; s < numShards; s++ {
		wg.Add(1)
		go func(sid int) {
			defer wg.Done()
			sh := n.shards[sid]
			for _, tx := range byShard[sid] {
				acc, ok := sh.Accounts[tx.From]
				if !ok || acc.Nonce != tx.Nonce || acc.Balance < tx.Amount+gasPrice {
					receipts = append(receipts, Receipt{TxHash: txHash(tx), BlockNum: n.blocks, From: tx.From, To: tx.To, Status: 0, GasUsed: 0})
					continue
				}
			if tx.To == "" || tx.To == "0x" {
				codeBytes, _ := hex.DecodeString(tx.Data[2:])
				// Generate 20-byte address from sender+nonce (simplified keccak)
				h := keccak256([]byte(tx.From + fmt.Sprintf("%d", tx.Nonce)))
				contractAddr := "0x" + fmt.Sprintf("%x", h[:20])
				// Run init code through EVM to get runtime bytecode
				runtimeCode, err := CallContract(codeBytes, nil, make(map[string]*big.Int))
				if err != nil || len(runtimeCode) == 0 {
					runtimeCode = codeBytes // fallback: store raw code
				}
				deployShard := n.shards[n.ShardOf(contractAddr)]
				deployShard.Accounts[contractAddr] = &Account{Code: runtimeCode, Storage: make(map[string]*big.Int)}
				acc.Nonce++
				receipts = append(receipts, Receipt{TxHash: txHash(tx), BlockNum: n.blocks, From: tx.From, To: contractAddr, Status: 1, GasUsed: gasPrice})
				continue
			}
				// ERC20 transfer to 0x10
				if tx.To == "0x10" || tx.To == "0x0000000000000000000000000000000000000010" {
					if len(tx.Data) >= 138 && tx.Data[:10] == "0xa9059cbb" {
						toAddr := "0x" + tx.Data[34:74]
						amount := new(big.Int)
						amount.SetString(tx.Data[74:138], 16)
						fromBal := n.erc20Bal[tx.From]
						if fromBal != nil && fromBal.Cmp(amount) >= 0 {
							fromBal.Sub(fromBal, amount)
							n.erc20Bal[toAddr] = new(big.Int).Add(n.erc20Bal[toAddr], amount)
							acc.Nonce++
							r := Receipt{TxHash: txHash(tx), BlockNum: n.blocks, From: tx.From, To: tx.To, Status: 1, GasUsed: gasPrice}
							n.receipts[r.TxHash] = &r
							n.mu.Lock(); n.txHistory = append(n.txHistory, TxInfo{r.TxHash, tx.From, tx.To, tx.Data, n.blocks, 1}); n.mu.Unlock()
						}
						continue
					}
				}
				toShard := n.ShardOf(tx.To)
				if toShard == sid {
					toAcc, ok := sh.Accounts[tx.To]
					if !ok { toAcc = &Account{}; sh.Accounts[tx.To] = toAcc }
					acc.Balance -= tx.Amount + gasPrice
					toAcc.Balance += tx.Amount
				} else {
					acc.Balance -= tx.Amount + gasPrice
					sh.Debts = append(sh.Debts, Debt{tx.To, tx.Amount})
				}
				acc.Nonce++
				r := Receipt{TxHash: txHash(tx), BlockNum: n.blocks, From: tx.From, To: tx.To, Status: 1, GasUsed: gasPrice}
				receipts = append(receipts, r)
				n.receipts[r.TxHash] = &r
				n.mu.Lock(); n.txHistory = append(n.txHistory, TxInfo{r.TxHash, tx.From, tx.To, tx.Data, n.blocks, 1}); n.mu.Unlock()
			}
		}(s)
	}
	wg.Wait()
	nonce := n.solvePoW()
	n.blocks++
	n.stateRoot = n.computeStateRoot()
	_ = nonce
	if n.blocks%5 == 0 { n.saveState() }
}

func (n *Node) saveState() {
	data, _ := json.MarshalIndent(map[string]interface{}{
		"blocks": n.blocks,
		"balances": func() map[string]uint64 {
			m := make(map[string]uint64)
			for _, s := range n.shards {
				for addr, acc := range s.Accounts { m[addr] = acc.Balance }
			}
			return m
		}(),
		"nonces": func() map[string]uint64 {
			m := make(map[string]uint64)
			for _, s := range n.shards {
				for addr, acc := range s.Accounts { m[addr] = acc.Nonce }
			}
			return m
		}(),
		"erc20": n.erc20Bal,
	}, "", "  ")
	os.WriteFile("/home/scdo/parallel/state.json", data, 0644)
}

func main() {
	node := NewNode()
	// Try load saved state
	if data, err := os.ReadFile("/home/scdo/parallel/state.json"); err == nil {
		var saved struct {
			Blocks   uint64
			Balances map[string]uint64
			Nonces   map[string]uint64
			Erc20    map[string]*big.Int
		}
		json.Unmarshal(data, &saved)
		node.blocks = saved.Blocks
		for addr, bal := range saved.Balances {
			sid := node.ShardOf(addr)
			nonce := uint64(0)
			if saved.Nonces != nil {
				nonce = saved.Nonces[addr]
			}
			node.shards[sid].Accounts[addr] = &Account{Balance: bal, Nonce: nonce}
		}
		for addr, bal := range saved.Erc20 {
			node.erc20Bal[addr] = bal
		}
		fmt.Printf("Restored state: block %d, %d accounts\n", node.blocks, len(saved.Balances))
		// Still need contract accounts
		node.shards[0].Accounts["0xtestcontract"] = &Account{
			Code:    []byte{0x60, 0x42, 0x60, 0x00, 0x52, 0x60, 0x20, 0x60, 0x00, 0xf3},
			Storage: make(map[string]*big.Int),
		}
		node.shards[0].Accounts["0xcounter"] = &Account{
			Code:    []byte{0x60, 0x2a, 0x60, 0x00, 0x55, 0x60, 0x00, 0x54, 0x60, 0x00, 0x52, 0x60, 0x20, 0x60, 0x00, 0xf3},
			Storage: make(map[string]*big.Int),
		}
	} else {
		genesis := []string{"0xaaaa", "0xbbbb", "0xcccc", "0xdddd",
			"0x70997970c51812dc3a010c7d01b50e0d17dc79c8",
			"0x3c44cdddb6a900fa2b585dd299e03d12fa4293bc"}
		for i, addr := range genesis {
			node.AddFunded(addr, 100000000)
			node.erc20Bal[addr] = new(big.Int).Mul(big.NewInt(1000000), big.NewInt(1e18))
			fmt.Printf("Genesis %d: %s\n", i, addr)
		}
		node.shards[0].Accounts["0xtestcontract"] = &Account{
			Code:    []byte{0x60, 0x42, 0x60, 0x00, 0x52, 0x60, 0x20, 0x60, 0x00, 0xf3},
			Storage: make(map[string]*big.Int),
		}
		node.shards[0].Accounts["0xcounter"] = &Account{
			Code:    []byte{0x60, 0x2a, 0x60, 0x00, 0x55, 0x60, 0x00, 0x54, 0x60, 0x00, 0x52, 0x60, 0x20, 0x60, 0x00, 0xf3},
			Storage: make(map[string]*big.Int),
		}
	}
	fmt.Println("Contracts: 0xtestcontract, 0xcounter")

	go func() {
		ticker := time.NewTicker(blockTime)
		for range ticker.C { node.MineBlock() }
	}()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		if r.URL.Path == "/state" {
			node.mu.Lock()
			defer node.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]interface{}{
				"blocks": node.blocks,
				"balances": func() map[string]uint64 {
					m := make(map[string]uint64)
					for _, s := range node.shards {
						for addr, acc := range s.Accounts { m[addr] = acc.Balance }
					}
					return m
				}(),
				"nonces": func() map[string]uint64 {
					m := make(map[string]uint64)
					for _, s := range node.shards {
						for addr, acc := range s.Accounts { m[addr] = acc.Nonce }
					}
					return m
				}(),
				"erc20": node.erc20Bal,
			})
			return
		}
		w.Header().Set("Content-Type", "application/json")
		var req struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
			ID     int             `json:"id"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		result := func(v interface{}) {
			json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "result": v, "id": req.ID})
		}

		switch req.Method {
		case "eth_blockNumber": result(fmt.Sprintf("0x%x", node.blocks))
		case "eth_getBlockByNumber":
			var p []interface{}; json.Unmarshal(req.Params, &p)
			blkHex, _ := p[0].(string)
			blkNum := node.blocks - 1
			if blkHex != "latest" && blkHex != "pending" && len(blkHex) > 2 {
				bi, _ := new(big.Int).SetString(blkHex[2:], 16); blkNum = bi.Uint64()
			}
			node.mu.Lock()
			var txs []TxInfo
			for _, t := range node.txHistory { if t.Block == blkNum { txs = append(txs, t) } }
			node.mu.Unlock()
			result(map[string]interface{}{
				"number": fmt.Sprintf("0x%x", blkNum),
				"hash": fmt.Sprintf("0x%x", blkNum),
				"parentHash": fmt.Sprintf("0x%x", blkNum-1),
				"timestamp": "0x60000000",
				"gasLimit": "0x1c9c380",
				"gasUsed": "0x5208",
				"transactions": txs,
				"miner": "0x0000000000000000000000000000000000000000",
				"difficulty": "0x0",
			})
		case "eth_chainId": result("0x238")
		case "net_version": result("568")
		case "eth_getBalance":
			var p []string; json.Unmarshal(req.Params, &p)
			acc := node.shards[node.ShardOf(p[0])].Accounts[p[0]]
			bal := uint64(0); if acc != nil { bal = acc.Balance }
			result(fmt.Sprintf("0x%x", bal))
		case "eth_getTransactionCount":
			var p []string; json.Unmarshal(req.Params, &p)
			acc := node.shards[node.ShardOf(p[0])].Accounts[p[0]]
			n := uint64(0); if acc != nil { n = acc.Nonce }
			result(fmt.Sprintf("0x%x", n))
		case "eth_getCode":
			var p []string; json.Unmarshal(req.Params, &p)
			acc := node.shards[node.ShardOf(p[0])].Accounts[p[0]]
			if acc != nil && len(acc.Code) > 0 { result("0x" + fmt.Sprintf("%x", acc.Code)) } else { result("0x") }
		case "eth_gasPrice": result("0x5208")
		case "eth_estimateGas": result("0x5208")
		case "eth_call":
			var p []map[string]interface{}; json.Unmarshal(req.Params, &p)
			to, _ := p[0]["to"].(string)
			data, _ := p[0]["data"].(string)
			isERC20 := to == "0x10" || to == "0x0000000000000000000000000000000000000010"
			if isERC20 {
				if len(data) >= 10 {
					selector := data[2:10]
					if selector == "70a08231" {
						addr := "0x" + data[34:74]
						bal := node.erc20Bal[addr]; result(fmt.Sprintf("0x%064x", bal))
					} else if selector == "18160ddd" {
						ts := new(big.Int).Mul(big.NewInt(6000000), big.NewInt(1e18)); result(fmt.Sprintf("0x%x", ts))
					} else if selector == "313ce567" {
						result("0x0000000000000000000000000000000000000000000000000000000000000012")
					} else {
						result("0x")
					}
				} else {
					result("0x")
				}
			} else {
				acc := node.shards[node.ShardOf(to)].Accounts[to]
				if acc != nil && len(acc.Code) > 0 {
					input, _ := hex.DecodeString(data[2:])
					e := NewEVM(acc.Code, input, acc.Storage)
					e.SetAddress(to)
					e.SetCallback(func(addr string, callInput []byte, delegate bool) ([]byte, error) {
						target := node.shards[node.ShardOf(addr)].Accounts[addr]
						if target != nil && len(target.Code) > 0 {
							if delegate {
								// DELEGATECALL: share caller's storage
								subEVM := NewEVM(target.Code, callInput, acc.Storage)
								subEVM.SetAddress(to)
								return subEVM.Run()
							}
							return CallContract(target.Code, callInput, target.Storage)
						}
						return []byte{}, nil
					})
					res, err := e.Run()
					if err == nil { result("0x" + fmt.Sprintf("%x", res)) } else { result("0x") }
				} else { result("0x") }
			}
		case "eth_sendRawTransaction":
			var p []interface{}; json.Unmarshal(req.Params, &p)
			if rawHex, ok := p[0].(string); ok && len(rawHex) > 10 && rawHex[:2] == "0x" {
				// RLP-encoded raw tx from MetaMask
				from, to, nonce, value, data, err := ParseRawTx(rawHex)
				if err != nil {
					result("0x" + fmt.Sprintf("%x", []byte("error")))
				} else {
					if from == "" { from = "0x70997970c51812dc3a010c7d01b50e0d17dc79c8" }
					tx := Tx{From: from, To: to, Amount: value, Nonce: nonce, Data: "0x" + hex.EncodeToString(data)}
					node.AddTx(tx)
					result(txHash(tx))
				}
			} else {
				// JSON object tx
				var p2 []json.RawMessage; json.Unmarshal(req.Params, &p2)
				var tx Tx; json.Unmarshal(p2[0], &tx)
				node.AddTx(tx)
				result(txHash(tx))
			}
		case "eth_getTransactionReceipt":
			var p []string; json.Unmarshal(req.Params, &p)
			node.mu.Lock()
			r, ok := node.receipts[p[0]]
			node.mu.Unlock()
			if !ok { result(nil) } else {
				result(map[string]interface{}{
					"transactionHash": r.TxHash,
					"blockNumber": fmt.Sprintf("0x%x", r.BlockNum),
					"from": r.From, "to": r.To,
					"status": fmt.Sprintf("0x%x", r.Status),
					"gasUsed": fmt.Sprintf("0x%x", r.GasUsed),
				})
			}
		case "web3_clientVersion": result("scdo-parallel/0.3")
		case "eth_getLogs":
			result([]interface{}{})
		case "eth_getLatestTxs":
			node.mu.Lock()
			txs := node.txHistory
			node.mu.Unlock()
			if len(txs) > 20 { txs = txs[len(txs)-20:] }
			result(txs)
		case "eth_getTransactionByHash":
			var p []string; json.Unmarshal(req.Params, &p)
			node.mu.Lock()
			r, ok := node.receipts[p[0]]
			node.mu.Unlock()
			if !ok { result(nil) } else {
				result(map[string]interface{}{
					"hash": r.TxHash,
					"blockNumber": fmt.Sprintf("0x%x", r.BlockNum),
					"from": r.From, "to": r.To,
					"gas": "0x5208", "gasPrice": "0x5208",
					"value": "0x0", "nonce": "0x0",
					"input": "0x",
				})
			}
		case "net_listening": result(true)
		case "net_peerCount": result("0x1")
		case "eth_protocolVersion": result("0x40")
		case "eth_getBlockByHash":
			var p []interface{}; json.Unmarshal(req.Params, &p)
			result(map[string]interface{}{
				"number": fmt.Sprintf("0x%x", node.blocks-1),
				"hash": p[0],
				"transactions": []interface{}{},
				"gasLimit": "0x1c9c380", "gasUsed": "0x5208",
				"timestamp": "0x60000000",
			})
		case "eth_feeHistory":
			result(map[string]interface{}{
				"baseFeePerGas": []string{"0x3b9aca00"},
				"gasUsedRatio": []float64{0.5},
				"oldestBlock": fmt.Sprintf("0x%x", node.blocks-10),
			})
		case "eth_syncing":
			result(false)
		default:
			json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "error": map[string]interface{}{"code": -32601, "message": req.Method}, "id": req.ID})
		}
	})

	http.HandleFunc("/blocks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		node.mu.Lock()
		type blkInfo struct {
			Number    uint64 `json:"number"`
			TxCount   int    `json:"txCount"`
		}
		var blks []blkInfo
		for i := uint64(0); i < 10; i++ {
			bn := node.blocks - 1 - i
			if bn > node.blocks { break }
			blks = append(blks, blkInfo{bn, len(node.txHistory)})
		}
		node.mu.Unlock()
		json.NewEncoder(w).Encode(blks)
	})

	http.HandleFunc("/ui", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		var sb strings.Builder
		sb.WriteString(`<!DOCTYPE html><html><head><meta charset="utf-8"><style>
body{font-family:system-ui;max-width:680px;margin:40px auto;padding:0 20px;background:#0d1117;color:#c9d1d9}
h1{color:#58a6ff} .card{background:#161b22;border:1px solid #30363d;border-radius:8px;padding:16px;margin:12px 0}
.big{font-size:28px;font-weight:700;color:#3fb950} input{background:#0d1117;border:1px solid #30363d;color:#c9d1d9;padding:8px;border-radius:4px;width:100%;box-sizing:border-box;margin:4px 0}
button{background:#238636;color:white;border:none;padding:10px 20px;border-radius:6px;cursor:pointer;margin-top:8px}
button:hover{background:#2ea043} .label{color:#8b949e;font-size:13px}
.wallet{background:#1f6feb} .addr{color:#58a6ff;font-family:monospace;font-size:12px;word-break:break-all}
</style></head><body>
<h1>SCDO Parallel Chain</h1>
<div class="card"><div class="label">Block Height</div><div class="big" id="h">...</div></div>
<div class="card">
<div class="card"><div class="label">Network</div>
<p style="margin:4px 0">ChainID: <span class="addr">0x238 (568)</span></p>
<p style="margin:4px 0">RPC: <span class="addr">http://192.168.50.50:8050</span></p>
<p style="margin:4px 0">Shards: 4 | BlockTime: 2s</p>
</div>
<button class="wallet" onclick="connect()">Connect MetaMask</button>
<p id="wallet" class="addr">Not connected</p>
</div>
<div class="card"><div class="label">Balance</div>
<input id="addr" value="0x70997970c51812dc3a010c7d01b50e0d17dc79c8">
<button onclick="look()">查询</button><p id="bal" style="margin-top:8px"></p><p id="tbal"></p></div>
<div class="card"><div class="label">Transfer ERC20</div>
<input id="to" placeholder="to" value="0x3c44cdddb6a900fa2b585dd299e03d12fa4293bc">
<input id="amt" placeholder="amount" value="1">
<button onclick="send()">发送</button><p id="txres"></p></div>
<div class="card"><div class="label">Faucet</div>
<button onclick="faucet()">领取测试币</button><p id="faucetres"></p></div>
<div class="card"><div class="label">Recent Transactions</div><div id="txlist"><span style="color:#8b949e">loading...</span></div></div>
<script>
async function rpc(m,p){const r=await fetch('/',{method:'POST',headers:{'Content-Type':'application/json'},body:JSON.stringify({method:m,params:p,id:1})});return (await r.json()).result}
async function h(){document.getElementById('h').textContent=parseInt(await rpc('eth_blockNumber'),16)}
async function look(){const a=document.getElementById('addr').value;const b=await rpc('eth_getBalance',[a,'latest']);document.getElementById('bal').textContent='Native: '+parseInt(b,16);const d='0x70a08231'+'0'.repeat(24)+a.slice(2);const t=await rpc('eth_call',[{to:'0x10',data:d},'latest']);document.getElementById('tbal').textContent='ERC20: '+(parseInt(t,16)/1e18).toFixed(2)}
async function send(){const from=document.getElementById('addr').value;const to=document.getElementById('to').value;const amt=parseFloat(document.getElementById('amt').value);const data='0xa9059cbb'+'0'.repeat(24)+to.slice(2)+Math.floor(amt*1e18).toString(16).padStart(64,'0');const hash=await rpc('eth_sendRawTransaction',[{from:from,to:'0x10',amount:0,nonce:0,data:data}]);document.getElementById('txres').textContent='TX: '+hash;setTimeout(look,3000)}
async function faucet(){const a=document.getElementById('addr').value;const r=await fetch('/faucet?addr='+a);const j=await r.json();document.getElementById('faucetres').textContent='Got: '+j.native+' native, '+(parseFloat(j.erc20)/1e18).toFixed(2)+' ERC20';setTimeout(look,1000)}
async function txlist(){const txs=await rpc('eth_getLatestTxs',[]);let html='';for(const t of (txs||[]).reverse()){html+=t.Status+' '+t.From.slice(0,10)+'->'+t.To.slice(0,10)+' blk'+t.Block+'<br>'}document.getElementById('txlist').innerHTML=html||'none'}
async function connect(){
if(typeof window.ethereum==='undefined'){alert('Please install MetaMask');return}
try{
await window.ethereum.request({method:'wallet_addEthereumChain',params:[{
chainId:'0x238',chainName:'SCDO Parallel',nativeCurrency:{name:'SCDO',symbol:'SCDO',decimals:18},
rpcUrls:['http://192.168.50.50:8050'],blockExplorerUrls:['http://192.168.50.50:8050/ui']
}]});
const accounts=await window.ethereum.request({method:'eth_requestAccounts'});
document.getElementById('wallet').textContent='Connected: '+accounts[0];
document.getElementById('addr').value=accounts[0];
look();
}catch(e){alert('Error: '+e.message)}
}
setInterval(h,2000);setInterval(txlist,3000);look();txlist();
</script></body></html>`)
		fmt.Fprint(w, sb.String())
	})

	http.HandleFunc("/state", func(w http.ResponseWriter, r *http.Request) {
		node.mu.Lock()
		defer node.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]interface{}{
			"blocks": node.blocks,
			"balances": func() map[string]uint64 {
				m := make(map[string]uint64)
				for _, s := range node.shards {
					for addr, acc := range s.Accounts { m[addr] = acc.Balance }
				}
				return m
			}(),
			"nonces": func() map[string]uint64 {
				m := make(map[string]uint64)
				for _, s := range node.shards {
					for addr, acc := range s.Accounts { m[addr] = acc.Nonce }
				}
				return m
			}(),
			"erc20": node.erc20Bal,
		})
	})
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]interface{}{"status":"ok","block":fmt.Sprintf("0x%x", node.blocks),"shards":numShards,"stateRoot":node.stateRoot,"peers":len(node.peers)})
	})
	http.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "scdo_block_height %d\n", node.blocks)
		fmt.Fprintf(w, "scdo_shards %d\n", numShards)
	})
	http.HandleFunc("/faucet", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		addr := r.URL.Query().Get("addr")
		if len(addr) < 3 { json.NewEncoder(w).Encode(map[string]string{"error": "missing addr"}); return }
		sid := node.ShardOf(addr)
		acc, ok := node.shards[sid].Accounts[addr]
		if !ok { acc = &Account{}; node.shards[sid].Accounts[addr] = acc }
		acc.Balance += 1000000
		if node.erc20Bal[addr] == nil { node.erc20Bal[addr] = big.NewInt(0) }
		node.erc20Bal[addr].Add(node.erc20Bal[addr], new(big.Int).Mul(big.NewInt(1000), big.NewInt(1e18)))
		eb := node.erc20Bal[addr]; if eb == nil { eb = big.NewInt(0) }
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "addr": addr, "native": acc.Balance, "erc20": eb.String()})
	})
fmt.Println("SCDO v0.3 on :8050")
	go node.syncFromPeer()
	go node.syncPeers()
	http.ListenAndServe(":8050", nil)
}
