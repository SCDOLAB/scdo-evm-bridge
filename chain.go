package main

// Persistent block header + transaction store (added 2026-09-28).
//
// headers.dat : fixed 80-byte records, record i = block i:
//               time(8) | powNonce(8) | hash(32) | stateRoot(32)
//               parentHash(i) = hash(i-1). Blocks mined before this upgrade were
//               back-filled once with best-effort values (time extrapolated at 2s
//               per block back from the upgrade moment, nonce 0, stateRoot 0).
// txs.jsonl   : one JSON StoredTx per line, appended when its block is sealed.

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sync"
)

const (
	recSize       = 80
	hashIndexKeep = 50000
)

var (
	emptyRoot, _      = hex.DecodeString("56e81f171bcc55a6ff8345e692c0f86e5b48e01b996cadc001622fb5e363b421")
	emptyUncleHash, _ = hex.DecodeString("1dcc4de8dec75d7aab85b567b6ccd41ad312451b948a7413f0a142fd40d49347")
)

type HeaderRec struct {
	Time      uint64
	Nonce     uint64
	Hash      [32]byte
	StateRoot [32]byte
}

func (h *HeaderRec) marshal() []byte {
	b := make([]byte, recSize)
	binary.BigEndian.PutUint64(b[0:8], h.Time)
	binary.BigEndian.PutUint64(b[8:16], h.Nonce)
	copy(b[16:48], h.Hash[:])
	copy(b[48:80], h.StateRoot[:])
	return b
}

func unmarshalRec(b []byte) HeaderRec {
	var h HeaderRec
	h.Time = binary.BigEndian.Uint64(b[0:8])
	h.Nonce = binary.BigEndian.Uint64(b[8:16])
	copy(h.Hash[:], b[16:48])
	copy(h.StateRoot[:], b[48:80])
	return h
}

type StoredTx struct {
	Hash     string `json:"hash"`
	Block    uint64 `json:"block"`
	Index    int    `json:"index"`
	From     string `json:"from"`
	To       string `json:"to"`
	Value    string `json:"value"` // hex quantity
	Nonce    uint64 `json:"nonce"`
	Gas      uint64 `json:"gas"`
	GasPrice string `json:"gasPrice"` // hex quantity as signed by sender
	Input    string `json:"input"`
	Type     uint8  `json:"type"`
	ChainID  uint64 `json:"chainId,omitempty"`
	V        string `json:"v,omitempty"`
	R        string `json:"r,omitempty"`
	S        string `json:"s,omitempty"`
	Status   uint64 `json:"status"`
	GasUsed  uint64 `json:"gasUsed"`
	Contract string `json:"contract,omitempty"`
}

type ChainStore struct {
	mu        sync.RWMutex
	hf        *os.File
	tf        *os.File
	count     uint64
	last      HeaderRec
	hashIdx   map[[32]byte]uint64
	txs       map[string]*StoredTx
	blockTxs  map[uint64][]*StoredTx
	recentTxs []*StoredTx
}

func OpenChainStore(dir string, height uint64, now uint64, genesisTime uint64) (*ChainStore, error) {
	hf, err := os.OpenFile(dir+"/headers.dat", os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return nil, err
	}
	st, _ := hf.Stat()
	cs := &ChainStore{hf: hf, hashIdx: map[[32]byte]uint64{}, txs: map[string]*StoredTx{}, blockTxs: map[uint64][]*StoredTx{}}
	cs.count = uint64(st.Size()) / recSize
	if uint64(st.Size())%recSize != 0 {
		hf.Truncate(int64(cs.count * recSize))
	}
	if cs.count > height {
		// Headers ahead of saved state (crash between header append and state save): roll back.
		fmt.Printf("chainstore: truncating headers %d -> %d to match state\n", cs.count, height)
		hf.Truncate(int64(height * recSize))
		cs.count = height
	}
	if cs.count > 0 {
		cs.last = cs.readRecLocked(cs.count - 1)
	}
	if cs.count < height {
		// Back-fill legacy blocks (mined before headers were persisted) with best-effort values.
		fmt.Printf("chainstore: back-filling legacy headers %d..%d\n", cs.count, height-1)
		w := bufio.NewWriterSize(&fileAppender{hf, int64(cs.count * recSize)}, 1<<20)
		for i := cs.count; i < height; i++ {
			t := genesisTime
			if now > (height-i)*2 {
				t = now - (height-i)*2
			}
			if t < genesisTime {
				t = genesisTime
			}
			var parent [32]byte
			if i > 0 {
				parent = cs.last.Hash
			}
			h := HeaderRec{Time: t}
			fields := headerFields(parent, h.StateRoot[:], emptyRoot, emptyRoot, i, 0, t, make([]byte, 32), 0)
			copy(h.Hash[:], keccak256(rlpEncode(fields)))
			w.Write(h.marshal())
			cs.last = h
		}
		w.Flush()
		cs.count = height
	}
	// recent hash index
	start := uint64(0)
	if cs.count > hashIndexKeep {
		start = cs.count - hashIndexKeep
	}
	buf := make([]byte, (cs.count-start)*recSize)
	hf.ReadAt(buf, int64(start*recSize))
	for i := start; i < cs.count; i++ {
		r := unmarshalRec(buf[(i-start)*recSize : (i-start+1)*recSize])
		cs.hashIdx[r.Hash] = i
	}
	// transactions
	tf, err := os.OpenFile(dir+"/txs.jsonl", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
	if err != nil {
		return nil, err
	}
	cs.tf = tf
	data, _ := os.ReadFile(dir + "/txs.jsonl")
	orphans := false
	var keep [][]byte
	for _, line := range bytes.Split(data, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var t StoredTx
		if json.Unmarshal(line, &t) != nil {
			orphans = true
			continue
		}
		if t.Block >= cs.count {
			orphans = true
			continue
		}
		keep = append(keep, line)
		cs.indexTxLocked(&t)
	}
	if orphans {
		os.WriteFile(dir+"/txs.jsonl.tmp", append(bytes.Join(keep, []byte("\n")), '\n'), 0644)
		os.Rename(dir+"/txs.jsonl.tmp", dir+"/txs.jsonl")
		tf.Close()
		cs.tf, _ = os.OpenFile(dir+"/txs.jsonl", os.O_RDWR|os.O_CREATE|os.O_APPEND, 0644)
	}
	for num := range cs.blockTxs {
		if r, ok := cs.readRec(num); ok {
			cs.hashIdx[r.Hash] = num
		}
	}
	fmt.Printf("chainstore: %d headers, %d txs loaded\n", cs.count, len(cs.txs))
	return cs, nil
}

type fileAppender struct {
	f   *os.File
	off int64
}

func (a *fileAppender) Write(p []byte) (int, error) {
	n, err := a.f.WriteAt(p, a.off)
	a.off += int64(n)
	return n, err
}

func (cs *ChainStore) indexTxLocked(t *StoredTx) {
	cs.txs[t.Hash] = t
	cs.blockTxs[t.Block] = append(cs.blockTxs[t.Block], t)
	cs.recentTxs = append(cs.recentTxs, t)
	if len(cs.recentTxs) > 2000 {
		cs.recentTxs = cs.recentTxs[len(cs.recentTxs)-1000:]
	}
}

func (cs *ChainStore) readRecLocked(n uint64) HeaderRec {
	b := make([]byte, recSize)
	cs.hf.ReadAt(b, int64(n*recSize))
	return unmarshalRec(b)
}

func (cs *ChainStore) readRec(n uint64) (HeaderRec, bool) {
	if n >= cs.count {
		return HeaderRec{}, false
	}
	if n == cs.count-1 {
		return cs.last, true
	}
	return cs.readRecLocked(n), true
}

func (cs *ChainStore) Count() uint64 {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.count
}

func (cs *ChainStore) LastHash() [32]byte {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if cs.count == 0 {
		return [32]byte{}
	}
	return cs.last.Hash
}

// Append seals block cs.count: txs first, then header (crash-safe ordering).
func (cs *ChainStore) Append(h HeaderRec, txs []*StoredTx) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	num := cs.count
	if len(txs) > 0 {
		var buf bytes.Buffer
		for _, t := range txs {
			t.Block = num
			b, _ := json.Marshal(t)
			buf.Write(b)
			buf.WriteByte('\n')
		}
		if _, err := cs.tf.Write(buf.Bytes()); err != nil {
			return err
		}
		for _, t := range txs {
			cs.indexTxLocked(t)
		}
	}
	if _, err := cs.hf.WriteAt(h.marshal(), int64(num*recSize)); err != nil {
		return err
	}
	cs.count++
	cs.last = h
	cs.hashIdx[h.Hash] = num
	if num >= hashIndexKeep {
		old := num - hashIndexKeep
		if _, hasTx := cs.blockTxs[old]; !hasTx {
			r := cs.readRecLocked(old)
			delete(cs.hashIdx, r.Hash)
		}
	}
	return nil
}

func (cs *ChainStore) Header(n uint64) (HeaderRec, [32]byte, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	r, ok := cs.readRec(n)
	if !ok {
		return r, [32]byte{}, false
	}
	var parent [32]byte
	if n > 0 {
		parent = cs.readRecLocked(n - 1).Hash
	}
	return r, parent, true
}

func (cs *ChainStore) BlockTxs(n uint64) []*StoredTx {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return append([]*StoredTx(nil), cs.blockTxs[n]...)
}

func (cs *ChainStore) Tx(hash string) *StoredTx {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	return cs.txs[hash]
}

func (cs *ChainStore) RecentTxs(max int) []*StoredTx {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	r := cs.recentTxs
	if len(r) > max {
		r = r[len(r)-max:]
	}
	return append([]*StoredTx(nil), r...)
}

func (cs *ChainStore) NumberByHash(h [32]byte) (uint64, bool) {
	cs.mu.RLock()
	defer cs.mu.RUnlock()
	if n, ok := cs.hashIdx[h]; ok {
		return n, true
	}
	// slow path: scan older headers backwards in 1MB chunks
	const chunk = 13107 // records per chunk
	end := cs.count
	if end > hashIndexKeep {
		end -= hashIndexKeep
	} else {
		return 0, false
	}
	buf := make([]byte, chunk*recSize)
	for end > 0 {
		start := uint64(0)
		if end > chunk {
			start = end - chunk
		}
		b := buf[:(end-start)*recSize]
		cs.hf.ReadAt(b, int64(start*recSize))
		for i := end - start; i > 0; i-- {
			if bytes.Equal(b[(i-1)*recSize+16:(i-1)*recSize+48], h[:]) {
				return start + i - 1, true
			}
		}
		end = start
	}
	return 0, false
}

// headerFields returns the Ethereum-style header list used for hashing.
func headerFields(parent [32]byte, stateRoot, txRoot, rcptRoot []byte, number, gasUsed, time uint64, mix []byte, nonce uint64) []interface{} {
	f := sealFields(parent, stateRoot, txRoot, rcptRoot, number, gasUsed, time)
	nb := make([]byte, 8)
	binary.BigEndian.PutUint64(nb, nonce)
	return append(f, mix, nb)
}

func sealFields(parent [32]byte, stateRoot, txRoot, rcptRoot []byte, number, gasUsed, time uint64) []interface{} {
	return []interface{}{
		parent[:], emptyUncleHash, coinbaseBytes(), stateRoot, txRoot, rcptRoot,
		make([]byte, 256), uint64(powDifficulty), number, uint64(blockGasLimit), gasUsed, time, []byte(extraData),
	}
}

func txRoots(txs []*StoredTx) (txRoot, rcptRoot []byte, gasUsed uint64) {
	if len(txs) == 0 {
		return emptyRoot, emptyRoot, 0
	}
	var a, b []byte
	for _, t := range txs {
		hb, _ := hex.DecodeString(t.Hash[2:])
		a = append(a, hb...)
		b = append(b, hb...)
		b = append(b, byte(t.Status))
		gasUsed += t.GasUsed
	}
	return keccak256(a), keccak256(b), gasUsed
}
