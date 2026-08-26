// ProximaSpammer — load generator for a Proxima testnet.
//
// A pool of self-funded ED25519 wallets churns plain sigLock transfer
// transactions back to themselves, one transaction per wallet per round,
// spread over one or more access node APIs. Tx construction is fully
// wallet-side: txbuildercore helpers + the library fetched from the node.
// No ledger.L() singleton, no faucet.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/lunfardo314/proxima/api/client"
	"github.com/lunfardo314/proxima/ledger/base"
	"github.com/lunfardo314/proxima/ledger/txbuildercore"
)

type config struct {
	nodeURLs    []string
	numWallets  int
	pace        int
	maxInputs   int
	maxTx       int64
	duration    time.Duration
	tagAlong    *base.ChainID
	tagAlongFee uint64
	fundKeyHex  string
	fundAmount  uint64
	walletsFile string
}

var (
	submitted atomic.Int64
	failed    atomic.Int64
)

func main() {
	cfg := &config{}
	var nodes, tagAlong, durationStr string

	flag.StringVar(&nodes, "node", "http://localhost:8000", "comma-separated node API URLs")
	flag.IntVar(&cfg.numWallets, "wallets", 10, "number of spam wallets")
	flag.IntVar(&cfg.pace, "pace", 25, "ticks between rounds of one wallet (minimum is the transaction pace)")
	flag.IntVar(&cfg.maxInputs, "max-inputs", 8, "maximum inputs consumed by one spam transaction")
	flag.Int64Var(&cfg.maxTx, "max-tx", 0, "stop after this many transactions in total (0 = unlimited)")
	flag.StringVar(&durationStr, "duration", "", "stop after this duration, e.g. '10m' (empty = unlimited)")
	flag.StringVar(&tagAlong, "tag-along", "", "sequencer chain ID (hex): attach a tag-along fee output to every tx for guaranteed sequencer pickup")
	flag.Uint64Var(&cfg.tagAlongFee, "fee", 100, "tag-along fee (only with -tag-along)")
	flag.StringVar(&cfg.fundKeyHex, "fund", "", "master private key (hex): distribute -funds to the spam wallets, wait until settled, then spam")
	flag.Uint64Var(&cfg.fundAmount, "funds", 0, "amount distributed to each spam wallet by -fund")
	flag.StringVar(&cfg.walletsFile, "wallets-file", "wallets.dat", "file holding the spam wallet private keys, one hex key per line")
	showInfo := flag.Bool("info", false, "show address and balance of every wallet, then exit")
	sweep := flag.Bool("sweep", false, "return all spam-wallet funds to the master (-fund) and exit")
	flag.Parse()

	for _, u := range strings.Split(nodes, ",") {
		if u = strings.TrimSpace(u); u != "" {
			cfg.nodeURLs = append(cfg.nodeURLs, u)
		}
	}
	if len(cfg.nodeURLs) == 0 {
		mustf(false, "no node URLs given")
	}
	if durationStr != "" {
		d, err := time.ParseDuration(durationStr)
		must(err)
		cfg.duration = d
	}
	if tagAlong != "" {
		cid, err := base.ChainIDFromHexString(tagAlong)
		must(err)
		cfg.tagAlong = &cid
	}

	seed := client.NewWithGoogleDNS(cfg.nodeURLs[0])
	lib, err := seed.GetLibrary(nil)
	must(err)
	consts, err := seed.GetLedgerConstants(nil)
	must(err)

	if cfg.tagAlong != nil {
		// the target sequencer's declared MinimumFee is the authority:
		// anything below it is silently never picked up
		info, err := seed.GetSequencerTargetInfo(*cfg.tagAlong)
		must(err)
		if cfg.tagAlongFee < info.MinimumFee {
			fmt.Printf("tag-along fee %d raised to the sequencer's minimum %d\n",
				cfg.tagAlongFee, info.MinimumFee)
			cfg.tagAlongFee = info.MinimumFee
		}
	}
	if cfg.pace < int(consts.TransactionPace) {
		fmt.Printf("pace %d raised to the transaction pace %d\n", cfg.pace, consts.TransactionPace)
		cfg.pace = int(consts.TransactionPace)
	}

	// -info never modifies the wallet file: load only, no appending
	keys := loadWallets(cfg.walletsFile)
	if !*showInfo {
		keys = ensureWallets(cfg.walletsFile, cfg.numWallets)
		if len(keys) > cfg.numWallets {
			keys = keys[:cfg.numWallets]
		}
	}
	mustf(len(keys) > 0, "no wallets in %s", cfg.walletsFile)
	minDeposit := minStorageDeposit(seed, lib, holderID(keys[0]))
	// requiredBalance is not arbitrary: every output must embed a storage
	// deposit covering its bytes on the ledger (minDeposit, evaluated
	// wallet-side through the node's storageDeposit schedule), and each
	// spam round is a full-balance sweep — all outputs merged into one new
	// output minus the fee. That output itself must carry at least
	// minDeposit (makeSweepTransaction rejects a tx when balance <=
	// fee + minDeposit), so two deposits are required: after paying the
	// tag-along fee the churned output still holds a full deposit with
	// margin, and a wallet never drifts to the boundary where tx
	// construction fails or runSpammer idles it out.
	requiredBalance := minDeposit*2 + cfg.tagAlongFee

	if *showInfo {
		printWalletInfo(seed, cfg.nodeURLs[0], keys, requiredBalance)
		return
	}
	if *sweep {
		mustf(cfg.fundKeyHex != "", "-sweep requires -fund (the master key to return the funds to)")
		sweepAll(seed, lib, consts, cfg, keys, minDeposit)
		return
	}
	fmt.Printf("%d wallets, minimum storage deposit of a spam output: %d, required balance per wallet: %d\n",
		len(keys), minDeposit, requiredBalance)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go onInterrupt(cancel)
	if cfg.duration > 0 {
		go func() {
			time.Sleep(cfg.duration)
			cancel()
		}()
	}

	if cfg.fundKeyHex != "" {
		mustf(cfg.fundAmount >= requiredBalance,
			"-funds (%d) must be at least the required balance per wallet (%d)", cfg.fundAmount, requiredBalance)
		fundWallets(ctx, seed, lib, consts, cfg, keys)
		waitUntilFunded(ctx, seed, cfg, keys, requiredBalance)
	}

	runSpammers(ctx, lib, consts, cfg, keys, requiredBalance, minDeposit)
	printSummary(startedAt)
	fmt.Println("spammer stopped")
}

var startedAt = time.Now()

func runSpammers(ctx context.Context, lib *txbuildercore.Library[any], consts *txbuildercore.Constants, cfg *config, keys []wallet, requiredBalance, minDeposit uint64) {
	statsDone := make(chan struct{})
	var statsWg sync.WaitGroup
	statsWg.Add(1)
	go func() {
		defer statsWg.Done()
		ticker := time.NewTicker(10 * time.Second)
		defer ticker.Stop()
		start := startedAt
		for {
			select {
			case <-ticker.C:
				printSummary(start)
			case <-statsDone:
				return
			}
		}
	}()

	var wg sync.WaitGroup
	for i, w := range keys {
		wg.Add(1)
		go func(i int, w wallet) {
			defer wg.Done()
			clt := client.NewWithGoogleDNS(cfg.nodeURLs[i%len(cfg.nodeURLs)])
			runSpammer(ctx, clt, lib, consts, cfg, w, requiredBalance, minDeposit)
		}(i, w)
	}
	wg.Wait()
	close(statsDone)
	statsWg.Wait()
}

func printSummary(since time.Time) {
	s, f := submitted.Load(), failed.Load()
	tps := float64(s) / time.Since(since).Seconds()
	fmt.Printf("[%s] submitted: %d, failed: %d, avg TPS: %.2f\n",
		time.Since(since).Round(time.Second), s, f, tps)
}

func onInterrupt(cancel context.CancelFunc) {
	ch := make(chan os.Signal, 1)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	<-ch
	fmt.Println("\ninterrupted")
	cancel()
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}

func mustf(cond bool, format string, args ...any) {
	if !cond {
		panic(fmt.Sprintf(format, args...))
	}
}
