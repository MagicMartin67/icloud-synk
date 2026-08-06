package icloud

// SRP-6a client used by Apple's idmsa.apple.com /signin/init + /signin/complete
// endpoints. The parameters match RFC 5054 group 2048 with SHA-256 and the
// quirks of Apple's implementation (the username is not mixed into `x`).

import (
	"crypto/rand"
	"crypto/sha256"
	"fmt"
	"math/big"
)

const srpNHex = "AC6BDB41324A9A9BF166DE5E1389582FAF72B6651987EE07FC3192943DB56050A37329CBB4A099ED8193E0757767A13DD52312AB4B03310DCD7F48A9DA04FD50E8083969EDB767B0CF6095179A163AB3661A05FBD5FAAAE82918A9962F0B93B855F97993EC975EEAA80D740ADBF4FF747359D041D5C33EA71D281E446B14773BCA97B43A23FB801676BD207A436C6481F1D2B9078717461A5B9D32E688F87748544523B524B0D57D5EA77A2775D2ECFA032CFBDBF52FB3786160279004E57AE6AF874E7303CE53299CCC041C7BC308D82A5698F3A8D0C38271AE35F8E9DBFBB694B5C803D89F7AE435DE236D525F54759B65E372FCD68EF20FA7111F9E4AFF73"

var (
	srpN, _ = new(big.Int).SetString(srpNHex, 16)
	srpG    = big.NewInt(2)
)

type srpClient struct {
	username string
	a        *big.Int
	A        *big.Int
}

func newSRPClient(username string) (*srpClient, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return nil, err
	}
	a := new(big.Int).SetBytes(buf)
	return &srpClient{
		username: username,
		a:        a,
		A:        new(big.Int).Exp(srpG, a, srpN),
	}, nil
}

// proof returns M1 and the expected server proof M2 for the given salt, server
// public value B and derived password key (PBKDF2 output).
func (c *srpClient) proof(salt, bBytes, passKey []byte) (m1, m2 []byte, err error) {
	width := (srpN.BitLen() + 7) / 8

	B := new(big.Int).SetBytes(bBytes)
	if new(big.Int).Mod(B, srpN).Sign() == 0 {
		return nil, nil, fmt.Errorf("srp: server sent invalid B")
	}

	k := hashInt(pad(srpN.Bytes(), width), pad(srpG.Bytes(), width))
	u := hashInt(pad(c.A.Bytes(), width), pad(B.Bytes(), width))
	if u.Sign() == 0 {
		return nil, nil, fmt.Errorf("srp: u == 0")
	}

	// Apple: x = H(salt | H(":" | passKey)) — the username is omitted.
	inner := sha256.Sum256(append([]byte(":"), passKey...))
	x := hashInt(salt, inner[:])

	v := new(big.Int).Exp(srpG, x, srpN)

	// S = (B - k*v) ^ (a + u*x) mod N
	base := new(big.Int).Sub(B, new(big.Int).Mul(k, v))
	base.Mod(base, srpN)
	exp := new(big.Int).Add(c.a, new(big.Int).Mul(u, x))
	S := new(big.Int).Exp(base, exp, srpN)

	kKey := sha256.Sum256(S.Bytes())

	hN := sha256.Sum256(srpN.Bytes())
	hG := sha256.Sum256(pad(srpG.Bytes(), width))
	nXorG := make([]byte, len(hN))
	for i := range hN {
		nXorG[i] = hN[i] ^ hG[i]
	}
	hUser := sha256.Sum256([]byte(c.username))

	// The salt is round-tripped through a big.Int, mirroring the reference
	// implementation (leading zero bytes are dropped).
	saltInt := new(big.Int).SetBytes(salt)

	h := sha256.New()
	h.Write(nXorG)
	h.Write(hUser[:])
	h.Write(saltInt.Bytes())
	h.Write(c.A.Bytes())
	h.Write(B.Bytes())
	h.Write(kKey[:])
	m1 = h.Sum(nil)

	h = sha256.New()
	h.Write(c.A.Bytes())
	h.Write(m1)
	h.Write(kKey[:])
	m2 = h.Sum(nil)

	return m1, m2, nil
}

func hashInt(parts ...[]byte) *big.Int {
	h := sha256.New()
	for _, p := range parts {
		h.Write(p)
	}
	return new(big.Int).SetBytes(h.Sum(nil))
}

func pad(b []byte, width int) []byte {
	if len(b) >= width {
		return b
	}
	out := make([]byte, width)
	copy(out[width-len(b):], b)
	return out
}
