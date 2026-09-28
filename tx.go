package main

// Signed transaction parsing + sender recovery (added 2026-09-28).
// Supports legacy (pre-EIP-155 and EIP-155), EIP-2930 (type 1) and EIP-1559 (type 2).

import (
	"encoding/hex"
	"fmt"
	"math/big"

	"github.com/decred/dcrd/dcrec/secp256k1/v4/ecdsa"
)

type ParsedTx struct {
	Type     byte
	ChainID  *big.Int
	Nonce    uint64
	GasPrice *big.Int
	Gas      uint64
	To       string // "" or "0x" for contract creation
	Value    *big.Int
	Data     []byte
	V, R, S  *big.Int
	From     string // "" when unsigned / not recoverable
	Hash     string
}

func rlpItemBytes(list []interface{}, i int) []byte {
	if i >= len(list) {
		return nil
	}
	b, _ := list[i].([]byte)
	return b
}

func rlpItemBig(list []interface{}, i int) *big.Int {
	return new(big.Int).SetBytes(rlpItemBytes(list, i))
}

func recoverAddr(sighash []byte, recid uint64, r, s *big.Int) (string, error) {
	if recid > 1 {
		return "", fmt.Errorf("invalid recovery id %d", recid)
	}
	sig := make([]byte, 65)
	sig[0] = byte(27 + recid)
	r.FillBytes(sig[1:33])
	s.FillBytes(sig[33:65])
	pub, _, err := ecdsa.RecoverCompact(sig, sighash)
	if err != nil {
		return "", err
	}
	h := keccak256(pub.SerializeUncompressed()[1:])
	return "0x" + hex.EncodeToString(h[12:]), nil
}

// ParseSignedTx decodes a raw (hex) transaction and recovers its sender.
func ParseSignedTx(rawHex string) (*ParsedTx, error) {
	if len(rawHex) < 4 || rawHex[:2] != "0x" {
		return nil, fmt.Errorf("invalid hex")
	}
	raw, err := hex.DecodeString(rawHex[2:])
	if err != nil || len(raw) == 0 {
		return nil, fmt.Errorf("invalid hex: %v", err)
	}
	p := &ParsedTx{Hash: "0x" + hex.EncodeToString(keccak256(raw))}
	payload := raw
	if raw[0] < 0x7f {
		p.Type = raw[0]
		payload = raw[1:]
		if p.Type != 1 && p.Type != 2 {
			return nil, fmt.Errorf("unsupported tx type %d", p.Type)
		}
	}
	dec, _ := decodeRLP(payload)
	list, ok := dec.([]interface{})
	if !ok {
		return nil, fmt.Errorf("tx is not an RLP list")
	}
	var sighash []byte
	var recid uint64
	toIdx := 3
	switch p.Type {
	case 0:
		if len(list) < 6 {
			return nil, fmt.Errorf("legacy tx too short: %d", len(list))
		}
		p.Nonce = rlpItemBig(list, 0).Uint64()
		p.GasPrice = rlpItemBig(list, 1)
		p.Gas = rlpItemBig(list, 2).Uint64()
		toIdx = 3
		p.Value = rlpItemBig(list, 4)
		p.Data = rlpItemBytes(list, 5)
		if len(list) >= 9 {
			p.V, p.R, p.S = rlpItemBig(list, 6), rlpItemBig(list, 7), rlpItemBig(list, 8)
			v := p.V.Uint64()
			if v >= 35 {
				p.ChainID = new(big.Int).SetUint64((v - 35) / 2)
				recid = (v - 35) % 2
				sighash = keccak256(rlpEncode([]interface{}{list[0], list[1], list[2], list[3], list[4], list[5], p.ChainID, uint64(0), uint64(0)}))
			} else if v == 27 || v == 28 {
				recid = v - 27
				sighash = keccak256(rlpEncode(list[:6]))
			} else {
				return nil, fmt.Errorf("invalid v %d", v)
			}
		}
	case 1, 2:
		need := 11
		if p.Type == 2 {
			need = 12
		}
		if len(list) < need {
			return nil, fmt.Errorf("typed tx too short: %d", len(list))
		}
		p.ChainID = rlpItemBig(list, 0)
		p.Nonce = rlpItemBig(list, 1).Uint64()
		off := 0
		if p.Type == 2 {
			off = 1
			p.GasPrice = rlpItemBig(list, 3) // maxFeePerGas
		} else {
			p.GasPrice = rlpItemBig(list, 2)
		}
		p.Gas = rlpItemBig(list, 3+off).Uint64()
		toIdx = 4 + off
		p.Value = rlpItemBig(list, 5+off)
		p.Data = rlpItemBytes(list, 6+off)
		p.V, p.R, p.S = rlpItemBig(list, 8+off), rlpItemBig(list, 9+off), rlpItemBig(list, 10+off)
		recid = p.V.Uint64()
		sighash = keccak256(append([]byte{p.Type}, rlpEncode(list[:8+off])...))
	}
	if tb := rlpItemBytes(list, toIdx); len(tb) > 0 {
		p.To = "0x" + hex.EncodeToString(tb)
	} else {
		p.To = "0x"
	}
	if p.ChainID != nil && p.ChainID.Sign() != 0 && p.ChainID.Uint64() != chainID {
		return nil, fmt.Errorf("invalid chain id %s (expected %d)", p.ChainID, chainID)
	}
	if sighash != nil {
		from, err := recoverAddr(sighash, recid, p.R, p.S)
		if err != nil {
			return nil, fmt.Errorf("invalid signature: %v", err)
		}
		p.From = from
	}
	return p, nil
}
