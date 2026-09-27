package main

import (
	"encoding/hex"
	"fmt"
	"math/big"
)

// Minimal RLP decoder for legacy transactions
type rlpList []interface{}

func decodeRLP(data []byte) (interface{}, int) {
	if len(data) == 0 { return nil, 0 }
	b := data[0]
	switch {
	case b < 0x80:
		return []byte{b}, 1
	case b < 0xb8:
		l := int(b - 0x80)
		return data[1 : 1+l], 1 + l
	case b < 0xc0:
		l := int(b-0xb7)
		ll := int(data[1])
		start := 2 + l
		return data[start : start+ll], start + ll
	case b < 0xf8:
		l := int(b - 0xc0)
		items := []interface{}{}
		pos := 1
		end := 1 + l
		for pos < end {
			item, n := decodeRLP(data[pos:])
			items = append(items, item)
			pos += n
		}
		return items, 1 + l
	default:
		l := int(b-0xf7)
		ll := int(new(big.Int).SetBytes(data[1 : 1+l]).Int64())
		start := 1 + l
		items := []interface{}{}
		pos := start
		end := start + ll
		for pos < end {
			item, n := decodeRLP(data[pos:])
			items = append(items, item)
			pos += n
		}
		return items, end
	}
}

func ParseRawTx(rawHex string) (from, to string, nonce uint64, data []byte, err error) {
	rawHex = rawHex[2:] // strip 0x
	raw, err := hex.DecodeString(rawHex)
	if err != nil { return "", "", 0, nil, err }
	decoded, _ := decodeRLP(raw)
	list, ok := decoded.([]interface{})
	if !ok { return "", "", 0, nil, fmt.Errorf("not a list") }
	// Legacy: [nonce, gasPrice, gasLimit, to, value, data, v, r, s]
	if len(list) < 6 { return "", "", 0, nil, fmt.Errorf("too short") }
	nonce = new(big.Int).SetBytes(list[0].([]byte)).Uint64()
	toBytes, _ := list[3].([]byte)
	if len(toBytes) == 0 {
		to = "0x" // contract creation
	} else {
		to = "0x" + hex.EncodeToString(toBytes)
	}
	data, _ = list[5].([]byte)
	return "0xunknown", to, nonce, data, nil
}
