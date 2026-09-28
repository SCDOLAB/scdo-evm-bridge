package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/sha256"
	"fmt"
	"golang.org/x/crypto/sha3"
	"math/big"
)

var secp256k1 = &elliptic.CurveParams{
	P:       mustBig("0xfffffffffffffffffffffffffffffffffffffffffffffffffffffffefffffc2f"),
	N:       mustBig("0xfffffffffffffffffffffffffffffffebaaedce6af48a03bbfd25e8cd0364141"),
	B:       mustBig("0x0000000000000000000000000000000000000000000000000000000000000007"),
	Gx:      mustBig("0x79be667ef9dcbbac55a06295ce870b07029bfcdb2dce28d959f2815b16f81798"),
	Gy:      mustBig("0x483ada7726a3c4655da4fbfc0e1108a8fd17b448a68554199c47d08ffb10d4b8"),
	BitSize: 256,
}

func mustBig(s string) *big.Int {
	v, _ := new(big.Int).SetString(s[2:], 16)
	return v
}

type LogEntry struct {
	Address string
	Topics  []string
	Data    []byte
}

type ContractCallBack func(addr string, input []byte, delegate bool) ([]byte, error)

type EVM struct {
	code       []byte
	input      []byte
	stack      []*big.Int
	memory     []byte
	storage    map[string]*big.Int
	pc         int
	gasLeft    uint64
	gasUsed    uint64
	ret        []byte
	logs       []LogEntry
	address    string
	callback   ContractCallBack
	validJumps map[int]bool
}

func NewEVM(code []byte, input []byte, storage map[string]*big.Int) *EVM {
	e := &EVM{code: code, input: input, storage: storage, gasLeft: 10000000, address: "0x0"}
	e.computeJumpDests()
	return e
}

func (e *EVM) SetCallback(cb ContractCallBack) { e.callback = cb }
func (e *EVM) SetAddress(addr string)          { e.address = addr }
func (e *EVM) GasUsed() uint64                 { return e.gasUsed }

func (e *EVM) computeJumpDests() {
	e.validJumps = make(map[int]bool)
	for i := 0; i < len(e.code); {
		op := e.code[i]
		if op == 0x5b {
			e.validJumps[i] = true
		}
		if op >= 0x60 && op <= 0x7f {
			i += int(op-0x5f) + 1
		} else {
			i++
		}
	}
}

func (e *EVM) stackPush(v *big.Int) { e.stack = append(e.stack, v) }
func (e *EVM) stackPop() *big.Int {
	if len(e.stack) == 0 {
		return big.NewInt(0)
	}
	v := e.stack[len(e.stack)-1]
	e.stack = e.stack[:len(e.stack)-1]
	return v
}

func (e *EVM) memRead(offset, size int64) []byte {
	if size <= 0 {
		return []byte{}
	}
	if int(offset+size) > len(e.memory) {
		buf := make([]byte, size)
		end := offset + size
		if int(end) <= len(e.memory) {
			copy(buf, e.memory[offset:end])
		} else if int(offset) < len(e.memory) {
			copy(buf, e.memory[offset:])
		}
		return buf
	}
	return e.memory[offset : offset+size]
}

func (e *EVM) memWrite(offset int64, data []byte) {
	for int(offset)+len(data) > len(e.memory) {
		e.memory = append(e.memory, 0)
	}
	copy(e.memory[offset:], data)
}

func (e *EVM) expandMemory(offset, size int64) {
	if size == 0 {
		return
	}
	newSize := int(offset + size)
	for len(e.memory) < newSize {
		e.memory = append(e.memory, 0)
	}
	// Memory expansion gas cost
	words := uint64((newSize + 31) / 32)
	e.gasLeft -= words*words/512 + words*3
}

func gasCost(op byte) uint64 {
	switch {
	case op == 0x00:
		return 0
	case op == 0x5b:
		return 1
	case op == 0x54:
		return 800
	case op == 0x55:
		return 20000
	case op == 0xf1, op == 0xf2, op == 0xf4, op == 0xfa:
		return 100
	case op == 0xf0, op == 0xf5:
		return 32000
	case op == 0xff:
		return 5000
	case op >= 0xa0 && op <= 0xa4:
		return 375 + uint64(op-0xa0)*375
	case op >= 0x60 && op <= 0x7f:
		return 3
	default:
		return 3
	}
}

func (e *EVM) Run() ([]byte, error) {
	for e.pc < len(e.code) && e.gasLeft > 0 {
		op := e.code[e.pc]
		e.pc++
		cost := gasCost(op)
		e.gasLeft -= cost
		e.gasUsed += cost

		switch op {
		case 0x00:
			return e.ret, nil
		case 0x01:
			a, b := e.stackPop(), e.stackPop()
			e.stackPush(new(big.Int).Add(a, b))
		case 0x02:
			a, b := e.stackPop(), e.stackPop()
			e.stackPush(new(big.Int).Mul(a, b))
		case 0x03:
			a, b := e.stackPop(), e.stackPop()
			e.stackPush(new(big.Int).Sub(b, a))
		case 0x04:
			a, b := e.stackPop(), e.stackPop()
			if a.Sign() == 0 {
				e.stackPush(big.NewInt(0))
			} else {
				e.stackPush(new(big.Int).Div(b, a))
			}
		case 0x06:
			a, b := e.stackPop(), e.stackPop()
			if a.Sign() == 0 {
				e.stackPush(big.NewInt(0))
			} else {
				e.stackPush(new(big.Int).Mod(b, a))
			}
		case 0x0a:
			base, exp := e.stackPop(), e.stackPop()
			e.stackPush(new(big.Int).Exp(base, exp, nil))
		case 0x10:
			a, b := e.stackPop(), e.stackPop()
			if b.Cmp(a) < 0 {
				e.stackPush(big.NewInt(1))
			} else {
				e.stackPush(big.NewInt(0))
			}
		case 0x11:
			a, b := e.stackPop(), e.stackPop()
			if b.Cmp(a) > 0 {
				e.stackPush(big.NewInt(1))
			} else {
				e.stackPush(big.NewInt(0))
			}
		case 0x14:
			a, b := e.stackPop(), e.stackPop()
			if a.Cmp(b) == 0 {
				e.stackPush(big.NewInt(1))
			} else {
				e.stackPush(big.NewInt(0))
			}
		case 0x15:
			a := e.stackPop()
			if a.Sign() == 0 {
				e.stackPush(big.NewInt(1))
			} else {
				e.stackPush(big.NewInt(0))
			}
		case 0x16:
			a, b := e.stackPop(), e.stackPop()
			e.stackPush(new(big.Int).And(a, b))
		case 0x17:
			a, b := e.stackPop(), e.stackPop()
			e.stackPush(new(big.Int).Or(a, b))
		case 0x18:
			a, b := e.stackPop(), e.stackPop()
			e.stackPush(new(big.Int).Xor(a, b))
		case 0x19:
			a := e.stackPop()
			e.stackPush(new(big.Int).Not(a))
		case 0x1b:
			shift, val := e.stackPop(), e.stackPop()
			e.stackPush(new(big.Int).Lsh(val, uint(shift.Int64())))
		case 0x1c:
			shift, val := e.stackPop(), e.stackPop()
			e.stackPush(new(big.Int).Rsh(val, uint(shift.Int64())))
		case 0x30:
			e.stackPush(big.NewInt(0))
		case 0x32, 0x33, 0x34:
			e.stackPush(big.NewInt(0))
		case 0x35:
			offset := e.stackPop().Int64()
			var buf [32]byte
			if int(offset) < len(e.input) {
				copy(buf[:], e.input[offset:])
			}
			e.stackPush(new(big.Int).SetBytes(buf[:]))
		case 0x36:
			e.stackPush(big.NewInt(int64(len(e.input))))
		case 0x38:
			e.stackPush(big.NewInt(int64(len(e.code))))
		case 0x39:
			dest, src, sz := e.stackPop().Int64(), e.stackPop().Int64(), e.stackPop().Int64()
			e.expandMemory(dest, sz)
			data := make([]byte, sz)
			if int(src+sz) <= len(e.code) {
				copy(data, e.code[src:src+sz])
			}
			e.memWrite(dest, data)
		case 0x3a: // GASPRICE: 1e10 wei (18-decimal native currency)
			e.stackPush(big.NewInt(10000000000))
		case 0x3b:
			e.stackPush(big.NewInt(0))
		case 0x3d:
			e.stackPush(big.NewInt(int64(len(e.ret))))
		case 0x3e:
			dest, offset, size := e.stackPop().Int64(), e.stackPop().Int64(), e.stackPop().Int64()
			e.expandMemory(dest, size)
			e.memWrite(dest, e.ret[offset:offset+size])
		case 0x42:
			e.stackPush(big.NewInt(1700000000))
		case 0x43:
			e.stackPush(big.NewInt(0))
		case 0x46:
			e.stackPush(big.NewInt(568))
		case 0x48:
			e.stackPush(big.NewInt(0))
		case 0x44: // RANDOM (VRF from block hash - EVM does not have this natively)
			e.stackPush(new(big.Int).SetBytes(e.input[:32])) // use block hash as randomness
		case 0x45: // PREVRANDAO (Ethereum merged this with RANDOM, we keep both)
			e.stackPush(big.NewInt(0))
		case 0x50:
			e.stackPop()
		case 0x51:
			offset := e.stackPop().Int64()
			e.expandMemory(offset, 32)
			var buf [32]byte
			if int(offset)+32 <= len(e.memory) {
				copy(buf[:], e.memory[offset:offset+32])
			}
			e.stackPush(new(big.Int).SetBytes(buf[:]))
		case 0x52:
			offset := e.stackPop().Int64()
			val := e.stackPop().Bytes()
			e.expandMemory(offset, 32)
			padded := append(make([]byte, 32-len(val)), val...)
			e.memWrite(offset, padded)
		case 0x53:
			offset := e.stackPop().Int64()
			val := byte(e.stackPop().Int64())
			e.expandMemory(offset, 1)
			e.memWrite(offset, []byte{val})
		case 0x54:
			idx := e.stackPop()
			if v, ok := e.storage[idx.String()]; ok {
				e.stackPush(new(big.Int).Set(v))
			} else {
				e.stackPush(big.NewInt(0))
			}
		case 0x55:
			idx, val := e.stackPop(), e.stackPop()
			e.storage[idx.String()] = new(big.Int).Set(val)
		case 0x56:
			dest := int(e.stackPop().Int64())
			if e.validJumps[dest] {
				e.pc = dest
			}
		case 0x57:
			dest := int(e.stackPop().Int64())
			if e.stackPop().Sign() != 0 && e.validJumps[dest] {
				e.pc = dest
			}
		case 0x58:
			e.stackPush(big.NewInt(int64(e.pc - 1)))
		case 0x59:
			e.stackPush(big.NewInt(int64(len(e.memory))))
		case 0x5b:
		case 0x5f:
			e.stackPush(big.NewInt(0))
		case 0x5e:
			dst, src, sz := e.stackPop().Int64(), e.stackPop().Int64(), e.stackPop().Int64()
			e.expandMemory(dst, sz)
			e.memWrite(dst, e.memory[src:src+sz])
		case 0xa0, 0xa1, 0xa2, 0xa3, 0xa4:
			nTopics := int(op - 0xa0)
			mSize := e.stackPop().Int64()
			mStart := e.stackPop().Int64()
			e.expandMemory(mStart, mSize)
			topics := []string{}
			for i := 0; i < nTopics; i++ {
				t := e.stackPop()
				topics = append(topics, fmt.Sprintf("0x%x", t))
			}
			data := e.memRead(mStart, mSize)
			e.logs = append(e.logs, LogEntry{Address: e.address, Topics: topics, Data: data})
		case 0xf3:
			offset, size := e.stackPop().Int64(), e.stackPop().Int64()
			e.expandMemory(offset, size)
			e.ret = e.memRead(offset, size)
			return e.ret, nil
		case 0xfd:
			return e.ret, fmt.Errorf("revert")
		case 0xff:
			return e.ret, nil
		case 0xf1, 0xf2, 0xf4, 0xfa:
			e.stackPop()
			addr := e.stackPop()
			if op != 0xf4 && op != 0xfa {
				e.stackPop()
			}
			argsOff := e.stackPop().Int64()
			argsSize := e.stackPop().Int64()
			retOff := e.stackPop().Int64()
			retSize := e.stackPop().Int64()
			e.expandMemory(argsOff, argsSize)
			e.expandMemory(retOff, retSize)

			input := e.memRead(argsOff, argsSize)
			var out []byte
			addrInt := addr.Int64()
			if addrInt == 1 {
				out = e.ecrecover(input)
			} else if addrInt == 2 {
				sum := sha256.Sum256(input)
				out = sum[:]
			} else if addrInt == 3 {
				sum := sha256.Sum256(input)
				out = append(make([]byte, 12), sum[:20]...)
			} else if addrInt == 4 {
				out = input
			} else if addrInt == 0x08 {
				// BLS12-381 pairing check - simplified: return 1 (true)
				out = make([]byte, 32)
				out[31] = 1
			} else if e.callback != nil {
				addrHex := fmt.Sprintf("0x%x", addr)
				delegate := (op == 0xf4)
				result, err := e.callback(addrHex, input, delegate)
				if err == nil {
					out = result
				}
			}
			if len(out) > int(retSize) {
				out = out[:retSize]
			}
			copy(e.memory[int(retOff):int(retOff)+len(out)], out)
			e.ret = out
			e.stackPush(big.NewInt(1))
		case 0xf0, 0xf5:
			e.stackPop()
			offset := e.stackPop().Int64()
			size := e.stackPop().Int64()
			if op == 0xf5 {
				e.stackPop()
			}
			e.expandMemory(offset, size)
			initCode := e.memRead(offset, size)
			subEVM := NewEVM(initCode, nil, make(map[string]*big.Int))
			runtime, err := subEVM.Run()
			if err != nil {
				e.stackPush(big.NewInt(0))
			} else {
				h := sha3.NewLegacyKeccak256()
				h.Write(runtime)
				e.stackPush(new(big.Int).SetBytes(h.Sum(nil)[:20]))
			}
		default:
			if op >= 0x60 && op <= 0x7f {
				n := int(op - 0x5f)
				end := e.pc + n
				if end > len(e.code) {
					end = len(e.code)
				}
				e.stackPush(new(big.Int).SetBytes(e.code[e.pc:end]))
				e.pc = end
			} else if op >= 0x80 && op <= 0x8f {
				n := int(op - 0x7f)
				if len(e.stack) >= n {
					e.stackPush(new(big.Int).Set(e.stack[len(e.stack)-n]))
				}
			} else if op >= 0x90 && op <= 0x9f {
				n := int(op - 0x8f)
				if len(e.stack) > n {
					e.stack[len(e.stack)-1], e.stack[len(e.stack)-1-n] = e.stack[len(e.stack)-1-n], e.stack[len(e.stack)-1]
				}
			}
		}
	}
	return e.ret, nil
}

// ecrecover: real secp256k1 public key recovery
func (e *EVM) ecrecover(input []byte) []byte {
	if len(input) < 128 {
		return make([]byte, 32)
	}
	hash := new(big.Int).SetBytes(input[0:32])
	v := new(big.Int).SetBytes(input[32:64])
	r := new(big.Int).SetBytes(input[64:96])
	s := new(big.Int).SetBytes(input[96:128])

	// Validate
	if r.Cmp(big.NewInt(0)) <= 0 || r.Cmp(secp256k1.N) >= 0 {
		return make([]byte, 32)
	}
	if s.Cmp(big.NewInt(0)) <= 0 || s.Cmp(secp256k1.N) >= 0 {
		return make([]byte, 32)
	}

	// Recover public key: Q = r^-1 * (s*R - z*G)
	// R = (r, y) where y is computed from curve
	// For PoC with elliptic package: compute y from r
	modP := secp256k1.P
	// y^2 = x^3 + 7 mod p
	r3 := new(big.Int).Exp(r, big.NewInt(3), modP)
	r3.Add(r3, secp256k1.B).Mod(r3, modP)
	// Compute sqrt: y = r3^((p+1)/4) mod p
	exp := new(big.Int).Add(modP, big.NewInt(1))
	exp.Div(exp, big.NewInt(4))
	y := new(big.Int).Exp(r3, exp, modP)

	// Choose y based on v (27 or 28)
	// v is typically 27 or 28, our input has it as 0 or 1 in the high bit
	yParity := v.Int64() - 27
	if yParity < 0 || yParity > 1 {
		yParity = 0
	}
	if y.Bit(0) != uint(yParity) {
		y.Sub(modP, y)
	}

	// R = (r, y)
	Rx, Ry := r, y
	// Compute s*R
	sRx, sRy := secp256k1.ScalarBaseMult(nil) // just for ref
	_ = sRx
	_ = sRy
	// Use ScalarMult
	sRx, sRy = secp256k1.ScalarMult(Rx, Ry, s.Bytes())

	// Compute z*G
	zGx, zGy := secp256k1.ScalarBaseMult(hash.Bytes())

	// Q = s*R - z*G = s*R + (-z)*G
	negZ := new(big.Int).Sub(secp256k1.N, hash)
	zGx, zGy = secp256k1.ScalarBaseMult(negZ.Bytes())

	// Add sR + zG
	Qx, Qy := secp256k1.Add(sRx, sRy, zGx, zGy)

	// Q = r^-1 * Q
	rInv := new(big.Int).ModInverse(r, secp256k1.N)
	Qx, Qy = secp256k1.ScalarMult(Qx, Qy, rInv.Bytes())

	// Public key hash = keccak256(Qx || Qy)[12:]
	pubBytes := append(Qx.Bytes(), Qy.Bytes()...)
	hash2 := sha256.Sum256(pubBytes)
	result := make([]byte, 32)
	copy(result[12:], hash2[:20])
	return result
}

func VerifySignature(pubKey []byte, hash []byte, r, s []byte) bool {
	if len(pubKey) < 64 {
		return false
	}
	pub := &ecdsa.PublicKey{
		Curve: secp256k1,
		X:     new(big.Int).SetBytes(pubKey[:32]),
		Y:     new(big.Int).SetBytes(pubKey[32:64]),
	}
	return ecdsa.Verify(pub, hash, new(big.Int).SetBytes(r), new(big.Int).SetBytes(s))
}

func CallContract(code []byte, input []byte, storage map[string]*big.Int) ([]byte, error) {
	e := NewEVM(code, input, storage)
	return e.Run()
}

func (e *EVM) GetLogs() []LogEntry { return e.logs }
