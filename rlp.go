package main

import (
	"encoding/hex"
	"fmt"
	"math/big"
)

func decodeRLP(data []byte) (interface{}, int) {
	if len(data) == 0 {
		return nil, 0
	}
	b := data[0]
	switch {
	case b < 0x80:
		return []byte{b}, 1
	case b <= 0xb7:
		l := int(b - 0x80)
		if 1+l > len(data) {
			return nil, len(data)
		}
		return data[1 : 1+l], 1 + l
	case b <= 0xbf:
		l := int(b - 0xb7)
		if 1+l > len(data) {
			return nil, len(data)
		}
		ll := int(new(big.Int).SetBytes(data[1 : 1+l]).Uint64())
		start := 1 + l
		if start+ll > len(data) {
			return nil, len(data)
		}
		return data[start : start+ll], start + ll
	case b <= 0xf7:
		l := int(b - 0xc0)
		if 1+l > len(data) {
			return nil, len(data)
		}
		items := []interface{}{}
		pos := 1
		end := 1 + l
		for pos < end {
			item, n := decodeRLP(data[pos:end])
			items = append(items, item)
			pos += n
		}
		return items, end
	default:
		l := int(b - 0xf7)
		if 1+l > len(data) {
			return nil, len(data)
		}
		ll := int(new(big.Int).SetBytes(data[1 : 1+l]).Uint64())
		start := 1 + l
		end := start + ll
		if end > len(data) {
			return nil, len(data)
		}
		items := []interface{}{}
		pos := start
		for pos < end {
			item, n := decodeRLP(data[pos:end])
			items = append(items, item)
			pos += n
		}
		return items, end
	}
}

func ParseRawTx(rawHex string) (from, to string, nonce uint64, value uint64, data []byte, err error) {
	rawHex = rawHex[2:]
	raw, err := hex.DecodeString(rawHex)
	if err != nil {
		return "", "", 0, 0, nil, err
	}
	decoded, _ := decodeRLP(raw)
	list, ok := decoded.([]interface{})
	if !ok {
		return "", "", 0, 0, nil, fmt.Errorf("not a list")
	}
	if len(list) < 6 {
		return "", "", 0, 0, nil, fmt.Errorf("too short: %d", len(list))
	}
	nonce = new(big.Int).SetBytes(list[0].([]byte)).Uint64()
	toBytes, _ := list[3].([]byte)
	if len(toBytes) == 0 {
		to = "0x"
	} else {
		to = "0x" + hex.EncodeToString(toBytes)
	}
	value = new(big.Int).SetBytes(list[4].([]byte)).Uint64()
	data, _ = list[5].([]byte)
	return "", to, nonce, value, data, nil
}

// ---------------------------------------------------------------------------
// RLP encoder (added 2026-09-28) — used for header hashing and tx sighash.
// ---------------------------------------------------------------------------

func rlpLenPrefix(l int, off byte) []byte {
	if l < 56 {
		return []byte{off + byte(l)}
	}
	lb := new(big.Int).SetInt64(int64(l)).Bytes()
	return append([]byte{off + 55 + byte(len(lb))}, lb...)
}

func rlpEncode(v interface{}) []byte {
	switch x := v.(type) {
	case nil:
		return []byte{0x80}
	case []byte:
		if len(x) == 1 && x[0] < 0x80 {
			return []byte{x[0]}
		}
		return append(rlpLenPrefix(len(x), 0x80), x...)
	case string:
		return rlpEncode([]byte(x))
	case uint64:
		if x == 0 {
			return []byte{0x80}
		}
		return rlpEncode(new(big.Int).SetUint64(x).Bytes())
	case *big.Int:
		if x == nil || x.Sign() == 0 {
			return []byte{0x80}
		}
		return rlpEncode(x.Bytes())
	case []interface{}:
		var body []byte
		for _, it := range x {
			body = append(body, rlpEncode(it)...)
		}
		return append(rlpLenPrefix(len(body), 0xc0), body...)
	}
	panic(fmt.Sprintf("rlpEncode: unsupported type %T", v))
}
