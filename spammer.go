package main

import (
	"bufio"
	"crypto/ed25519"
	"encoding/binary"
	"encoding/hex"
	"flag"
	"fmt"
	"io/fs"
	"math"
	"math/rand"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/juju/fslock"
	"github.com/lunfardo314/proxima/api/client"
	"github.com/lunfardo314/proxima/ledger"
	"github.com/lunfardo314/proxima/ledger/transaction"
	"github.com/lunfardo314/proxima/proxi/glb"
	"github.com/lunfardo314/proxima/util"
	"github.com/lunfardo314/unitrie/common"
	"golang.org/x/crypto/blake2b"
)

const seedsFile = "wallets.dat"
const minFunds = 50000

const minimumBalance = 1000

type spammerConfig struct {
	outputAmount      uint64
	bundleSize        int
	pace              int
	maxTransactions   int
	maxDuration       time.Duration
	tagAlongSequencer ledger.ChainID
	tagAlongFee       uint64
	target            ledger.Accountable
	finalitySlots     int
}

var g_faucet *string

func main() {
	nbrNodes := flag.Int("nbrNodes", 5, "Number of nodes you want to test against")
	myNode := flag.String("node", "http://192.168.178.35:8000", "Valid node for initial transactions")
	//myNode := flag.String("node", "http://127.0.0.1:8000", "Valid node for initial transactions")
	//g_faucet = flag.String("faucet", "http://127.0.0.1:9500", "faucet url")
	g_faucet = flag.String("faucet", "http://192.168.178.35:9500", "faucet url")

	nodeAPIURL := myNode

	flag.Parse()

	nodes := GetNodes(GetRandomNode(*nodeAPIURL, 8 /* choose from all neighbors */), *nbrNodes, false)
	fmt.Printf("spamming with %d nodes\n", len(nodes))

	InitLedger(GetClient(*nodeAPIURL))

	cfg := spammerConfig{
		outputAmount:    1000,
		bundleSize:      5,
		pace:            25,
		maxTransactions: 0,
		maxDuration:     0,
		//tagAlongSequencer: ledger.ChainIDFromHexString("6393b6781206a652070e78d1391bc467e9d9704e9aa59ec7f7131f329d662dcc"),
		tagAlongFee: 100,
		//target            ledger.Accountable
		finalitySlots: 3,
	}
	cfg.tagAlongSequencer, _ = ledger.ChainIDFromHexString("6393b6781206a652070e78d1391bc467e9d9704e9aa59ec7f7131f329d662dcc")

	var wallets []glb.WalletData
	wallets = make([]glb.WalletData, *nbrNodes)

	for i, node := range nodes {
		_, w := InitWallet(node, i)
		wallets[i] = *w
	}
	var wg sync.WaitGroup
	for i, node := range nodes {
		wg.Add(1)

		go doSpamming(node, cfg, wallets[i])
		time.Sleep(2000 * time.Millisecond)
	}
	wg.Wait()
}

func GetClient(endpoint string) *client.APIClient {
	var timeout []time.Duration
	timeout = []time.Duration{time.Duration(10) * time.Second}
	return client.NewWithGoogleDNS(endpoint, timeout...)
}

func GetRandomNode(url string, numberOfNodes int) string {
	nodes := GetNodes(url, numberOfNodes, true)

	if nodes == nil {
		return ""
	}
	return nodes[rand.Intn(len(nodes))]
}

func GetNodes(url string, numberOfNodes int, noCheck bool) []string {
	var nodes []string
	nodes = append(nodes, url)
	if numberOfNodes == 1 {
		return nodes
	}
	count := 0

	client := GetClient(url)
	peersInfo, err := client.GetPeersInfo()
	AssertNoError(err)

	for _, node := range peersInfo.Peers {
		i := 0
		for _, maddr := range node.MultiAddresses {
			fmt.Printf("Checking url = %v\n", maddr)
			url := "http://" + strings.Split(maddr, "/")[2] + ":8000"
			clt := GetClient(url)
			status, err := clt.GetSyncInfo()
			if err != nil {
				url = "http://" + strings.Split(maddr, "/")[2] + ":8001"
				clt := GetClient(url)
				status, err = clt.GetSyncInfo()
			}
			if err == nil && status.Synced {
				fmt.Println(status)

				nodes = append(nodes, url)
				count++
				if count == (numberOfNodes - 1) {
					return nodes
				}
			}
			i += 1
			if i > 8 {
				break
			}
		}
	}
	return nodes
}

func Fatalf(format string, args ...any) {
	fmt.Printf("Error: "+format+"\n", args...)
	os.Exit(1)
}

func AssertNoError(err error) {
	if err != nil {
		Fatalf("error: %v", err)
	}
}

func NewWallet() (error, glb.WalletData) {

	numBytes := 20
	randomBytes := make([]byte, numBytes)

	// Fill the byte slice with random data
	_, err := rand.Read(randomBytes)
	if err != nil {
		fmt.Println("Error generating random bytes:", err)
		return err, glb.WalletData{}
	}

	var u32 [4]byte
	binary.BigEndian.PutUint32(u32[:], uint32(1))
	seed := blake2b.Sum256(common.Concat([]byte(randomBytes), u32[:]))
	priv := ed25519.NewKeyFromSeed(seed[:])
	pub := priv.Public().(ed25519.PublicKey)
	addr := ledger.AddressED25519FromPublicKey(pub)
	chainId, _ := ledger.ChainIDFromBytes([]byte("6393b6781206a652070e78d1391bc467e9d9704e9aa59ec7f7131f329d662dcc"))
	return nil, glb.WalletData{PrivateKey: priv, Account: addr, Sequencer: &chainId}
}

func getAccountTotal(client *client.APIClient, accountable ledger.Accountable) uint64 {
	var sum uint64
	outs, _, err := client.GetAccountOutputs(accountable)
	glb.AssertNoError(err)

	for _, o := range outs {
		sum += o.Output.Amount()
	}

	return sum
}

func getFunds(client *client.APIClient, account ledger.AddressED25519) {
	faucet := GetClient(*g_faucet)

	path := fmt.Sprintf("/"+"?addr=%s", account.String())

	sumOld := getAccountTotal(client, account)
	answer, err := faucet.Get(path)
	glb.Infof("answer len: %d", len(answer))

	if err != nil || len(answer) > 2 {
		if err != nil {
			glb.Infof("error requesting funds from: %s", err.Error())
		} else {
			glb.Infof("error requesting funds from: %s", string(answer))
		}
	} else {
		glb.Infof("Funds requested successfully!")
	}

	d := 0
	for {
		if getAccountTotal(client, account) > sumOld {
			break
		}
		time.Sleep(5 * time.Second)

		d += 1
		if d > 20 {
			break
		}
	}
}

func InitLedger(client *client.APIClient) {
	ledgerID, err := client.GetLedgerID()
	AssertNoError(err)
	ledger.Init(ledgerID)
}

func InitWallet(url string, index int) (error, *glb.WalletData) {
	client := GetClient(url)

	walletData := LoadWallet(seedsFile, index)
	if walletData == nil {
		err, wd := NewWallet()
		if err == nil {
			walletData = &wd
			SaveWallet(seedsFile, *walletData)
		} else {
			return err, nil
		}

	}
	if getAccountTotal(client, walletData.Account) < minFunds {
		getFunds(client, walletData.Account)
		funds := getAccountTotal(client, walletData.Account)
		glb.Infof(" wallet funds: %d", funds)
	}

	return nil, walletData
}

func doSpamming(url string, cfg spammerConfig, walletData glb.WalletData) {

	client := GetClient(url)
	funds := getAccountTotal(client, walletData.Account)
	glb.Infof(" wallet funds: %d", funds)

	txCounter := 0
	deadline := time.Unix(0, math.MaxInt64)
	if cfg.maxDuration > 0 {
		deadline = time.Now().Add(cfg.maxDuration)
	}

	cfg.target = walletData.Account
	beginTime := time.Now()
	for {
		time.Sleep(time.Duration(cfg.pace) * ledger.TickDuration())

		glb.Assertf(cfg.maxTransactions == 0 || txCounter < cfg.maxTransactions, "maximum transaction limit %d has been reached", cfg.maxTransactions)
		glb.Assertf(time.Now().Before(deadline), "spam duration limit has been reached")

		outs, _, balance, err := client.GetTransferableOutputs(walletData.Account, 256)
		glb.AssertNoError(err)

		glb.Verbosef("Fetched inputs from account %s:\n%s", walletData.Account.String(), glb.LinesOutputsWithIDs(outs).String())

		glb.Infof("transferable balance: %s, number of outputs: %d", util.Th(balance), len(outs))
		requiredBalance := minimumBalance + cfg.outputAmount*uint64(cfg.bundleSize) + cfg.tagAlongFee
		if balance < requiredBalance {
			glb.Infof("transferable balance (%s) is too small for the bundle (required is %s). Waiting for more..",
				util.Th(balance), util.Th(requiredBalance))
			return
			//continue
		}

		bundle, oid := prepareBundle(client, walletData, cfg)
		bundlePace := cfg.pace * len(bundle)
		bundleDuration := time.Duration(bundlePace) * ledger.TickDuration()
		glb.Infof("submitting bundle of %d transactions, total duration %d ticks, %v", len(bundle), bundlePace, bundleDuration)

		for i, txBytes := range bundle {
			err = client.SubmitTransaction(txBytes)
			glb.AssertNoError(err)
			txid, err := transaction.IDFromTransactionBytes(txBytes)
			glb.AssertNoError(err)
			if i == len(bundle)-1 {
				glb.Verbosef("%2d: submitted %s -> tag-along", i, txid.StringShort())
			} else {
				glb.Verbosef("%2d: submitted %s", i, txid.StringShort())
			}
		}

		ReportTxInclusion(client, oid.TransactionID(), time.Second, ledger.Slot(cfg.finalitySlots))

		txCounter += len(bundle)
		timeSinceBeginning := time.Since(beginTime)
		glb.Infof("tx counter: %d, TPS avg: %2f", txCounter, float32(txCounter)/float32(timeSinceBeginning/time.Second))
	}
}

func maxTimestamp(outs []*ledger.OutputWithID) (ret ledger.Time) {
	for _, o := range outs {
		ret = ledger.MaximumTime(ret, o.Timestamp())
	}
	return
}

func prepareBundle(clt *client.APIClient, walletData glb.WalletData, cfg spammerConfig) ([][]byte, ledger.OutputID) {
	ret := make([][]byte, 0)
	txCtx, err := clt.MakeCompactTransaction(walletData.PrivateKey, nil, 0, cfg.bundleSize*3)
	glb.AssertNoError(err)

	numTx := cfg.bundleSize
	var lastOuts []*ledger.OutputWithID
	if txCtx != nil {
		ret = append(ret, txCtx.TransactionBytes())
		lastOut, _ := txCtx.ProducedOutput(0)
		lastOuts = []*ledger.OutputWithID{lastOut}
		numTx--
	} else {
		lastOuts, _, _, err = clt.GetTransferableOutputs(walletData.Account, cfg.bundleSize)
		glb.AssertNoError(err)
	}

	for i := 0; i < numTx; i++ {
		fee := uint64(0)
		if i == numTx-1 {
			fee = cfg.tagAlongFee
		}
		ts := ledger.MaximumTime(maxTimestamp(lastOuts).AddTicks(cfg.pace), ledger.TimeNow())
		txBytes, err := client.MakeTransferTransaction(client.MakeTransferTransactionParams{
			Inputs:        lastOuts,
			Target:        cfg.target.AsLock(),
			Amount:        cfg.outputAmount,
			Remainder:     walletData.Account,
			PrivateKey:    walletData.PrivateKey,
			TagAlongSeqID: &cfg.tagAlongSequencer,
			TagAlongFee:   fee,
			Timestamp:     ts,
		})
		glb.AssertNoError(err)

		ret = append(ret, txBytes)

		lastOuts, err = transaction.OutputsWithIDFromTransactionBytes(txBytes)
		glb.AssertNoError(err)
		lastOuts = util.PurgeSlice(lastOuts, func(o *ledger.OutputWithID) bool {
			return ledger.EqualConstraints(o.Output.Lock(), walletData.Account)
		})
	}
	glb.Verbosef("last outputs in the bundle:")
	for i, o := range lastOuts {
		glb.Verbosef("--- %d:\n%s", i, o.String())
	}

	return ret, lastOuts[0].ID
}

const slotSpan = 2

func ReportTxInclusion(clt *client.APIClient, txid ledger.TransactionID, poll time.Duration, maxSlots ...ledger.Slot) {
	weakFinality := true //GetIsWeakFinality()

	if len(maxSlots) > 0 {
		glb.Infof("Tracking inclusion of %s (hex=%s) for at most %d slots:", txid.String(), txid.StringHex(), maxSlots[0])
	} else {
		glb.Infof("Tracking inclusion of %s (hex=%s):", txid.String(), txid.StringHex())
	}
	inclusionThresholdNumerator, inclusionThresholdDenominator := 2, 3

	fin := "strong"
	if weakFinality {
		fin = "weak"
	}
	glb.Infof("  finality criterion: %s, slot span: %d, strong inclusion threshold: %d/%d",
		fin, slotSpan, inclusionThresholdNumerator, inclusionThresholdDenominator)

	startSlot := ledger.TimeNow().Slot()
	for {
		score, err := clt.QueryTxInclusionScore(txid, inclusionThresholdNumerator, inclusionThresholdDenominator, slotSpan)
		AssertNoError(err)

		lrbid, err := ledger.TransactionIDFromHexString(score.LRBID)
		AssertNoError(err)

		slotsBack := ledger.TimeNow().Slot() - lrbid.Slot()
		glb.Infof("   weak score: %d%%, strong score: %d%%, slot span %d - %d (%d), included in LRB: %v, LRB is slots back: %d",
			score.WeakScore, score.StrongScore, score.EarliestSlot, score.LatestSlot, score.LatestSlot-score.EarliestSlot+1,
			score.IncludedInLRB, slotsBack)

		if weakFinality {
			if score.WeakScore == 100 {
				return
			}
		} else {
			if score.StrongScore == 100 {
				return
			}
		}
		time.Sleep(poll)

		slotNow := ledger.TimeNow().Slot()
		if len(maxSlots) > 0 && maxSlots[0] < slotNow-startSlot {
			glb.Infof("----- failed to reach finality in %d slots", maxSlots[0])
			return
		}
	}
}

func LockFile(fileName string) *fslock.Lock {
	var lock *fslock.Lock = nil
	for i := 0; i < 100; i++ {
		lock = fslock.New(fileName)
		lockErr := lock.TryLock()
		if lockErr != nil {
			//fmt.Println("falied to acquire lock > " + lockErr.Error())
			time.Sleep(100 * time.Millisecond)
		} else {
			break
		}
	}
	return lock
}

func SaveWallet(fileName string, wallet glb.WalletData) {

	// lock the file
	lock := LockFile(fileName)
	if lock == nil {
		fmt.Println("failed to acquire lock ")
		return
	}

	// Open a new file for writing only
	file, err := os.OpenFile(
		fileName,
		os.O_WRONLY|os.O_CREATE|os.O_APPEND,
		0644|fs.ModeExclusive,
	)
	if err != nil {
		fmt.Println(err)
	}
	defer func() {
		file.Close()
		lock.Unlock()
	}()

	seed := fmt.Sprintf("%s:%s", wallet.Account.String(), hex.EncodeToString(wallet.PrivateKey))
	// append bytes to file
	_, err = file.WriteString(seed + "\n")
}

func LoadWallet(fileName string, index int) *glb.WalletData {

	// lock the file
	lock := LockFile(fileName)
	if lock == nil {
		fmt.Println("failed to acquire lock ")
		return nil
	}

	defer lock.Unlock()

	file, err := os.Open(fileName)
	if err != nil {
		return nil
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)

	// Read the rest of the lines into a slice
	var lines []string
	i := 0
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
		if i == index {
			break
		}
		i += 1
	}

	// Close the file and overwrite with the remaining lines
	file.Close()

	if len(lines) < (index + 1) {
		return nil
	}

	seed := lines[index]
	sacc, spriv, found := strings.Cut(seed, ":")
	if !found {
		fmt.Println("Colon not found in the input")
		return nil
	}

	account, err := ledger.AddressED25519FromSource(sacc)
	AssertNoError(err)
	privKey, err := util.ED25519PrivateKeyFromHexString(spriv)
	AssertNoError(err)

	return &glb.WalletData{
		PrivateKey: privKey,
		Account:    account,
	}
}

// func LoadWallet(fileName string, index int) *glb.WalletData {

// 	// lock the file
// 	lock := LockFile(fileName)
// 	if lock == nil {
// 		fmt.Println("failed to acquire lock ")
// 		return nil
// 	}

// 	defer lock.Unlock()

// 	file, err := os.Open(fileName)
// 	if err != nil {
// 		return nil
// 	}
// 	defer file.Close()

// 	scanner := bufio.NewScanner(file)

// 	// Read the first line
// 	var firstLine string
// 	if scanner.Scan() {
// 		firstLine = scanner.Text()
// 	} else {
// 		// If the file is empty or an error occurs
// 		if err := scanner.Err(); err != nil {
// 			return nil
// 		}
// 		return nil
// 	}

// 	// Read the rest of the lines into a slice
// 	var remainingLines []string
// 	for scanner.Scan() {
// 		remainingLines = append(remainingLines, scanner.Text())
// 	}

// 	// Close the file and overwrite with the remaining lines
// 	file.Close()

// 	file, err = os.Create(fileName)
// 	if err != nil {
// 		return nil
// 	}
// 	defer file.Close()

// 	writer := bufio.NewWriter(file)
// 	for _, line := range remainingLines {
// 		_, err := writer.WriteString(line + "\n")
// 		if err != nil {
// 			return nil
// 		}
// 	}

// 	// Flush the buffer to ensure all data is written to the file
// 	err = writer.Flush()
// 	if err != nil {
// 		return nil
// 	}

// 	// ED25519PrivateKeyFromHexString(str string) (ed25519.PrivateKey, error)

// 	return firstLine
// }
