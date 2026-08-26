package main

import (
	"context"
	"fmt"
	"time"

	"github.com/lunfardo314/proxima/api/client"
	"github.com/lunfardo314/proxima/ledger"
	"github.com/lunfardo314/proxima/ledger/base"
	"github.com/lunfardo314/proxima/ledger/txbuildercore"
)

// spamTimestampTick is where in the current slot a spam transaction aims.
// Only a starting point: the timestamp is pushed past the newest input by
// the transaction pace. Slot boundaries (tick 0) are reserved for branch
// transactions, hence the +1 bump.
const spamTimestampTick = 10

// runSpammer is the per-wallet churn loop. Each round it sweeps up to
// maxInputs of its own sigLock outputs into one output locked back to
// itself (minus the optional tag-along fee), keeping the wallet's UTXO
// count stable at whatever the funding left it with. Full-balance churn
// means no remainder output and no dust accumulation.
func runSpammer(ctx context.Context, clt *client.APIClient, lib *txbuildercore.Library[any], consts *txbuildercore.Constants, cfg *config, w wallet, requiredBalance, minDeposit uint64) {
	acct := account(w)
	paceDur := time.Duration(cfg.pace) * consts.TickDuration
	fmt.Printf("wallet %s starts spamming, one tx per %d ticks (%v)\n",
		acct.String(), cfg.pace, paceDur)

	for ctx.Err() == nil {
		if cfg.maxTx > 0 && submitted.Load() >= cfg.maxTx {
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(paceDur):
		}

		outs, _, balance, err := clt.GetTransferableOutputs(acct, cfg.maxInputs)
		if err != nil {
			fmt.Printf("%s: fetch outputs failed: %v\n", acct.String(), err)
			failed.Add(1)
			continue
		}
		if len(outs) == 0 || balance < requiredBalance {
			// not funded yet or drained below the storage deposit — idle
			continue
		}

		txBytes, txid, err := makeSweepTransaction(clt, lib, consts, cfg, w, outs, minDeposit, holderID(w))
		if err != nil {
			fmt.Printf("%s: build tx failed: %v\n", acct.String(), err)
			failed.Add(1)
			continue
		}
		if err = clt.SubmitTransaction(txBytes); err != nil {
			fmt.Printf("%s: submit %s failed: %v\n", acct.String(), txid.StringShort(), err)
			failed.Add(1)
			continue
		}
		submitted.Add(1)
	}
}

// makeSweepTransaction sweeps the given outputs into a single sigLock
// output locked to `target`, optionally minus a tag-along fee output.
// The churn loop passes the signer's own holder ID (full-balance
// self-transfer); -sweep passes the master's. Unlock pattern mirrors
// txbuildercore.MakeCompactTransaction: signature unlock on input 0,
// reference unlocks on the rest.
func makeSweepTransaction(clt *client.APIClient, lib *txbuildercore.Library[any], consts *txbuildercore.Constants, cfg *config, w wallet, outs []*ledger.OutputWithID, minDeposit uint64, target base.HolderID) ([]byte, base.TransactionID, error) {
	priv := w.priv
	txb := txbuildercore.New(0)
	inTotal := uint64(0)
	newestInput := base.NilLedgerTime
	for i, o := range outs {
		txb.ConsumeOutput(o.Output.Bytes(), o.ID)
		if i == 0 {
			txb.PutSignatureUnlock(0)
		} else if err := txb.PutUnlockReference(byte(i), ledger.ConstraintIndexLock, 0); err != nil {
			return nil, base.TransactionID{}, err
		}
		inTotal += o.Output.TokenBalance()
		newestInput = base.MaximumTime(newestInput, o.ID.Timestamp())
	}

	hid := holderID(w)
	fee := uint64(0)
	if cfg.tagAlong != nil {
		fee = cfg.tagAlongFee
	}
	if inTotal <= fee+minDeposit {
		return nil, base.TransactionID{}, fmt.Errorf("balance %d does not cover fee %d plus storage deposit %d", inTotal, fee, minDeposit)
	}

	mainOut, err := txbuildercore.NewSigLockOutput(lib, inTotal-fee, target)
	if err != nil {
		return nil, base.TransactionID{}, err
	}
	txb.ProduceOutput(mainOut.Bytes())

	if cfg.tagAlong != nil {
		taOut, err := txbuildercore.NewTagAlongOutput(lib, fee, *cfg.tagAlong, hid)
		if err != nil {
			return nil, base.TransactionID{}, err
		}
		txb.ProduceOutput(taOut.Bytes())
	}

	now, err := clt.GetLedgerTime()
	if err != nil {
		return nil, base.TransactionID{}, err
	}
	ts := base.MaximumTime(
		base.T(now.Slot, spamTimestampTick),
		newestInput.AddTicks(int(consts.TransactionPace)),
	)
	if ts.IsSlotBoundary() {
		ts = ts.AddTicks(1)
	}
	txb.SetTimestamp(ts)
	txb.ComputeInputCommitment()
	txb.SignED25519(priv)

	txBytes := txb.Bytes()
	txid, err := txbuildercore.TxIDFromBytes(txBytes)
	if err != nil {
		return nil, base.TransactionID{}, err
	}
	return txBytes, txid, nil
}
