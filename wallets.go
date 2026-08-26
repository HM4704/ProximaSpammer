package main

import (
	"bufio"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"

	"github.com/lunfardo314/proxima/ledger"
	"github.com/lunfardo314/proxima/ledger/base"
	"github.com/lunfardo314/proxima/util"
)

// wallet is one entry of the wallet file.
type wallet struct {
	priv ed25519.PrivateKey
	// legacyAddr is the address stored alongside the key in the legacy
	// ProximaSpammer format `a(0x<hex>):<key hex>`. It was derived with
	// blake2b(pubkey), while the current ledger derives the holder ID as
	// blake2b(signatureType || pubkey) — so the two differ. Funds funded
	// under the legacy address are visible there but may not be spendable
	// under current unlock rules.
	legacyAddr string
}

// loadWallets reads the wallet file without modifying it. Accepted line
// formats:
//   - `<64-byte private key hex>` (current format)
//   - `<address>:<private key hex>` (legacy ProximaSpammer format)
func loadWallets(fileName string) []wallet {
	var ret []wallet
	for _, ln := range readLines(fileName) {
		ln = strings.TrimSpace(ln)
		var legacy string
		if i := strings.LastIndex(ln, ":"); i >= 0 {
			legacy = ln[:i]
			ln = ln[i+1:]
		}
		priv, err := util.ED25519PrivateKeyFromHexString(ln)
		must(err)
		ret = append(ret, wallet{priv: priv, legacyAddr: legacy})
	}
	return ret
}

// ensureWallets loads the wallet file, creating and appending new random
// wallets until it holds `n` of them.
func ensureWallets(fileName string, n int) []wallet {
	ret := loadWallets(fileName)
	f, err := os.OpenFile(fileName, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o600)
	must(err)
	defer f.Close()
	w := bufio.NewWriter(f)
	for len(ret) < n {
		seed := make([]byte, ed25519.SeedSize)
		_, err = rand.Read(seed)
		must(err)
		priv := ed25519.NewKeyFromSeed(seed)
		ret = append(ret, wallet{priv: priv})
		// full 64-byte private key (seed + public part), matching
		// util.ED25519PrivateKeyFromHexString on load
		_, err = w.WriteString(hex.EncodeToString(priv) + "\n")
		must(err)
	}
	must(w.Flush())
	return ret
}

func holderID(w wallet) base.HolderID {
	return base.HolderIDFromED25519PrivateKey(w.priv)
}

func account(w wallet) ledger.SigLock {
	return ledger.SigLockFromED25519PrivateKey(w.priv)
}

func readLines(fileName string) []string {
	f, err := os.Open(fileName)
	if err != nil {
		return nil
	}
	defer f.Close()
	var ret []string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if ln := strings.TrimSpace(sc.Text()); ln != "" {
			ret = append(ret, ln)
		}
	}
	return ret
}
