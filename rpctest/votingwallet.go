// Copyright (c) 2019-2022 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package rpctest

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/decred/dcrd/blockchain/stake/v5"
	"github.com/decred/dcrd/blockchain/standalone/v2"
	"github.com/decred/dcrd/chaincfg/chainhash"
	"github.com/decred/dcrd/chaincfg/v3"
	"github.com/decred/dcrd/dcrec"
	"github.com/decred/dcrd/dcrec/secp256k1/v4"
	"github.com/decred/dcrd/dcrutil/v4"
	dcrdtypes "github.com/decred/dcrd/rpc/jsonrpc/types/v4"
	"github.com/decred/dcrd/rpcclient/v8"
	"github.com/decred/dcrd/txscript/v4"
	"github.com/decred/dcrd/txscript/v4/sign"
	"github.com/decred/dcrd/txscript/v4/stdaddr"
	"github.com/decred/dcrd/wire"
)

var (
	// feeRate used when sending voting wallet transactions.
	feeRate = dcrutil.Amount(1e4)

	// hardcodedPrivateKey used for all signing operations.
	hardcodedPrivateKey = []byte{
		0x79, 0xa6, 0x1a, 0xdb, 0xc6, 0xe5, 0xa2, 0xe1,
		0x39, 0xd2, 0x71, 0x3a, 0x54, 0x6e, 0xc7, 0xc8,
		0x75, 0x63, 0x2e, 0x75, 0xf1, 0xdf, 0x9c, 0x3f,
		0xa6, 0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
	}

	// nullPay2SSTXChange is the pkscript used on sstxchange outputs of the
	// tickets purchased by the voting wallet. This sends all change into a
	// null address, effectively discarding it.
	nullPay2SSTXChange = []byte{
		0xbd, 0xa9, 0x14, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x87,
	}

	// stakebaseOutPoint is the outpoint that needs to be used in stakebase
	// inputs of vote transactions.
	stakebaseOutPoint = wire.OutPoint{Index: math.MaxUint32}

	// commitAmountMultiplier is a multiplier for the minimum stake difficulty,
	// used to fund inputs used in purchasing tickets. This needs to be high
	// enough that (minimumStakeDifficulty*commitAmountMultiplier) -
	// minimumStakeDifficulty is greater than the dust limit and will allow the
	// ticket to be relayed on the network.
	commitAmountMultiplier = int64(4)
)

type blockConnectedNtfn struct {
	blockHeader  []byte
	transactions [][]byte
}

type winningTicketsNtfn struct {
	blockHash      *chainhash.Hash
	blockHeight    int64
	winningTickets []*chainhash.Hash
}

type ticketInfo struct {
	ticketPrice int64
}

type utxoInfo struct {
	outpoint wire.OutPoint
	amount   int64
}

// VotingWallet stores the state for a simulated voting wallet. Once it is
// started, it will receive notifications from the associated harness, purchase
// tickets and vote on blocks as necessary to keep the chain going.
//
// This currently only implements the bare minimum requirements for maintaining
// a functioning voting wallet and does not handle reorgs, multiple voting and
// ticket buying wallets, setting vote bits, expired/missed votes, etc.
//
// All operations (after initial funding) are done solely via stake
// transactions, so no additional regular transactions are published. This is
// ideal for use in test suites that require a large (greater than SVH) number
// of blocks.
type VotingWallet struct {
	hn         *Harness
	privateKey []byte
	address    stdaddr.Address
	c          *rpcclient.Client

	blockConnectedNtfnChan chan blockConnectedNtfn
	winningTicketsNtfnChan chan winningTicketsNtfn

	p2sstxVer        uint16
	p2sstx           []byte
	commitScriptVer  uint16
	commitScript     []byte
	p2pkh            []byte
	p2pkhVer         uint16
	voteScriptVer    uint16
	voteScript       []byte
	voteRetScriptVer uint16
	voteRetScript    []byte

	errorReporter func(error)

	// miner is a function responsible for generating new blocks. If
	// specified, then this function is used instead of directly calling
	// the underlying harness' Generate().
	miner func(context.Context, uint32) ([]*chainhash.Hash, error)

	subsidyCache *standalone.SubsidyCache

	// utxos are the unspent outpoints not yet locked into a ticket.
	utxos []utxoInfo

	// tickets map the outstanding unspent tickets
	tickets map[chainhash.Hash]ticketInfo

	// maturingVotes tracks the votes maturing at each (future) block height,
	// which will be available for purchasing new tickets.
	maturingVotes map[int64][]utxoInfo

	// tspends to vote for when generating votes.
	tspendVotes []*stake.TreasuryVoteTuple

	// Limit the total number of votes to that.
	limitNbVotes int
}

// NewVotingWallet creates a new minimal voting wallet for the given harness.
// This wallet should be able to maintain the chain generated by the miner node
// of the harness working after it has passed SVH (Stake Validation Height) by
// continuously buying tickets and voting on them.
func NewVotingWallet(ctx context.Context, hn *Harness) (*VotingWallet, error) {
	privKey := secp256k1.PrivKeyFromBytes(hardcodedPrivateKey)
	serPub := privKey.PubKey().SerializeCompressed()
	h160 := stdaddr.Hash160(serPub)
	addr, err := stdaddr.NewAddressPubKeyHashEcdsaSecp256k1V0(h160, hn.ActiveNet)
	if err != nil {
		return nil, fmt.Errorf("unable to generate address for pubkey: %v", err)
	}

	p2sstxVer, p2sstx := addr.VotingRightsScript()
	p2pkhVer, p2pkh := addr.PaymentScript()

	commitAmount := hn.ActiveNet.MinimumStakeDiff * commitAmountMultiplier
	const voteFeeLimit = 0
	const revokeFeeLimit = 16777216
	commitScriptVer, commitScript := addr.RewardCommitmentScript(commitAmount,
		voteFeeLimit, revokeFeeLimit)

	voteScriptVer := uint16(0)
	voteScript, err := txscript.GenerateSSGenVotes(0x0001)
	if err != nil {
		return nil, fmt.Errorf("unable to prepare vote script: %v", err)
	}
	voteReturnScriptVer, voteReturnScript := addr.PayVoteCommitmentScript()

	// Hints for the initial sizing of the tickets and maturing votes maps.
	// Given we have a deterministic purchase process, this should allow us to
	// size these maps only once at setup time.
	hintTicketsCap := requiredTicketCount(hn.ActiveNet)
	hintMaturingVotesCap := int(hn.ActiveNet.CoinbaseMaturity)

	// Buffer length for notification channels. As long as we don't get
	// notifications faster than this, we should be fine.
	bufferLen := 20

	w := &VotingWallet{
		hn:                     hn,
		privateKey:             hardcodedPrivateKey,
		address:                addr,
		p2sstxVer:              p2sstxVer,
		p2sstx:                 p2sstx,
		p2pkhVer:               p2pkhVer,
		p2pkh:                  p2pkh,
		commitScriptVer:        commitScriptVer,
		commitScript:           commitScript,
		voteScriptVer:          voteScriptVer,
		voteScript:             voteScript,
		voteRetScriptVer:       voteReturnScriptVer,
		voteRetScript:          voteReturnScript,
		subsidyCache:           standalone.NewSubsidyCache(hn.ActiveNet),
		limitNbVotes:           int(hn.ActiveNet.TicketsPerBlock),
		tickets:                make(map[chainhash.Hash]ticketInfo, hintTicketsCap),
		maturingVotes:          make(map[int64][]utxoInfo, hintMaturingVotesCap),
		blockConnectedNtfnChan: make(chan blockConnectedNtfn, bufferLen),
		winningTicketsNtfnChan: make(chan winningTicketsNtfn, bufferLen),
	}

	handlers := &rpcclient.NotificationHandlers{
		OnBlockConnected: w.onBlockConnected,
		OnWinningTickets: w.onWinningTickets,
	}

	rpcConf := hn.RPCConfig()
	for i := 0; i < 20; i++ {
		if w.c, err = rpcclient.New(&rpcConf, handlers); err != nil {
			time.Sleep(time.Duration(i) * 50 * time.Millisecond)
			continue
		}
		break
	}
	if w.c == nil {
		return nil, fmt.Errorf("unable to connect to miner node")
	}

	if err = w.c.NotifyBlocks(ctx); err != nil {
		return nil, fmt.Errorf("unable to subscribe to block notifications: %v", err)
	}
	if err = w.c.NotifyWinningTickets(ctx); err != nil {
		return nil, fmt.Errorf("unable to subscribe to winning tickets notification: %v", err)
	}

	return w, nil
}

// Start stars the goroutines necessary for this voting wallet to function.
func (w *VotingWallet) Start(ctx context.Context) error {
	value := w.hn.ActiveNet.MinimumStakeDiff * commitAmountMultiplier

	// Create enough outputs to perform the voting, each with twice the amount
	// of the minimum ticket price.
	//
	// The number of required outputs is twice the coinbase maturity, since
	// we buy TicketsPerBlock tickets per block, starting at SVH-TM. At SVH,
	// TicketsPerBlock tickets will mature and be selected to vote (given they
	// are the only ones in the live ticket pool).
	//
	// Every following block we purchase the same amount of tickets, such that
	// TicketsPerBlock are maturing.
	nbOutputs := requiredTicketCount(w.hn.ActiveNet)
	outputs := make([]*wire.TxOut, nbOutputs)

	for i := 0; i < nbOutputs; i++ {
		outputs[i] = wire.NewTxOut(value, w.p2pkh)
	}

	txid, err := w.hn.SendOutputs(outputs, feeRate)
	if err != nil {
		return fmt.Errorf("unable to fund voting wallet: %v", err)
	}

	// Build the outstanding utxos for ticket buying. These will be the first
	// nbOutputs outputs from txid (assuming the SendOutputs() from above always
	// sends the change last).
	utxos := make([]utxoInfo, nbOutputs)
	for i := 0; i < nbOutputs; i++ {
		utxos[i] = utxoInfo{
			outpoint: wire.OutPoint{Hash: *txid, Index: uint32(i), Tree: wire.TxTreeRegular},
			amount:   value,
		}
	}
	w.utxos = utxos

	go w.handleNotifications(ctx)

	return nil
}

// SetErrorReporting allows users of the voting wallet to specify a function
// that will be called whenever an error happens while purchasing tickets or
// generating votes.
func (w *VotingWallet) SetErrorReporting(f func(err error)) {
	w.errorReporter = f
}

// SetMiner allows users of the voting wallet to specify a function that will
// be used to mine new blocks instead of using the regular Generate function of
// the configured rpcclient.
//
// This allows callers to use a custom function to generate blocks, such as one
// that allows faster mining in simnet.
func (w *VotingWallet) SetMiner(f func(context.Context, uint32) ([]*chainhash.Hash, error)) {
	w.miner = f
}

// LimitNbVotes limits the number of votes issued by the voting wallet to the
// given amount, which is useful for testing scenarios where less than the
// total number of votes per block are cast in the network.
//
// Note that due to limitations in the current implementation of the voting
// wallet, you can only reduce this amount (never increase it) and simnet
// voting will stop once CoinbaseMaturity blocks have passed (so this needs to
// be used only at the end of a test run).
func (w *VotingWallet) LimitNbVotes(newLimit int) error {
	if newLimit < 0 {
		return fmt.Errorf("cannot use negative number of votes")
	}

	if newLimit > w.limitNbVotes {
		return fmt.Errorf("cannot increase number of votes")
	}

	w.limitNbVotes = newLimit
	return nil
}

// GenerateBlocks generates blocks while ensuring the chain will continue past
// SVH indefinitely. This will generate a block then wait for the votes from
// this wallet to be sent and tickets to be purchased before either generating
// the next block or returning.
//
// This function will either return the hashes of the generated blocks or an
// error if, after generating a candidate block, votes and tickets aren't
// submitted in a timely fashion.
func (w *VotingWallet) GenerateBlocks(ctx context.Context, nb uint32) ([]*chainhash.Hash, error) {
	_, startHeight, err := w.c.GetBestBlock(ctx)
	if err != nil {
		return nil, err
	}

	nbVotes := w.limitNbVotes
	nbTickets := int(w.hn.ActiveNet.TicketsPerBlock)
	hashes := make([]*chainhash.Hash, nb)

	miner := w.c.Generate
	if w.miner != nil {
		miner = w.miner
	}

	for i := uint32(0); i < nb; i++ {
		// genHeight is the height of the _next_ block (the one that will be
		// generated once we call generate()).
		genHeight := startHeight + int64(i) + 1

		h, err := miner(ctx, 1)
		if err != nil {
			return nil, fmt.Errorf("unable to generate block at height %d: %v",
				genHeight, err)
		}
		hashes[i] = h[0]

		needsVotes := genHeight >= (w.hn.ActiveNet.StakeValidationHeight - 1)
		needsTickets := genHeight >= ticketPurchaseStartHeight(w.hn.ActiveNet)

		timeout := time.After(time.Second * 5)
		testTimeout := time.After(time.Millisecond * 2)
		gotAllReqs := !needsVotes && !needsTickets
		for !gotAllReqs {
			select {
			case <-timeout:
				mempoolTickets, _ := w.c.GetRawMempool(ctx, dcrdtypes.GRMTickets)
				mempoolVotes, _ := w.c.GetRawMempool(ctx, dcrdtypes.GRMVotes)
				var notGot []string
				if len(mempoolVotes) != nbVotes {
					notGot = append(notGot, "votes")
				}
				if len(mempoolTickets) != nbTickets {
					notGot = append(notGot, "tickets")
				}

				return nil, fmt.Errorf("timeout waiting for %s "+
					"at height %d", strings.Join(notGot, ","), genHeight)
			case <-ctx.Done():
				return nil, fmt.Errorf("wallet is stopping")
			case <-testTimeout:
				mempoolTickets, _ := w.c.GetRawMempool(ctx, dcrdtypes.GRMTickets)
				mempoolVotes, _ := w.c.GetRawMempool(ctx, dcrdtypes.GRMVotes)

				gotAllReqs = (!needsTickets || (len(mempoolTickets) >= nbVotes)) &&
					(!needsVotes || (len(mempoolVotes) >= nbVotes))
				testTimeout = time.After(time.Millisecond * 2)
			}
		}
	}

	return hashes, nil
}

func (w *VotingWallet) logError(err error) {
	if w.errorReporter != nil {
		w.errorReporter(err)
	}
}

func (w *VotingWallet) onBlockConnected(blockHeader []byte, transactions [][]byte) {
	w.blockConnectedNtfnChan <- blockConnectedNtfn{
		blockHeader:  blockHeader,
		transactions: transactions,
	}
}

// newTxOut returns a new transaction output with the given parameters.
func newTxOut(amount int64, pkScriptVer uint16, pkScript []byte) *wire.TxOut {
	return &wire.TxOut{
		Value:    amount,
		Version:  pkScriptVer,
		PkScript: pkScript,
	}
}

func (w *VotingWallet) handleBlockConnectedNtfn(ctx context.Context, ntfn *blockConnectedNtfn) {
	var header wire.BlockHeader
	err := header.FromBytes(ntfn.blockHeader)
	if err != nil {
		w.logError(err)
		return
	}

	blockHeight := int64(header.Height)
	purchaseHeight := ticketPurchaseStartHeight(w.hn.ActiveNet)
	if blockHeight < purchaseHeight {
		// No need to purchase tickets yet.
		return
	}

	// Purchase TicketsPerBlock tickets.
	nbTickets := int(w.hn.ActiveNet.TicketsPerBlock)
	if len(w.utxos) < nbTickets {
		w.logError(fmt.Errorf("number of available utxos (%d) less than "+
			"number of tickets to purchase (%d)", len(w.utxos), nbTickets))
		return
	}

	// Use a slightly higher ticket price than the current minimum, to allow us
	// to ignore stakediff changes at exactly the next block (where purchasing
	// at the current value would cause our tickets to be rejected).
	ticketPrice := header.SBits + (header.SBits / 6)
	commitAmount := w.hn.ActiveNet.MinimumStakeDiff * commitAmountMultiplier

	// Select utxos to use and mark them used.
	utxos := make([]utxoInfo, nbTickets)
	copy(utxos, w.utxos[len(w.utxos)-nbTickets:])
	w.utxos = w.utxos[:len(w.utxos)-nbTickets]

	tickets := make([]wire.MsgTx, nbTickets)
	for i := 0; i < nbTickets; i++ {
		changeAmount := utxos[i].amount - commitAmount

		t := &tickets[i]
		t.Version = wire.TxVersion
		t.AddTxIn(wire.NewTxIn(&utxos[i].outpoint, wire.NullValueIn, nil))
		t.AddTxOut(newTxOut(ticketPrice, w.p2sstxVer, w.p2sstx))
		t.AddTxOut(newTxOut(0, w.commitScriptVer, w.commitScript))
		t.AddTxOut(wire.NewTxOut(changeAmount, nullPay2SSTXChange))

		prevScript := w.p2pkh
		if utxos[i].outpoint.Tree == wire.TxTreeStake {
			prevScript = w.voteRetScript
		}

		sig, err := sign.SignatureScript(t, 0, prevScript, txscript.SigHashAll,
			w.privateKey, dcrec.STEcdsaSecp256k1, true)
		if err != nil {
			w.logError(fmt.Errorf("failed to sign ticket tx: %v", err))
			return
		}
		t.TxIn[0].SignatureScript = sig
	}

	// Submit all tickets to the network.
	promises := make([]*rpcclient.FutureSendRawTransactionResult, nbTickets)
	for i := 0; i < nbTickets; i++ {
		promises[i] = w.c.SendRawTransactionAsync(ctx, &tickets[i], true)
	}

	for i := 0; i < nbTickets; i++ {
		h, err := promises[i].Receive()
		if err != nil {
			w.logError(fmt.Errorf("unable to send ticket tx: %v", err))
			return
		}

		w.tickets[*h] = ticketInfo{
			ticketPrice: ticketPrice,
		}
	}

	// Mark all maturing votes (if any) as available for spending.
	if maturingVotes, has := w.maturingVotes[blockHeight]; has {
		w.utxos = append(w.utxos, maturingVotes...)
		delete(w.maturingVotes, blockHeight)
	}
}

func (w *VotingWallet) onWinningTickets(blockHash *chainhash.Hash, blockHeight int64,
	winningTickets []*chainhash.Hash) {

	w.winningTicketsNtfnChan <- winningTicketsNtfn{
		blockHash:      blockHash,
		blockHeight:    blockHeight,
		winningTickets: winningTickets,
	}
}

func (w *VotingWallet) handleWinningTicketsNtfn(ctx context.Context, ntfn *winningTicketsNtfn) {
	blockRefScript, err := txscript.GenerateSSGenBlockRef(*ntfn.blockHash,
		uint32(ntfn.blockHeight))
	if err != nil {
		w.logError(fmt.Errorf("unable to generate ssgen block ref: %v", err))
		return
	}

	// Always consider the subsidy split enabled since the test voting wallet
	// is only used with simnet where the agenda is always active.
	const isSubsidySplitEnabled = true
	stakebaseValue := w.subsidyCache.CalcStakeVoteSubsidyV2(ntfn.blockHeight,
		isSubsidySplitEnabled)

	// Create the votes. nbVotes is the number of tickets from the wallet that
	// voted.
	votes := make([]wire.MsgTx, w.limitNbVotes)
	nbVotes := 0

	var (
		ticket   ticketInfo
		myTicket bool
	)

	for _, wt := range ntfn.winningTickets {
		if ticket, myTicket = w.tickets[*wt]; !myTicket {
			continue
		}

		voteRetValue := ticket.ticketPrice + stakebaseValue

		// Create a corresponding vote transaction.
		vote := &votes[nbVotes]
		nbVotes++
		vote.Version = wire.TxVersion
		vote.AddTxIn(wire.NewTxIn(
			&stakebaseOutPoint, stakebaseValue, w.hn.ActiveNet.StakeBaseSigScript,
		))
		vote.AddTxIn(wire.NewTxIn(
			wire.NewOutPoint(wt, 0, wire.TxTreeStake),
			wire.NullValueIn, nil,
		))
		vote.AddTxOut(wire.NewTxOut(0, blockRefScript))
		vote.AddTxOut(newTxOut(0, w.voteScriptVer, w.voteScript))
		vote.AddTxOut(newTxOut(voteRetValue, w.voteRetScriptVer, w.voteRetScript))

		// If there are tspends to vote for, create an additional
		// output.
		if len(w.tspendVotes) > 0 {
			n := len(w.tspendVotes)
			opReturnLen := 2 + chainhash.HashSize*n + n
			opReturnData := make([]byte, 0, opReturnLen)
			opReturnData = append(opReturnData, 'T', 'V')
			for _, v := range w.tspendVotes {
				opReturnData = append(opReturnData, v.Hash[:]...)
				opReturnData = append(opReturnData, byte(v.Vote))
			}
			var bldr txscript.ScriptBuilder
			bldr.AddOp(txscript.OP_RETURN)
			bldr.AddData(opReturnData)
			voteScript, err := bldr.Script()
			if err != nil {
				w.logError(fmt.Errorf("unable to construct vote script: %v", err))
				return
			}
			vote.AddTxOut(wire.NewTxOut(0, voteScript))
			vote.Version = wire.TxVersionTreasury
		}

		sig, err := sign.SignatureScript(vote, 1, w.p2sstx, txscript.SigHashAll,
			w.privateKey, dcrec.STEcdsaSecp256k1, true)
		if err != nil {
			w.logError(fmt.Errorf("failed to sign ticket tx: %v", err))
			return
		}
		vote.TxIn[1].SignatureScript = sig

		err = stake.CheckSSGen(vote)
		if err != nil {
			w.logError(fmt.Errorf("transaction is not a valid vote: %v", err))
			return
		}

		// Limit the total number of issued votes if requested.
		if nbVotes >= w.limitNbVotes {
			break
		}
	}

	newUtxos := make([]utxoInfo, nbVotes)

	// Publish the votes.
	promises := make([]*rpcclient.FutureSendRawTransactionResult, nbVotes)
	for i := 0; i < nbVotes; i++ {
		promises[i] = w.c.SendRawTransactionAsync(ctx, &votes[i], true)
	}
	for i := 0; i < nbVotes; i++ {
		h, err := promises[i].Receive()
		if err != nil {
			w.logError(fmt.Errorf("unable to send vote tx: %v", err))
			return
		}
		newUtxos[i] = utxoInfo{
			outpoint: wire.OutPoint{Hash: *h, Index: 2, Tree: wire.TxTreeStake},
			amount:   votes[i].TxOut[2].Value,
		}
	}

	maturingHeight := ntfn.blockHeight + int64(w.hn.ActiveNet.CoinbaseMaturity)
	w.maturingVotes[maturingHeight] = newUtxos
}

// handleNotifications handles all notifications. This blocks until the passed
// context is cancelled and MUST be run on a separate goroutine.
func (w *VotingWallet) handleNotifications(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case ntfn := <-w.blockConnectedNtfnChan:
			w.handleBlockConnectedNtfn(ctx, &ntfn)
		case ntfn := <-w.winningTicketsNtfnChan:
			w.handleWinningTicketsNtfn(ctx, &ntfn)
		}
	}
}

// VoteForTSpends sets the wallet to vote for the provided tspends when
// creating vote transactions.
func (w *VotingWallet) VoteForTSpends(votes []*stake.TreasuryVoteTuple) {
	w.tspendVotes = votes
}

// ticketPurchaseStartHeight returns the block height where ticket buying
// needs to start so that there will be enough mature tickets for voting
// once SVH is reached.
func ticketPurchaseStartHeight(net *chaincfg.Params) int64 {
	return net.StakeValidationHeight - int64(net.TicketMaturity) - 2
}

// requiredTicketCount returns the number of tickets required to maintain the
// network functioning past SVH, assuming only as many tickets as votes will
// be purchased at every block.
func requiredTicketCount(net *chaincfg.Params) int {
	return int((net.CoinbaseMaturity + net.TicketMaturity + 2) * net.TicketsPerBlock)
}
