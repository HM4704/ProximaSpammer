package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/lunfardo314/proxima/api/client"
	"github.com/lunfardo314/proxima/ledger"
	"github.com/lunfardo314/proxima/ledger/base"
	"github.com/lunfardo314/proxima/ledger/txbuildercore"
	"github.com/lunfardo314/proxima/util"
)

// fundWallets tops up each spam wallet from the master wallet in a single
// transaction. Wallets already holding at least fundAmount are skipped;
// every other wallet receives only its shortfall (fundAmount - balance),
// so the total requested is the minimum needed. When -tag-along is set, a
// single tag-along fee output is attached — one settled tx puts every
// wallet output into the state, so one fee funds all wallets.
func fundWallets(ctx context.Context, seed *client.APIClient, lib *txbuildercore.Library[any], consts *txbuildercore.Constants, cfg *config, keys []wallet) {
	masterPriv, err := util.ED25519PrivateKeyFromHexString(cfg.fundKeyHex)
	must(err)
	master := account(wallet{priv: masterPriv})

	type topUp struct {
		idx    int
		amount uint64
	}
	var tops []topUp
	for i, w := range keys {
		_, _, bal, err := seed.GetTransferableOutputs(account(w), 256)
		must(err)
		if bal >= cfg.fundAmount {
			fmt.Printf("wallet #%d: already funded (%s), skipping\n", i, util.Th(bal))
			continue
		}
		tops = append(tops, topUp{idx: i, amount: cfg.fundAmount - bal})
	}
	if len(tops) == 0 {
		fmt.Println("all wallets already funded, nothing to do")
		return
	}

	fee := uint64(0)
	if cfg.tagAlong != nil {
		fee = cfg.tagAlongFee
	}

	outs, _, balance, err := seed.GetTransferableOutputs(master, 256)
	must(err)
	var needed uint64
	for _, t := range tops {
		needed += t.amount
	}
	needed += fee
	fmt.Printf("master %s: transferable balance %d, needed %d (incl. tag-along fee %d) for %d of %d wallets\n",
		master.String(), balance, needed, fee, len(tops), len(keys))
	if balance < needed {
		mustf(false, "master wallet is short of funds")
	}
	mustf(len(outs)+len(tops)+2 <= consts.AttachmentCostBudget,
		"funding tx self-cost %d would exceed the attachment cost budget %d",
		len(outs)+len(tops)+2, consts.AttachmentCostBudget)

	txb := txbuildercore.New(0)
	newestInput := base.NilLedgerTime
	for i, o := range outs {
		txb.ConsumeOutput(o.Output.Bytes(), o.ID)
		if i == 0 {
			txb.PutSignatureUnlock(0)
		} else if err = txb.PutUnlockReference(byte(i), ledger.ConstraintIndexLock, 0); err != nil {
			must(err)
		}
		newestInput = base.MaximumTime(newestInput, o.ID.Timestamp())
	}

	for _, t := range tops {
		out, oerr := txbuildercore.NewSigLockOutput(lib, t.amount, holderID(keys[t.idx]))
		must(oerr)
		txb.ProduceOutput(out.Bytes())
	}
	if cfg.tagAlong != nil {
		taOut, terr := txbuildercore.NewTagAlongOutput(lib, fee, *cfg.tagAlong, holderID(wallet{priv: masterPriv}))
		must(terr)
		txb.ProduceOutput(taOut.Bytes())
	}
	if rem := balance - needed; rem > 0 {
		out, rerr := txbuildercore.NewSigLockOutput(lib, rem, holderID(wallet{priv: masterPriv}))
		must(rerr)
		txb.ProduceOutput(out.Bytes())
	}

	now, err := seed.GetLedgerTime()
	must(err)
	ts := base.MaximumTime(base.T(now.Slot, spamTimestampTick), newestInput.AddTicks(int(consts.TransactionPace)))
	if ts.IsSlotBoundary() {
		ts = ts.AddTicks(1)
	}
	txb.SetTimestamp(ts)
	txb.ComputeInputCommitment()
	txb.SignED25519(masterPriv)

	txBytes := txb.Bytes()
	txid, err := txbuildercore.TxIDFromBytes(txBytes)
	must(err)
	fmt.Printf("submitting funding tx %s: %d input(s), %d output(s), tag-along=%v, %d bytes...\n",
		txid.StringShort(), len(outs), len(tops)+2, cfg.tagAlong != nil, len(txBytes))
	must(seed.SubmitTransaction(txBytes))
}

// sweepAll returns every spam wallet's full balance to the master wallet,
// one transaction per wallet in parallel, each with the tag-along fee
// output (plain transfers are not reliably picked up). Waits for all
// transactions to reach the LRB, then prints a summary.
func sweepAll(seed *client.APIClient, lib *txbuildercore.Library[any], consts *txbuildercore.Constants, cfg *config, ws []wallet, minDeposit uint64) {
	masterPriv, err := util.ED25519PrivateKeyFromHexString(cfg.fundKeyHex)
	must(err)
	masterHID := holderID(wallet{priv: masterPriv})
	master := account(wallet{priv: masterPriv})

	var sent, settled atomic.Int64
	var wg sync.WaitGroup
	for i, w := range ws {
		wg.Add(1)
		go func(i int, w wallet) {
			defer wg.Done()
			// each goroutine gets its own client: APIClient is not
			// guaranteed concurrency-safe for concurrent requests
			clt := client.NewWithGoogleDNS(cfg.nodeURLs[0])

			outs, _, balance, err := clt.GetTransferableOutputs(account(w), 256)
			if err != nil {
				fmt.Printf("wallet #%d: fetch outputs failed: %v\n", i, err)
				return
			}
			fee := uint64(0)
			if cfg.tagAlong != nil {
				fee = cfg.tagAlongFee
			}
			if len(outs) == 0 || balance <= fee+minDeposit {
				fmt.Printf("wallet #%d: nothing to return (%s)\n", i, util.Th(balance))
				return
			}

			txBytes, txid, err := makeSweepTransaction(clt, lib, consts, cfg, w, outs, minDeposit, masterHID)
			if err != nil {
				fmt.Printf("wallet #%d: build failed: %v\n", i, err)
				return
			}
			if err = clt.SubmitTransaction(txBytes); err != nil {
				fmt.Printf("wallet #%d: submit failed: %v\n", i, err)
				return
			}
			sent.Add(1)
			fmt.Printf("wallet #%d: returning %s to %s in tx %s...\n",
				i, util.Th(balance-fee), master.String(), txid.StringShort())

			for t := 0; t < 24; t++ {
				time.Sleep(5 * time.Second)
				_, depth, err := clt.CheckTransactionIDInLRB(txid, 100)
				if err != nil {
					fmt.Printf("wallet #%d: inclusion check failed: %v\n", i, err)
					return
				}
				if depth >= 0 {
					settled.Add(1)
					fmt.Printf("wallet #%d: settled at depth %d\n", i, depth)
					return
				}
			}
			fmt.Printf("wallet #%d: tx %s did not settle within 2 minutes\n", i, txid.StringShort())
		}(i, w)
	}
	wg.Wait()
	fmt.Printf("sweep done: %d sent, %d settled; check balances with -info\n", sent.Load(), settled.Load())
}
