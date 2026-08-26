# ProximaSpammer

Load generator for a Proxima testnet. A pool of ED25519 wallets churns plain
sigLock transfer transactions back to themselves over one or more access node
APIs.

Tx construction is fully wallet-side (`txbuildercore` helpers + the ledger
library fetched from the node). No `ledger.L()` singleton, no faucet
dependency.

## How it works

- Each spam wallet keeps its UTXO count stable: every round it sweeps up to
  `-max-inputs` of its own sigLock outputs into **one** output locked back to
  itself (full-balance churn, minus an optional tag-along fee output).
- Wallet keys persist in `wallets.dat`, one hex private key per line, so
  repeated runs reuse the same identities and balances.
- Timestamps follow the ledger rules: at least one transaction pace after the
  newest consumed input, inside the current slot of the node clock, never on
  a slot boundary.
- Optional `-tag-along <seqID>` attaches a tag-along fee output to every tx,
  which routes each transaction through the target sequencer's backlog.
  **Recommended on the current testnet**: plain transfers without a tag-along
  are not reliably picked up by sequencer milestones. The fee actually paid
  is `max(-fee, the sequencer's declared MinimumFee)`, so an undersized
  `-fee` is raised automatically instead of producing ignored transactions.

## Build

```bash
go build -o proxima-spammer .
```

## Usage

Fund and spam in one go (single funding tx from the master wallet, then
churn; with `-tag-along` the funding tx carries one fee output as well,
since plain transfers are not reliably picked up):

```bash
./proxima-spammer \
  -node http://65.21.170.230:8001 \
  -wallets 10 \
  -fund <master-private-key-hex> -funds 20000000
```

Spam only (wallets already funded):

```bash
./proxima-spammer -node http://localhost:8001 -wallets 10 -pace 25
```

Spread over several nodes, tag-along mode (get an active sequencer ID from
`GetLastKnownSequencerData` or the node API; the fee is auto-raised to the
sequencer's minimum):

```bash
./proxima-spammer \
  -node "http://65.21.170.230:8001,http://79.137.70.25:8001" \
  -wallets 20 \
  -tag-along <sequencer-chain-id-hex>
```

Bounded run:

```bash
./proxima-spammer -node http://localhost:8001 -duration 10m -max-tx 5000
```

Return all wallet funds to the master wallet (one tx per wallet, waits for
settlement):

```bash
./proxima-spammer -node http://65.21.170.230:8001 \
  -fund <master-private-key-hex> \
  -tag-along <sequencer-chain-id-hex> -sweep
```

## Flags

| Flag | Default | Meaning |
|------|---------|---------|
| `-node` | `http://localhost:8000` | comma-separated node API URLs, round-robin per wallet |
| `-wallets` | 10 | number of spam wallets |
| `-pace` | 25 | ticks between rounds of one wallet (min = transaction pace) |
| `-max-inputs` | 8 | maximum inputs swept by one transaction |
| `-tag-along` | – | sequencer chain ID for a fee output on every tx |
| `-fee` | 100 | tag-along fee |
| `-fund` | – | master private key hex: distribute `-funds`, wait, then spam |
| `-funds` | 0 | amount per wallet distributed by `-fund` |
| `-max-tx` | 0 | total tx limit across all wallets (0 = unlimited) |
| `-duration` | – | wall-clock limit, e.g. `10m` |
| `-info` | – | show address and balance of every wallet, then exit |
| `-sweep` | – | return all wallet funds to the master (`-fund`), then exit |
| `-wallets-file` | `wallets.dat` | wallet key file |

Wallets need roughly twice the minimum storage deposit of a spam output
(plus the fee in tag-along mode); the tool evaluates the deposit through the
node's `/api/v1/eval` at startup and prints the required balance.


FAQ:

How much TPS can the spammer achieve?

TPS is deterministic by configuration:
TPS ≈ -wallets ÷ (-pace × tick-duration)
Each wallet fires one tx per round, rounds are paced in ticks (testnet tick ≈ 80 ms), so:
Config
10 wallets, pace 25 (~2 s/round)
50 wallets, pace 25
200 wallets, pace 25
Practical ceilings, in order of what bites first:
1. Per-wallet floor: a wallet can't exceed 1 tx per TransactionPace ticks (12 by default) anyway — that's a ledger validity rule, not a tool limit.
2. Node-side limits: txsenders rate-limits per holder ID (irrelevant here — every wallet has its own key), then the txinput queue/attacher pipeline. For reference, the old testnet measurements ran ~200+ multispam senders before stability issues appeared (claude/crash.md: crash under 217 senders).
3. Client-side: each round does a GetTransferableOutputs query + submit per wallet over HTTP; beyond a few hundred wallets the client loop itself becomes the bottleneck before the node does.
So: the tool scales linearly with wallet count up to roughly ~150–200 wallets before you're measuring the node's (or your uplink's) limits rather than the spammer. If you want raw max-throughput numbers rather than steady load, lower -pace to the transaction pace minimum and add wallets/nodes.