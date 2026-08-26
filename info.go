package main

import (
	"fmt"
	"strings"

	"github.com/lunfardo314/proxima/api/client"
	"github.com/lunfardo314/proxima/ledger"
	"github.com/lunfardo314/proxima/util"
)

// printWalletInfo shows the address and transferable balance of every
// wallet in the wallet file. No transactions are built. For legacy entries
// (old `address:key` format) the balance of the stored legacy address is
// shown as well, since it may hold funds invisible to the derived address.
func printWalletInfo(seed *client.APIClient, nodeURL string, ws []wallet, requiredBalance uint64) {
	fmt.Printf("%d wallets on %s:\n", len(ws), nodeURL)
	total := uint64(0)
	funded := 0
	for i, w := range ws {
		_, _, bal, err := seed.GetTransferableOutputs(account(w), 256)
		must(err)
		status := ""
		if bal >= requiredBalance {
			status = " ok"
			funded++
		} else if bal > 0 {
			status = " low"
		}
		fmt.Printf("  #%d %s balance: %s%s\n", i, account(w).String(), util.Th(bal), status)
		total += bal
		if w.legacyAddr != "" {
			lb := legacyBalance(seed, w.legacyAddr)
			marker := ""
			if lb > 0 {
				marker = "  <-- funds under legacy address, likely NOT spendable (holder-ID derivation changed)"
			}
			fmt.Printf("      legacy %s balance: %s%s\n", legacyAddrDisplay(w.legacyAddr), util.Th(lb), marker)
		}
	}
	fmt.Printf("total: %s, funded (>= %s): %d/%d\n",
		util.Th(total), util.Th(requiredBalance), funded, len(ws))
}

// legacyAddrDisplay converts a stored legacy address `a(0x<hex>)` into the
// current display form `a/<hex>`.
func legacyAddrDisplay(legacy string) string {
	legacy = strings.TrimPrefix(legacy, "a(0x")
	legacy = strings.TrimSuffix(legacy, ")")
	return "a/" + legacy
}

func legacyBalance(seed *client.APIClient, legacy string) uint64 {
	ctrl, err := ledger.ControllerFromSource(legacyAddrDisplay(legacy))
	if err != nil {
		return 0
	}
	_, _, bal, err := seed.GetTransferableOutputs(ctrl, 256)
	if err != nil {
		return 0
	}
	return bal
}
