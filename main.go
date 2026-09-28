package main

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/crypto/sha3"
)

const (
	numShards = 4
	blockTime = 2 * time.Second
	txGas     = 21000 // flat gas charged per tx
	chainID   = 568
	// Native currency: 18 decimals since the 2026-09-28 migration (1 SCDO = 1e18 wei).
	// Before that the ledger used 8 decimals (1 SCDO = 1e8 units); stored balances were
	// multiplied by legacyScale once at startup (see main()), so real values are unchanged.
	nativeDecimals = 18
	legacyDecimals = 8
	weiPerGas      = 10000000000 // eth_gasPrice = 1e10 wei; 21000 gas * 1e10 = 2.1e14 wei = 0.00021 SCDO (same real fee as before)
	powBits        = 16
	powDifficulty  = 1 << powBits
	blockGasLimit  = 30000000
	extraData      = "scdo-parallel/0.4" // hashed into block headers: must stay constant for existing chain data
	clientVersion  = "scdo-parallel/0.5.0"
	// genesisTime: lower bound for back-filled legacy header timestamps.
	genesisTime = 1790000000
)

// Data directory (override with SCDO_DATADIR for testing).
var dataDir = func() string {
	if d := os.Getenv("SCDO_DATADIR"); d != "" {
		return d
	}
	return "/root/parallel"
}()
var stateFile = dataDir + "/state.json"

var (
	legacyScale = new(big.Int).Exp(big.NewInt(10), big.NewInt(nativeDecimals-legacyDecimals), nil) // 1e10
	txFeeWei    = new(big.Int).Mul(big.NewInt(txGas), big.NewInt(weiPerGas))                       // 2.1e14 wei
	faucetWei   = new(big.Int).Mul(big.NewInt(1000000), legacyScale)                               // 1e16 wei = 0.01 SCDO (was 1e6 units)
	genesisWei  = new(big.Int).Exp(big.NewInt(10), big.NewInt(nativeDecimals), nil)                // 1 SCDO
	maxUint256  = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 256), big.NewInt(1))
)

func hexBig(v *big.Int) string {
	if v == nil {
		return "0x0"
	}
	return "0x" + v.Text(16)
}

var coinbaseAddr = func() string {
	if v := os.Getenv("SCDO_COINBASE"); len(v) == 42 {
		return strings.ToLower(v)
	}
	return "0x" + hex.EncodeToString(keccak256([]byte("scdo-shard0-parallel-coinbase"))[12:])
}()

func coinbaseBytes() []byte { b, _ := hex.DecodeString(coinbaseAddr[2:]); return b }

func keccak256(data []byte) []byte {
	h := sha3.NewLegacyKeccak256()
	h.Write(data)
	return h.Sum(nil)
}

func norm(a string) string  { return strings.ToLower(strings.TrimSpace(a)) }
func hexU(v uint64) string  { return fmt.Sprintf("0x%x", v) }
func h32(b [32]byte) string { return "0x" + hex.EncodeToString(b[:]) }

type Account struct {
	Balance *big.Int            `json:"balance"` // wei (18 decimals)
	Nonce   uint64              `json:"nonce"`
	Code    []byte              `json:"code,omitempty"`
	Storage map[string]*big.Int `json:"storage,omitempty"`
}

// bal returns the (non-nil) balance of a, allocating a zero balance if needed.
func (a *Account) bal() *big.Int {
	if a.Balance == nil {
		a.Balance = new(big.Int)
	}
	return a.Balance
}

// Tx is a pooled (pending) transaction.
type Tx struct {
	From     string   `json:"from"`
	To       string   `json:"to"`
	Amount   *big.Int `json:"amount"` // wei
	Nonce    uint64   `json:"nonce"`
	Data     string   `json:"data,omitempty"`
	Hash     string   `json:"-"`
	Gas      uint64   `json:"-"`
	GasPrice string   `json:"-"`
	Type     uint8    `json:"-"`
	ChainID  uint64   `json:"-"`
	V, R, S  string   `json:"-"`
}

type Debt struct {
	ToAddr string
	Amount *big.Int
}

type TxInfo struct {
	Hash   string
	From   string
	To     string
	Data   string
	Block  uint64
	Status uint64
}

type Node struct {
	mu        sync.Mutex
	shards    []*Shard
	pool      []Tx
	blocks    uint64
	erc20Bal  map[string]*big.Int
	stateRoot string
	peers     []string
	store     *ChainStore
	// first block produced with 18-decimal accounting (txs in earlier blocks carry 8-decimal values)
	decimalsMigrationBlock uint64
}

type Shard struct {
	ID       int
	Accounts map[string]*Account
	Debts    []Debt
}

func NewNode() *Node {
	n := &Node{erc20Bal: make(map[string]*big.Int)}
	// Peers disabled by default (old LAN peers are unreachable); set SCDO_PEERS=http://a:8050,http://b:8050 to enable.
	if p := os.Getenv("SCDO_PEERS"); p != "" {
		n.peers = strings.Split(p, ",")
	}
	for i := 0; i < numShards; i++ {
		n.shards = append(n.shards, &Shard{ID: i, Accounts: make(map[string]*Account)})
	}
	return n
}

func (n *Node) account(addr string) *Account {
	return n.shards[n.ShardOf(addr)].Accounts[addr]
}

func (n *Node) computeStateRoot() []byte {
	h := sha3.NewLegacyKeccak256()
	var addrs []string
	accs := map[string]*Account{}
	for _, s := range n.shards {
		for addr, acc := range s.Accounts {
			addrs = append(addrs, addr)
			accs[addr] = acc
		}
	}
	sort.Strings(addrs)
	b := make([]byte, 8)
	bb := make([]byte, 32)
	for _, addr := range addrs {
		acc := accs[addr]
		h.Write([]byte(addr))
		acc.bal().FillBytes(bb) // 32-byte big-endian wei balance
		h.Write(bb)
		binary.BigEndian.PutUint64(b, acc.Nonce)
		h.Write(b)
		h.Write(acc.Code)
	}
	var e []string
	for a := range n.erc20Bal {
		e = append(e, a)
	}
	sort.Strings(e)
	for _, a := range e {
		h.Write([]byte(a))
		if v := n.erc20Bal[a]; v != nil {
			h.Write(v.Bytes())
		}
	}
	return h.Sum(nil)
}

func (n *Node) syncPeers() {
	for {
		time.Sleep(5 * time.Second)
		for _, peer := range n.peers {
			c := http.Client{Timeout: 3 * time.Second}
			resp, err := c.Post(peer, "application/json",
				bytes.NewBufferString(`{"jsonrpc":"2.0","method":"eth_blockNumber","params":[],"id":1}`))
			if err != nil {
				continue
			}
			resp.Body.Close()
		}
	}
}

func (n *Node) ShardOf(addr string) int {
	if len(addr) < 3 {
		return 0
	}
	if addr == "0xtestcontract" || addr == "0xcounter" {
		return 0
	}
	if len(addr) >= 42 {
		last := addr[len(addr)-1]
		return int(last) % numShards
	}
	return int(addr[2]) % numShards
}

func (n *Node) AddFunded(addr string, balance *big.Int) {
	s := n.shards[n.ShardOf(addr)]
	s.Accounts[addr] = &Account{Balance: new(big.Int).Set(balance)}
}

func (n *Node) AddTx(tx Tx) {
	n.mu.Lock()
	n.pool = append(n.pool, tx)
	n.mu.Unlock()
}

// solvePoW finds nonce such that keccak(sealHash || nonce) has powBits leading zero bits.
func solvePoW(sealHash []byte) (uint64, []byte) {
	buf := make([]byte, 40)
	copy(buf, sealHash)
	for nonce := uint64(0); nonce < 1<<24; nonce++ {
		binary.BigEndian.PutUint64(buf[32:], nonce)
		h := keccak256(buf)
		if h[0] == 0 && h[1] == 0 {
			return nonce, h
		}
	}
	return 0, make([]byte, 32)
}

func powMix(sealHash []byte, nonce uint64) []byte {
	buf := make([]byte, 40)
	copy(buf, sealHash)
	binary.BigEndian.PutUint64(buf[32:], nonce)
	return keccak256(buf)
}

type deploy struct {
	addr string
	code []byte
}

func (n *Node) MineBlock() {
	n.mu.Lock()
	defer n.mu.Unlock()
	txs := n.pool
	n.pool = nil
	number := n.blocks

	for _, s := range n.shards {
		for _, d := range s.Debts {
			target := n.shards[n.ShardOf(d.ToAddr)]
			if acc, ok := target.Accounts[d.ToAddr]; ok {
				acc.bal().Add(acc.bal(), d.Amount)
			} else {
				target.Accounts[d.ToAddr] = &Account{Balance: new(big.Int).Set(d.Amount)}
			}
		}
		s.Debts = nil
	}

	byShard := make([][]Tx, numShards)
	for _, tx := range txs {
		sid := n.ShardOf(tx.From)
		byShard[sid] = append(byShard[sid], tx)
	}

	results := make([][]*StoredTx, numShards)
	deploys := make([][]deploy, numShards)
	fees := make([]uint64, numShards) // gas units charged per internal shard
	var xmu sync.Mutex                // guards shared erc20Bal map
	var wg sync.WaitGroup
	for s := 0; s < numShards; s++ {
		wg.Add(1)
		go func(sid int) {
			defer wg.Done()
			sh := n.shards[sid]
			for _, tx := range byShard[sid] {
				st := &StoredTx{Hash: tx.Hash, From: tx.From, To: tx.To, Value: hexBig(tx.Amount), Nonce: tx.Nonce,
					Gas: tx.Gas, GasPrice: tx.GasPrice, Input: tx.Data, Type: tx.Type, ChainID: tx.ChainID, V: tx.V, R: tx.R, S: tx.S}
				if st.Input == "" {
					st.Input = "0x"
				}
				if st.Gas == 0 {
					st.Gas = 21000
				}
				if st.GasPrice == "" {
					st.GasPrice = hexU(weiPerGas)
				}
				results[sid] = append(results[sid], st)
				acc, ok := sh.Accounts[tx.From]
				cost := new(big.Int).Add(tx.Amount, txFeeWei)
				if !ok || acc.Nonce != tx.Nonce || acc.bal().Cmp(cost) < 0 {
					st.Status, st.GasUsed = 0, 0 // rejected: no state change, no fee
					continue
				}
				// contract creation
				if tx.To == "" || tx.To == "0x" {
					st.To = ""
					codeBytes := []byte{}
					if len(tx.Data) > 2 {
						codeBytes, _ = hex.DecodeString(tx.Data[2:])
					}
					h := keccak256([]byte(tx.From + fmt.Sprintf("%d", tx.Nonce)))
					contractAddr := "0x" + fmt.Sprintf("%x", h[:20])
					runtimeCode, err := CallContract(codeBytes, nil, make(map[string]*big.Int))
					if err != nil || len(runtimeCode) == 0 {
						runtimeCode = codeBytes
					}
					deploys[sid] = append(deploys[sid], deploy{contractAddr, runtimeCode})
					acc.Nonce++
					acc.bal().Sub(acc.bal(), txFeeWei)
					fees[sid] += txGas
					st.Status, st.GasUsed, st.Contract = 1, txGas, contractAddr
					continue
				}
				// ERC20 transfer on the built-in token at 0x10
				if (tx.To == "0x10" || tx.To == "0x0000000000000000000000000000000000000010") &&
					len(tx.Data) >= 138 && tx.Data[:10] == "0xa9059cbb" {
					toAddr := norm("0x" + tx.Data[34:74])
					amount := new(big.Int)
					amount.SetString(tx.Data[74:138], 16)
					acc.Nonce++
					acc.bal().Sub(acc.bal(), txFeeWei)
					fees[sid] += txGas
					st.GasUsed = txGas
					xmu.Lock()
					fromBal := n.erc20Bal[tx.From]
					if fromBal != nil && fromBal.Cmp(amount) >= 0 {
						fromBal.Sub(fromBal, amount)
						if n.erc20Bal[toAddr] == nil {
							n.erc20Bal[toAddr] = new(big.Int)
						}
						n.erc20Bal[toAddr].Add(n.erc20Bal[toAddr], amount)
						st.Status = 1
					} else {
						st.Status = 0 // reverted: insufficient token balance (nonce + fee consumed)
					}
					xmu.Unlock()
					continue
				}
				// native transfer
				acc.bal().Sub(acc.bal(), cost)
				fees[sid] += txGas
				if n.ShardOf(tx.To) == sid {
					toAcc, ok := sh.Accounts[tx.To]
					if !ok {
						toAcc = &Account{}
						sh.Accounts[tx.To] = toAcc
					}
					toAcc.bal().Add(toAcc.bal(), tx.Amount)
				} else {
					sh.Debts = append(sh.Debts, Debt{tx.To, new(big.Int).Set(tx.Amount)}) // cross-shard, settled next block
				}
				acc.Nonce++
				st.Status, st.GasUsed = 1, txGas
			}
		}(s)
	}
	wg.Wait()

	var sealed []*StoredTx
	var totalGas uint64
	for s := 0; s < numShards; s++ {
		for _, d := range deploys[s] {
			n.shards[n.ShardOf(d.addr)].Accounts[d.addr] = &Account{Code: d.code, Storage: make(map[string]*big.Int)}
		}
		sealed = append(sealed, results[s]...)
		totalGas += fees[s]
	}
	for i, t := range sealed {
		t.Index = i
	}
	if totalGas > 0 {
		cb := n.shards[n.ShardOf(coinbaseAddr)]
		if cb.Accounts[coinbaseAddr] == nil {
			cb.Accounts[coinbaseAddr] = &Account{}
		}
		fee := new(big.Int).Mul(new(big.Int).SetUint64(totalGas), big.NewInt(weiPerGas))
		cb.Accounts[coinbaseAddr].bal().Add(cb.Accounts[coinbaseAddr].bal(), fee)
	}

	stateRoot := n.computeStateRoot()
	txRoot, rcptRoot, gasUsed := txRoots(sealed)
	parent := n.store.LastHash()
	ts := uint64(time.Now().Unix())
	seal := keccak256(rlpEncode(sealFields(parent, stateRoot, txRoot, rcptRoot, number, gasUsed, ts)))
	nonce, mix := solvePoW(seal)
	rec := HeaderRec{Time: ts, Nonce: nonce}
	copy(rec.StateRoot[:], stateRoot)
	copy(rec.Hash[:], keccak256(rlpEncode(headerFields(parent, stateRoot, txRoot, rcptRoot, number, gasUsed, ts, mix, nonce))))
	if err := n.store.Append(rec, sealed); err != nil {
		fmt.Println("ERROR: chainstore append failed:", err)
	}
	n.blocks++
	n.stateRoot = "0x" + hex.EncodeToString(stateRoot)
	n.saveState()
}

func (n *Node) stateMaps() (map[string]*big.Int, map[string]uint64) {
	bal := make(map[string]*big.Int)
	non := make(map[string]uint64)
	for _, s := range n.shards {
		for addr, acc := range s.Accounts {
			bal[addr] = new(big.Int).Set(acc.bal())
			non[addr] = acc.Nonce
		}
	}
	return bal, non
}

func (n *Node) saveState() {
	bal, non := n.stateMaps()
	data, err := json.MarshalIndent(map[string]interface{}{
		"blocks": n.blocks, "balances": bal, "nonces": non, "erc20": n.erc20Bal,
		"decimals": nativeDecimals, "decimalsMigrationBlock": n.decimalsMigrationBlock,
	}, "", "  ")
	if err != nil {
		return
	}
	tmp := stateFile + ".tmp"
	if os.WriteFile(tmp, data, 0644) == nil {
		os.Rename(tmp, stateFile)
	}
}

// ---------------------------------------------------------------------------
// JSON-RPC
// ---------------------------------------------------------------------------

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

type rpcReq struct {
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
	ID     json.RawMessage `json:"id"`
}

func zeroBloom() string { return "0x" + strings.Repeat("0", 512) }

func (n *Node) resolveBlockTag(tag interface{}) (uint64, bool) {
	head := n.store.Count()
	if head == 0 {
		return 0, false
	}
	s, _ := tag.(string)
	switch s {
	case "", "latest", "pending", "safe", "finalized":
		return head - 1, true
	case "earliest":
		return 0, true
	}
	if f, ok := tag.(float64); ok {
		s = fmt.Sprintf("0x%x", uint64(f))
	}
	if !strings.HasPrefix(s, "0x") {
		return 0, false
	}
	bi, ok := new(big.Int).SetString(s[2:], 16)
	if !ok || !bi.IsUint64() || bi.Uint64() >= head {
		return 0, false
	}
	return bi.Uint64(), true
}

func (n *Node) txObject(t *StoredTx, blockHash string) map[string]interface{} {
	var to interface{} = t.To
	if t.To == "" || t.To == "0x" {
		to = nil
	}
	m := map[string]interface{}{
		"hash": t.Hash, "nonce": hexU(t.Nonce), "blockHash": blockHash, "blockNumber": hexU(t.Block),
		"transactionIndex": hexU(uint64(t.Index)), "from": t.From, "to": to, "value": t.Value,
		"gas": hexU(t.Gas), "gasPrice": t.GasPrice, "input": t.Input, "type": hexU(uint64(t.Type)),
		"chainId": hexU(chainID),
	}
	if t.V != "" {
		m["v"], m["r"], m["s"] = t.V, t.R, t.S
	} else {
		m["v"], m["r"], m["s"] = "0x0", "0x0", "0x0"
	}
	if t.Type == 2 {
		m["maxFeePerGas"], m["maxPriorityFeePerGas"] = t.GasPrice, "0x0"
	}
	return m
}

func (n *Node) blockHashOf(num uint64) string {
	r, _, ok := n.store.Header(num)
	if !ok {
		return ""
	}
	return h32(r.Hash)
}

func (n *Node) blockObject(num uint64, full bool) interface{} {
	rec, parent, ok := n.store.Header(num)
	if !ok {
		return nil
	}
	txs := n.store.BlockTxs(num)
	txRoot, rcptRoot, gasUsed := txRoots(txs)
	seal := keccak256(rlpEncode(sealFields(parent, rec.StateRoot[:], txRoot, rcptRoot, num, gasUsed, rec.Time)))
	// Legacy back-filled headers were hashed with a zero mixHash; detect by re-hashing.
	mix := powMix(seal, rec.Nonce)
	if chk := keccak256(rlpEncode(headerFields(parent, rec.StateRoot[:], txRoot, rcptRoot, num, gasUsed, rec.Time, mix, rec.Nonce))); !bytes.Equal(chk, rec.Hash[:]) {
		mix = make([]byte, 32)
	}
	hdr := rlpEncode(headerFields(parent, rec.StateRoot[:], txRoot, rcptRoot, num, gasUsed, rec.Time, mix, rec.Nonce))
	bh := h32(rec.Hash)
	txList := []interface{}{}
	size := len(hdr)
	for _, t := range txs {
		size += len(t.Input)/2 + 110
		if full {
			txList = append(txList, n.txObject(t, bh))
		} else {
			txList = append(txList, t.Hash)
		}
	}
	return map[string]interface{}{
		"number":           hexU(num),
		"hash":             bh,
		"parentHash":       h32(parent),
		"nonce":            fmt.Sprintf("0x%016x", rec.Nonce),
		"mixHash":          "0x" + hex.EncodeToString(mix),
		"sha3Uncles":       "0x" + hex.EncodeToString(emptyUncleHash),
		"logsBloom":        zeroBloom(),
		"transactionsRoot": "0x" + hex.EncodeToString(txRoot),
		"stateRoot":        h32(rec.StateRoot),
		"receiptsRoot":     "0x" + hex.EncodeToString(rcptRoot),
		"miner":            coinbaseAddr,
		"difficulty":       hexU(powDifficulty),
		"totalDifficulty":  "0x" + new(big.Int).Mul(new(big.Int).SetUint64(num+1), big.NewInt(powDifficulty)).Text(16),
		"extraData":        "0x" + hex.EncodeToString([]byte(extraData)),
		"size":             hexU(uint64(size)),
		"gasLimit":         hexU(blockGasLimit),
		"gasUsed":          hexU(gasUsed),
		"timestamp":        hexU(rec.Time),
		"transactions":     txList,
		"uncles":           []interface{}{},
	}
}

func (n *Node) receiptObject(t *StoredTx) map[string]interface{} {
	bh := n.blockHashOf(t.Block)
	var cum uint64
	for _, o := range n.store.BlockTxs(t.Block) {
		cum += o.GasUsed
		if o.Hash == t.Hash {
			break
		}
	}
	var to, contract interface{} = t.To, nil
	if t.To == "" || t.To == "0x" {
		to = nil
	}
	if t.Contract != "" {
		contract = t.Contract
	}
	return map[string]interface{}{
		"transactionHash": t.Hash, "transactionIndex": hexU(uint64(t.Index)), "blockHash": bh, "blockNumber": hexU(t.Block),
		"from": t.From, "to": to, "cumulativeGasUsed": hexU(cum), "gasUsed": hexU(t.GasUsed),
		"effectiveGasPrice": n.effectiveGasPrice(t.Block), "contractAddress": contract, "logs": []interface{}{}, "logsBloom": zeroBloom(),
		"status": hexU(t.Status), "type": hexU(uint64(t.Type)),
	}
}

// effectiveGasPrice: 1e10 wei/gas for 18-decimal blocks; blocks before the decimals migration
// charged 1 unit (1e-8 SCDO) per gas, reported as-is ("0x1") to keep history unchanged.
func (n *Node) effectiveGasPrice(block uint64) string {
	if block < n.decimalsMigrationBlock {
		return "0x1"
	}
	return hexU(weiPerGas)
}

func (n *Node) pendingTx(hash string) *Tx {
	n.mu.Lock()
	defer n.mu.Unlock()
	for i := range n.pool {
		if n.pool[i].Hash == hash {
			t := n.pool[i]
			return &t
		}
	}
	return nil
}

func paramsList(raw json.RawMessage) []interface{} {
	var p []interface{}
	json.Unmarshal(raw, &p)
	return p
}

func pstr(p []interface{}, i int) string {
	if i < len(p) {
		if s, ok := p[i].(string); ok {
			return s
		}
	}
	return ""
}

func pbool(p []interface{}, i int) bool {
	if i < len(p) {
		b, _ := p[i].(bool)
		return b
	}
	return false
}

func (n *Node) handleRPC(req rpcReq) (interface{}, *rpcError) {
	p := paramsList(req.Params)
	switch req.Method {
	case "eth_blockNumber":
		c := n.store.Count()
		if c == 0 {
			return "0x0", nil
		}
		return hexU(c - 1), nil
	case "eth_chainId":
		return hexU(chainID), nil
	case "net_version":
		return fmt.Sprintf("%d", chainID), nil
	case "web3_clientVersion":
		return clientVersion, nil
	case "net_listening":
		return true, nil
	case "net_peerCount":
		return hexU(uint64(len(n.peers))), nil
	case "eth_protocolVersion":
		return "0x40", nil
	case "eth_syncing":
		return false, nil
	case "eth_mining":
		return true, nil
	case "eth_hashrate":
		return hexU(powDifficulty / 2), nil
	case "eth_coinbase":
		return coinbaseAddr, nil
	case "eth_accounts":
		return []string{}, nil
	case "eth_gasPrice":
		// 1e10 wei per gas; flat 21000 gas per tx -> 2.1e14 wei (0.00021 SCDO) fee.
		return hexU(weiPerGas), nil
	case "scdo_nativeCurrency":
		return map[string]interface{}{
			"symbol": "SCDO", "decimals": nativeDecimals, "gasPrice": hexU(weiPerGas), "txGas": txGas,
			"txFeeWei": txFeeWei.String(), "faucetWei": faucetWei.String(),
			"legacyDecimals": legacyDecimals, "decimalsMigrationBlock": n.decimalsMigrationBlock,
			"note": "tx values/gasPrice in blocks < decimalsMigrationBlock are in 1e-8 SCDO units; later blocks use wei (1e-18 SCDO)",
		}, nil
	case "eth_maxPriorityFeePerGas":
		return "0x0", nil
	case "eth_estimateGas":
		return "0x5208", nil
	case "eth_getBlockByNumber":
		num, ok := n.resolveBlockTag(func() interface{} {
			if len(p) > 0 {
				return p[0]
			}
			return "latest"
		}())
		if !ok {
			return nil, nil
		}
		return n.blockObject(num, pbool(p, 1)), nil
	case "eth_getBlockByHash":
		hb, err := hex.DecodeString(strings.TrimPrefix(norm(pstr(p, 0)), "0x"))
		if err != nil || len(hb) != 32 {
			return nil, nil
		}
		var h [32]byte
		copy(h[:], hb)
		num, ok := n.store.NumberByHash(h)
		if !ok {
			return nil, nil
		}
		return n.blockObject(num, pbool(p, 1)), nil
	case "eth_getBlockTransactionCountByNumber":
		num, ok := n.resolveBlockTag(func() interface{} {
			if len(p) > 0 {
				return p[0]
			}
			return "latest"
		}())
		if !ok {
			return nil, nil
		}
		return hexU(uint64(len(n.store.BlockTxs(num)))), nil
	case "eth_getBlockTransactionCountByHash":
		hb, _ := hex.DecodeString(strings.TrimPrefix(norm(pstr(p, 0)), "0x"))
		var h [32]byte
		copy(h[:], hb)
		num, ok := n.store.NumberByHash(h)
		if !ok {
			return nil, nil
		}
		return hexU(uint64(len(n.store.BlockTxs(num)))), nil
	case "eth_getTransactionByBlockNumberAndIndex":
		num, ok := n.resolveBlockTag(func() interface{} {
			if len(p) > 0 {
				return p[0]
			}
			return "latest"
		}())
		if !ok {
			return nil, nil
		}
		idx, _ := new(big.Int).SetString(strings.TrimPrefix(pstr(p, 1), "0x"), 16)
		txs := n.store.BlockTxs(num)
		if idx == nil || !idx.IsUint64() || idx.Uint64() >= uint64(len(txs)) {
			return nil, nil
		}
		return n.txObject(txs[idx.Uint64()], n.blockHashOf(num)), nil
	case "eth_getTransactionByHash":
		h := norm(pstr(p, 0))
		if t := n.store.Tx(h); t != nil {
			return n.txObject(t, n.blockHashOf(t.Block)), nil
		}
		if pt := n.pendingTx(h); pt != nil {
			st := &StoredTx{Hash: pt.Hash, From: pt.From, To: pt.To, Value: hexBig(pt.Amount), Nonce: pt.Nonce, Gas: pt.Gas,
				GasPrice: pt.GasPrice, Input: pt.Data, Type: pt.Type, V: pt.V, R: pt.R, S: pt.S}
			m := n.txObject(st, "")
			m["blockHash"], m["blockNumber"], m["transactionIndex"] = nil, nil, nil
			return m, nil
		}
		return nil, nil
	case "eth_getTransactionReceipt":
		if t := n.store.Tx(norm(pstr(p, 0))); t != nil {
			return n.receiptObject(t), nil
		}
		return nil, nil
	case "eth_getLatestTxs":
		var out []TxInfo
		for _, t := range n.store.RecentTxs(20) {
			out = append(out, TxInfo{t.Hash, t.From, t.To, t.Input, t.Block, t.Status})
		}
		return out, nil
	case "eth_getLogs":
		return []interface{}{}, nil
	case "eth_feeHistory":
		c := n.store.Count()
		oldest := uint64(0)
		if c > 10 {
			oldest = c - 10
		}
		return map[string]interface{}{
			"baseFeePerGas": []string{"0x0"}, "gasUsedRatio": []float64{0}, "oldestBlock": hexU(oldest),
		}, nil
	case "eth_getBalance", "eth_getTransactionCount", "eth_getCode":
		addr := norm(pstr(p, 0))
		n.mu.Lock()
		defer n.mu.Unlock()
		acc := n.account(addr)
		switch req.Method {
		case "eth_getBalance":
			if acc == nil {
				return "0x0", nil
			}
			return hexBig(acc.bal()), nil
		case "eth_getTransactionCount":
			var nonce uint64
			if acc != nil {
				nonce = acc.Nonce
			}
			if pstr(p, 1) == "pending" {
				for _, t := range n.pool {
					if t.From == addr && t.Nonce >= nonce {
						nonce = t.Nonce + 1
					}
				}
			}
			return hexU(nonce), nil
		default:
			if acc != nil && len(acc.Code) > 0 {
				return "0x" + hex.EncodeToString(acc.Code), nil
			}
			return "0x", nil
		}
	case "eth_call":
		var cp []map[string]interface{}
		json.Unmarshal(req.Params, &cp)
		if len(cp) == 0 {
			return nil, &rpcError{-32602, "missing call object"}
		}
		to, _ := cp[0]["to"].(string)
		to = norm(to)
		data, _ := cp[0]["data"].(string)
		if data == "" {
			data, _ = cp[0]["input"].(string)
		}
		n.mu.Lock()
		defer n.mu.Unlock()
		if to == "0x10" || to == "0x0000000000000000000000000000000000000010" {
			if len(data) >= 10 {
				switch data[2:10] {
				case "70a08231":
					if len(data) < 74 {
						return "0x", nil
					}
					bal := n.erc20Bal[norm("0x"+data[34:74])]
					if bal == nil {
						bal = new(big.Int)
					}
					return fmt.Sprintf("0x%064x", bal), nil
				case "18160ddd":
					return fmt.Sprintf("0x%064x", new(big.Int).Mul(big.NewInt(6000000), big.NewInt(1e18))), nil
				case "313ce567":
					return fmt.Sprintf("0x%064x", 18), nil
				}
			}
			return "0x", nil
		}
		acc := n.account(to)
		if acc == nil || len(acc.Code) == 0 || len(data) < 2 {
			return "0x", nil
		}
		input, _ := hex.DecodeString(data[2:])
		e := NewEVM(acc.Code, input, acc.Storage)
		e.SetAddress(to)
		e.SetCallback(func(addr string, callInput []byte, delegate bool) ([]byte, error) {
			target := n.account(addr)
			if target != nil && len(target.Code) > 0 {
				if delegate {
					sub := NewEVM(target.Code, callInput, acc.Storage)
					sub.SetAddress(to)
					return sub.Run()
				}
				return CallContract(target.Code, callInput, target.Storage)
			}
			return []byte{}, nil
		})
		res, err := e.Run()
		if err != nil {
			return "0x", nil
		}
		return "0x" + hex.EncodeToString(res), nil
	case "eth_sendRawTransaction":
		rawHex := pstr(p, 0)
		if rawHex == "" {
			// The unsigned JSON-object form ({from,to,amount,...}) was removed in 0.4: it allowed spending from any account.
			return nil, &rpcError{-32602, "expected signed raw transaction hex (unsigned JSON transactions are not accepted)"}
		}
		pt, err := ParseSignedTx(rawHex)
		if err != nil {
			return nil, &rpcError{-32000, err.Error()}
		}
		if pt.From == "" {
			return nil, &rpcError{-32000, "transaction is not signed"}
		}
		if pt.ChainID == nil || pt.ChainID.Uint64() != chainID {
			return nil, &rpcError{-32000, fmt.Sprintf("chain id required (EIP-155), expected %d", chainID)}
		}
		if pt.Value.Sign() < 0 || pt.Value.Cmp(maxUint256) > 0 {
			return nil, &rpcError{-32000, "value out of range (uint256)"}
		}
		if pt.Gas < txGas {
			return nil, &rpcError{-32000, "intrinsic gas too low (need 21000)"}
		}
		if pt.GasPrice == nil || pt.GasPrice.Cmp(big.NewInt(weiPerGas)) < 0 {
			return nil, &rpcError{-32000, fmt.Sprintf("transaction underpriced: gasPrice %s < %d wei (shard0 native decimals = 18)", pt.GasPrice, weiPerGas)}
		}
		tx := Tx{From: norm(pt.From), To: norm(pt.To), Nonce: pt.Nonce, Amount: new(big.Int).Set(pt.Value), Data: "0x" + hex.EncodeToString(pt.Data),
			Hash: pt.Hash, Gas: pt.Gas, Type: pt.Type, GasPrice: "0x" + pt.GasPrice.Text(16), ChainID: pt.ChainID.Uint64()}
		tx.V, tx.R, tx.S = "0x"+pt.V.Text(16), "0x"+pt.R.Text(16), "0x"+pt.S.Text(16)
		if n.store.Tx(tx.Hash) != nil {
			return nil, &rpcError{-32000, "already known"}
		}
		n.mu.Lock()
		defer n.mu.Unlock()
		bal := new(big.Int)
		var want uint64
		if acc := n.account(tx.From); acc != nil {
			bal, want = new(big.Int).Set(acc.bal()), acc.Nonce
		}
		pendingCost := new(big.Int)
		for _, q := range n.pool {
			if q.Hash == tx.Hash {
				return nil, &rpcError{-32000, "already known"}
			}
			if q.From == tx.From {
				want = q.Nonce + 1
				pendingCost.Add(pendingCost, q.Amount)
				pendingCost.Add(pendingCost, txFeeWei)
			}
		}
		if tx.Nonce < want {
			return nil, &rpcError{-32000, fmt.Sprintf("nonce too low: next nonce %d, tx nonce %d", want, tx.Nonce)}
		}
		if tx.Nonce > want {
			return nil, &rpcError{-32000, fmt.Sprintf("nonce too high: next nonce %d, tx nonce %d", want, tx.Nonce)}
		}
		need := new(big.Int).Add(tx.Amount, txFeeWei)
		need.Add(need, pendingCost)
		if bal.Cmp(need) < 0 {
			return nil, &rpcError{-32000, fmt.Sprintf("insufficient funds: balance %s wei, need %s wei (value + %s wei fee)", bal, need, txFeeWei)}
		}
		n.pool = append(n.pool, tx)
		return tx.Hash, nil
	}
	return nil, &rpcError{-32601, "method not found: " + req.Method}
}

func (n *Node) serveRPC(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Type", "application/json")
	body, _ := readAll(r)
	one := func(req rpcReq) map[string]interface{} {
		id := req.ID
		if len(id) == 0 {
			id = json.RawMessage("null")
		}
		res, err := n.handleRPC(req)
		if err != nil {
			return map[string]interface{}{"jsonrpc": "2.0", "error": err, "id": id}
		}
		return map[string]interface{}{"jsonrpc": "2.0", "result": res, "id": id}
	}
	trim := bytes.TrimSpace(body)
	if len(trim) > 0 && trim[0] == '[' {
		var reqs []rpcReq
		if err := json.Unmarshal(trim, &reqs); err != nil {
			json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "error": rpcError{-32700, "parse error"}, "id": nil})
			return
		}
		out := make([]interface{}, 0, len(reqs))
		for _, q := range reqs {
			out = append(out, one(q))
		}
		json.NewEncoder(w).Encode(out)
		return
	}
	var req rpcReq
	if err := json.Unmarshal(trim, &req); err != nil {
		json.NewEncoder(w).Encode(map[string]interface{}{"jsonrpc": "2.0", "error": rpcError{-32700, "parse error"}, "id": nil})
		return
	}
	json.NewEncoder(w).Encode(one(req))
}

func readAll(r *http.Request) ([]byte, error) {
	var b bytes.Buffer
	_, err := b.ReadFrom(http.MaxBytesReader(nil, r.Body, 4<<20))
	return b.Bytes(), err
}

func (n *Node) stateJSON(w http.ResponseWriter) {
	n.mu.Lock()
	defer n.mu.Unlock()
	bal, non := n.stateMaps()
	json.NewEncoder(w).Encode(map[string]interface{}{"blocks": n.blocks, "balances": bal, "nonces": non, "erc20": n.erc20Bal,
		"decimals": nativeDecimals, "decimalsMigrationBlock": n.decimalsMigrationBlock})
}

func main() {
	node := NewNode()
	testContract := func() {
		node.shards[0].Accounts["0xtestcontract"] = &Account{
			Code:    []byte{0x60, 0x42, 0x60, 0x00, 0x52, 0x60, 0x20, 0x60, 0x00, 0xf3},
			Storage: make(map[string]*big.Int),
		}
		node.shards[0].Accounts["0xcounter"] = &Account{
			Code:    []byte{0x60, 0x2a, 0x60, 0x00, 0x55, 0x60, 0x00, 0x54, 0x60, 0x00, 0x52, 0x60, 0x20, 0x60, 0x00, 0xf3},
			Storage: make(map[string]*big.Int),
		}
	}
	if data, err := os.ReadFile(stateFile); err == nil {
		var saved struct {
			Blocks                 uint64
			Balances               map[string]*big.Int
			Nonces                 map[string]uint64
			Erc20                  map[string]*big.Int
			Decimals               int
			DecimalsMigrationBlock uint64
		}
		if err := json.Unmarshal(data, &saved); err != nil {
			fmt.Println("FATAL: cannot parse state.json:", err)
			os.Exit(1)
		}
		node.blocks = saved.Blocks
		// One-off decimals migration: state files without "decimals" were written by <= 0.4 with
		// 8-decimal balances (1 SCDO = 1e8 units). Scale every balance by 1e10 to wei (18 decimals).
		scale := big.NewInt(1)
		switch saved.Decimals {
		case 0, legacyDecimals:
			scale = legacyScale
			node.decimalsMigrationBlock = saved.Blocks
			fmt.Printf("MIGRATION: state.json has %d-decimal balances; multiplying %d balances by %s (-> %d decimals), effective from block %d\n",
				legacyDecimals, len(saved.Balances), legacyScale, nativeDecimals, saved.Blocks)
		case nativeDecimals:
			node.decimalsMigrationBlock = saved.DecimalsMigrationBlock
		default:
			fmt.Println("FATAL: unsupported state.json decimals:", saved.Decimals)
			os.Exit(1)
		}
		// Merge mixed-case duplicate keys into lowercase (addresses are case-insensitive).
		for addr, bal := range saved.Balances {
			a := norm(addr)
			sid := node.ShardOf(a)
			acc := node.shards[sid].Accounts[a]
			if acc == nil {
				acc = &Account{}
				node.shards[sid].Accounts[a] = acc
			}
			if bal != nil {
				if bal.Sign() < 0 {
					fmt.Println("FATAL: negative balance in state.json for", addr)
					os.Exit(1)
				}
				acc.bal().Add(acc.bal(), new(big.Int).Mul(bal, scale))
			}
			if nn := saved.Nonces[addr]; nn > acc.Nonce {
				acc.Nonce = nn
			}
		}
		for addr, bal := range saved.Erc20 {
			a := norm(addr)
			if node.erc20Bal[a] == nil {
				node.erc20Bal[a] = new(big.Int)
			}
			if bal != nil {
				node.erc20Bal[a].Add(node.erc20Bal[a], bal)
			}
		}
		fmt.Printf("Restored state: block %d, %d accounts\n", node.blocks, len(saved.Balances))
		testContract()
	} else {
		genesis := []string{"0xaaaa", "0xbbbb", "0xcccc", "0xdddd",
			"0x70997970c51812dc3a010c7d01b50e0d17dc79c8",
			"0x3c44cdddb6a900fa2b585dd299e03d12fa4293bc"}
		for i, addr := range genesis {
			node.AddFunded(addr, genesisWei)
			node.erc20Bal[addr] = new(big.Int).Mul(big.NewInt(1000000), big.NewInt(1e18))
			fmt.Printf("Genesis %d: %s\n", i, addr)
		}
		testContract()
	}
	store, err := OpenChainStore(dataDir, node.blocks, uint64(time.Now().Unix()), genesisTime)
	if err != nil {
		fmt.Println("FATAL: chainstore:", err)
		os.Exit(1)
	}
	node.store = store
	node.stateRoot = "0x" + hex.EncodeToString(node.computeStateRoot())
	fmt.Println("Contracts: 0xtestcontract, 0xcounter; coinbase", coinbaseAddr)

	go func() {
		ticker := time.NewTicker(blockTime)
		for range ticker.C {
			node.MineBlock()
		}
	}()

	http.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/state" {
			w.Header().Set("Access-Control-Allow-Origin", "*")
			node.stateJSON(w)
			return
		}
		node.serveRPC(w, r)
	})

	http.HandleFunc("/blocks", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		type blkInfo struct {
			Number  uint64 `json:"number"`
			TxCount int    `json:"txCount"`
			Hash    string `json:"hash"`
			Time    uint64 `json:"timestamp"`
		}
		var blks []blkInfo
		c := node.store.Count()
		for i := uint64(0); i < 10 && i < c; i++ {
			bn := c - 1 - i
			rec, _, _ := node.store.Header(bn)
			blks = append(blks, blkInfo{bn, len(node.store.BlockTxs(bn)), h32(rec.Hash), rec.Time})
		}
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
<p style="margin:4px 0">RPC: <span class="addr">https://scdoscan.io/rpc/0</span></p>
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
function fmt18(h){const u=BigInt(h||'0x0'),d=10n**18n;let f=(u%d).toString().padStart(18,'0').replace(/0+$/,'');return (u/d).toString()+(f?'.'+f:'');}
async function h(){document.getElementById('h').textContent=parseInt(await rpc('eth_blockNumber'),16)}
async function look(){const a=document.getElementById('addr').value;const b=await rpc('eth_getBalance',[a,'latest']);document.getElementById('bal').textContent='Native: '+fmt18(b)+' SCDO';const d='0x70a08231'+'0'.repeat(24)+a.slice(2);const t=await rpc('eth_call',[{to:'0x10',data:d},'latest']);document.getElementById('tbal').textContent='ERC20: '+(parseInt(t,16)/1e18).toFixed(2)}
async function send(){const from=document.getElementById('addr').value;const to=document.getElementById('to').value;const amt=parseFloat(document.getElementById('amt').value);const data='0xa9059cbb'+'0'.repeat(24)+to.slice(2)+Math.floor(amt*1e18).toString(16).padStart(64,'0');const hash=await rpc('eth_sendRawTransaction',[{from:from,to:'0x10',amount:0,nonce:0,data:data}]);document.getElementById('txres').textContent='TX: '+hash;setTimeout(look,3000)}
async function faucet(){const a=document.getElementById('addr').value;const r=await fetch('/faucet?addr='+a);const j=await r.json();document.getElementById('faucetres').textContent='Got: '+fmt18('0x'+BigInt(j.nativeAdded||0).toString(16))+' SCDO (balance '+fmt18('0x'+BigInt(j.native||0).toString(16))+'), '+(parseFloat(j.erc20)/1e18).toFixed(2)+' ERC20';setTimeout(look,1000)}
async function txlist(){const txs=await rpc('eth_getLatestTxs',[]);let html='';for(const t of (txs||[]).reverse()){html+=t.Status+' '+t.From.slice(0,10)+'->'+t.To.slice(0,10)+' blk'+t.Block+'<br>'}document.getElementById('txlist').innerHTML=html||'none'}
async function connect(){
if(typeof window.ethereum==='undefined'){alert('Please install MetaMask');return}
try{
await window.ethereum.request({method:'wallet_addEthereumChain',params:[{
chainId:'0x238',chainName:'SCDO Parallel',nativeCurrency:{name:'SCDO',symbol:'SCDO',decimals:18},
rpcUrls:['https://scdoscan.io/rpc/0'],blockExplorerUrls:['https://scdoscan.io']
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

	http.HandleFunc("/state", func(w http.ResponseWriter, r *http.Request) { node.stateJSON(w) })
	http.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		c := node.store.Count()
		rec, _, _ := node.store.Header(c - 1)
		node.mu.Lock()
		sr := node.stateRoot
		node.mu.Unlock()
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "block": hexU(c - 1), "hash": h32(rec.Hash),
			"timestamp": rec.Time, "shards": numShards, "stateRoot": sr, "peers": len(node.peers), "version": clientVersion})
	})
	http.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		fmt.Fprintf(w, "scdo_block_height %d\n", node.store.Count())
		fmt.Fprintf(w, "scdo_shards %d\n", numShards)
	})
	faucet := loadFaucetLimits(dataDir + "/faucet.json")
	http.HandleFunc("/faucet", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		addr := norm(r.URL.Query().Get("addr"))
		if len(addr) != 42 || !strings.HasPrefix(addr, "0x") {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid addr (expected 0x + 40 hex chars)"})
			return
		}
		if _, err := hex.DecodeString(addr[2:]); err != nil {
			w.WriteHeader(400)
			json.NewEncoder(w).Encode(map[string]string{"error": "invalid addr"})
			return
		}
		ip := clientIP(r)
		if wait := faucet.check(addr, ip); wait > 0 {
			w.WriteHeader(429)
			json.NewEncoder(w).Encode(map[string]interface{}{"error": "rate limited: faucet allows 1 claim per address per 24h and per IP per hour", "retryAfterSec": int(wait.Seconds())})
			return
		}
		node.mu.Lock()
		defer node.mu.Unlock()
		sid := node.ShardOf(addr)
		acc, ok := node.shards[sid].Accounts[addr]
		if !ok {
			acc = &Account{}
			node.shards[sid].Accounts[addr] = acc
		}
		acc.bal().Add(acc.bal(), faucetWei)
		if node.erc20Bal[addr] == nil {
			node.erc20Bal[addr] = big.NewInt(0)
		}
		node.erc20Bal[addr].Add(node.erc20Bal[addr], new(big.Int).Mul(big.NewInt(1000), big.NewInt(1e18)))
		faucet.record(addr, ip)
		json.NewEncoder(w).Encode(map[string]interface{}{"status": "ok", "addr": addr, "native": acc.bal().String(), "nativeAdded": faucetWei.String(),
			"decimals": nativeDecimals, "erc20": node.erc20Bal[addr].String()})
	})
	fmt.Println(clientVersion + " on :8050")
	go node.syncPeers()
	http.ListenAndServe(":8050", nil)
}
