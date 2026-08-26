package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/lunfardo314/proxima/api/client"
	"github.com/lunfardo314/proxima/ledger"
	"github.com/lunfardo314/proxima/ledger/base"
	"github.com/lunfardo314/proxima/ledger/txbuildercore"
)

// minStorageDeposit returns the minimum storage deposit for a sigLock
// output of the given holder, computed wallet-side: effective size
// (output bytes + index-values tuple bytes + 33 per index value, mirroring
// ledger.effectiveStorageSize) fed through the `storageDeposit($0)`
// schedule via /eval. Same approach as proxi's minStorageDeposit.
func minStorageDeposit(clt *client.APIClient, lib *txbuildercore.Library[any], hid base.HolderID) uint64 {
	out, err := txbuildercore.NewSigLockOutput(lib, 1<<20, hid)
	must(err)
	size := uint64(len(out.Bytes()))
	if ivBin, ierr := out.ConstraintAt(ledger.ConstraintIndexIndexValues); ierr == nil && len(ivBin) > 0 {
		values, verr := ledger.IndexValuesFromBytes(ivBin)
		must(verr)
		size += uint64(len(ivBin)) + uint64(len(values))*33
	}
	deposit, err := clt.EvalU64(0, fmt.Sprintf("storageDeposit(u64/%d)", size))
	must(err)
	return deposit
}

// waitUntilFunded polls the seed node until every spam wallet holds at
// least the required balance (or the context is canceled).
func waitUntilFunded(ctx context.Context, seed *client.APIClient, cfg *config, keys []wallet, requiredBalance uint64) {
	for {
		missing := 0
		for _, w := range keys {
			_, _, bal, err := seed.GetTransferableOutputs(account(w), 256)
			must(err)
			if bal < requiredBalance {
				missing++
			}
		}
		if missing == 0 {
			fmt.Println("all wallets funded")
			return
		}
		fmt.Printf("waiting for %d/%d wallets to reach %d...\n", missing, len(keys), requiredBalance)
		select {
		case <-ctx.Done():
			fmt.Println("aborted while waiting for funding")
			osExit(1)
		case <-time.After(5 * time.Second):
		}
	}
}

// osExit is a variable so tests can intercept it.
var osExit = os.Exit
