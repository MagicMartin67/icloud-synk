package icloud

import (
	"bytes"
	"crypto/sha256"
	"math/big"
	"testing"
)

// TestSRPAgreesWithAServer runs the client against a reference SRP-6a verifier
// implemented independently below. If the client's group arithmetic or its proof
// layout were wrong, the two would not agree on M1.
func TestSRPAgreesWithAServer(t *testing.T) {
	const username = "user@example.com"
	salt := []byte{0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08}
	passKey := derivePasswordKey("correct horse battery staple", salt, 20, "s2k")

	cl, err := newSRPClient(username)
	if err != nil {
		t.Fatal(err)
	}

	width := (srpN.BitLen() + 7) / 8

	// ---- server side --------------------------------------------------------
	// x and the verifier are derived exactly as Apple's scheme prescribes.
	inner := sha256.Sum256(append([]byte(":"), passKey...))
	x := hashInt(salt, inner[:])
	v := new(big.Int).Exp(srpG, x, srpN)

	k := hashInt(pad(srpN.Bytes(), width), pad(srpG.Bytes(), width))
	b := big.NewInt(0xDEADBEEF) // fixed for reproducibility
	B := new(big.Int).Add(new(big.Int).Mul(k, v), new(big.Int).Exp(srpG, b, srpN))
	B.Mod(B, srpN)

	u := hashInt(pad(cl.A.Bytes(), width), pad(B.Bytes(), width))
	// S = (A * v^u) ^ b mod N
	base := new(big.Int).Mul(cl.A, new(big.Int).Exp(v, u, srpN))
	base.Mod(base, srpN)
	serverS := new(big.Int).Exp(base, b, srpN)
	serverK := sha256.Sum256(serverS.Bytes())

	hN := sha256.Sum256(srpN.Bytes())
	hG := sha256.Sum256(pad(srpG.Bytes(), width))
	nXorG := make([]byte, len(hN))
	for i := range hN {
		nXorG[i] = hN[i] ^ hG[i]
	}
	hUser := sha256.Sum256([]byte(username))
	h := sha256.New()
	h.Write(nXorG)
	h.Write(hUser[:])
	h.Write(new(big.Int).SetBytes(salt).Bytes())
	h.Write(cl.A.Bytes())
	h.Write(B.Bytes())
	h.Write(serverK[:])
	serverM1 := h.Sum(nil)

	// ---- client side --------------------------------------------------------
	m1, m2, err := cl.proof(salt, B.Bytes(), passKey)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(m1, serverM1) {
		t.Fatalf("M1 mismatch:\n client %x\n server %x", m1, serverM1)
	}

	h = sha256.New()
	h.Write(cl.A.Bytes())
	h.Write(serverM1)
	h.Write(serverK[:])
	if want := h.Sum(nil); !bytes.Equal(m2, want) {
		t.Fatalf("M2 mismatch:\n client %x\n server %x", m2, want)
	}
}

func TestSRPRejectsBadB(t *testing.T) {
	cl, err := newSRPClient("user@example.com")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := cl.proof([]byte("salt"), srpN.Bytes(), []byte("key")); err == nil {
		t.Fatal("expected an error for B ≡ 0 mod N")
	}
}

func TestDerivePasswordKeyProtocols(t *testing.T) {
	salt := []byte("saltysalt")
	s2k := derivePasswordKey("hunter2", salt, 20, "s2k")
	fo := derivePasswordKey("hunter2", salt, 20, "s2k_fo")

	if len(s2k) != 32 || len(fo) != 32 {
		t.Fatalf("key lengths = %d/%d, want 32", len(s2k), len(fo))
	}
	// s2k_fo hex-encodes the SHA-256 digest before PBKDF2, so the keys differ.
	if bytes.Equal(s2k, fo) {
		t.Fatal("s2k and s2k_fo produced the same key")
	}
	if !bytes.Equal(s2k, derivePasswordKey("hunter2", salt, 20, "s2k")) {
		t.Fatal("derivation is not deterministic")
	}
}

func TestPadLeftPads(t *testing.T) {
	got := pad([]byte{0xAB}, 4)
	if !bytes.Equal(got, []byte{0, 0, 0, 0xAB}) {
		t.Fatalf("pad = %x, want 000000ab", got)
	}
	if got := pad([]byte{1, 2, 3}, 2); !bytes.Equal(got, []byte{1, 2, 3}) {
		t.Fatalf("pad must not truncate, got %x", got)
	}
}

func TestNewUUIDShape(t *testing.T) {
	id := newUUID()
	if len(id) != 36 || id[14] != '4' {
		t.Fatalf("newUUID = %q, want a v4 UUID", id)
	}
	if id == newUUID() {
		t.Fatal("newUUID returned the same value twice")
	}
}
