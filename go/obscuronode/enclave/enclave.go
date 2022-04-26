package enclave

import (
	"crypto/rand"
	"fmt"
	"math/big"

	"github.com/obscuronet/obscuro-playground/go/l1client/rollupcontractlib"

	"github.com/ethereum/go-ethereum/core"

	"github.com/obscuronet/obscuro-playground/go/log"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/obscuronet/obscuro-playground/go/obscurocommon"
	"github.com/obscuronet/obscuro-playground/go/obscuronode/nodecommon"
)

const ChainID = 777 // The unique ID for the Obscuro chain. Required for Geth signing.

// todo - this should become an elaborate data structure
type SharedEnclaveSecret []byte

type StatsCollector interface {
	// Register when a node has to discard the speculative work built on top of the winner of the gossip round.
	L2Recalc(id common.Address)
	RollupWithMoreRecentProof()
}

type enclaveImpl struct {
	node           common.Address
	mining         bool
	storage        Storage
	blockResolver  BlockResolver
	statsCollector StatsCollector
	l1Blockchain   *core.BlockChain

	txCh                 chan nodecommon.L2Tx
	roundWinnerCh        chan *Rollup
	exitCh               chan bool
	speculativeWorkInCh  chan bool
	speculativeWorkOutCh chan speculativeWork
	txHandler            rollupcontractlib.TxHandler
}

func (e *enclaveImpl) IsReady() error {
	return nil // The enclave is local so it is always ready
}

func (e *enclaveImpl) StopClient() {
	// The enclave is local so there is no client to stop
}

func (e *enclaveImpl) Start(block types.Block) {
	// start the speculative rollup execution loop on its own go routine
	go e.start(block)
}

func (e *enclaveImpl) start(block types.Block) {
	env := processingEnvironment{processedTxsMap: make(map[common.Hash]nodecommon.L2Tx)}
	// determine whether the block where the speculative execution will start already contains Obscuro state
	blockState, f := e.storage.FetchBlockState(block.Hash())
	if f {
		env.headRollup = blockState.head
		if env.headRollup != nil {
			env.state = copyState(e.storage.FetchRollupState(env.headRollup.Hash()))
		}
	}

	for {
		select {
		// A new winner was found after gossiping. Start speculatively executing incoming transactions to already have a rollup ready when the next round starts.
		case winnerRollup := <-e.roundWinnerCh:
			env.header = newHeader(winnerRollup, winnerRollup.Header.Height+1, e.node)
			env.headRollup = winnerRollup
			env.state = copyState(e.storage.FetchRollupState(winnerRollup.Hash()))

			// determine the transactions that were not yet included
			env.processedTxs = currentTxs(winnerRollup, e.storage.FetchMempoolTxs(), e.storage)
			env.processedTxsMap = makeMap(env.processedTxs)

			// calculate the State after executing them
			env.state = executeTransactions(env.processedTxs, env.state, env.headRollup.Header)

		case tx := <-e.txCh:
			// only process transactions if there is already a rollup to use as parent
			if env.headRollup != nil {
				_, found := env.processedTxsMap[tx.Hash()]
				if !found {
					env.processedTxsMap[tx.Hash()] = tx
					env.processedTxs = append(env.processedTxs, tx)
					executeTx(env.state, tx)
				}
			}

		case <-e.speculativeWorkInCh:
			if env.header == nil {
				e.speculativeWorkOutCh <- speculativeWork{found: false}
			} else {
				b := make([]nodecommon.L2Tx, 0, len(env.processedTxs))
				b = append(b, env.processedTxs...)
				state := copyState(env.state)
				e.speculativeWorkOutCh <- speculativeWork{
					found: true,
					r:     env.headRollup,
					s:     state,
					h:     env.header,
					txs:   b,
				}
			}

		case <-e.exitCh:
			return
		}
	}
}

func (e *enclaveImpl) ProduceGenesis(blkHash common.Hash) nodecommon.BlockSubmissionResponse {
	rolGenesis := NewRollup(blkHash, nil, obscurocommon.L2GenesisHeight, common.HexToAddress("0x0"), []nodecommon.L2Tx{}, []nodecommon.Withdrawal{}, obscurocommon.GenerateNonce(), common.BigToHash(big.NewInt(0)))
	return nodecommon.BlockSubmissionResponse{
		L2Hash:         rolGenesis.Header.Hash(),
		L1Hash:         blkHash,
		ProducedRollup: rolGenesis.ToExtRollup(),
		IngestedBlock:  true,
	}
}

// IngestBlocks is used to update the enclave with the full history of the L1 chain to date.
func (e *enclaveImpl) IngestBlocks(blocks []*types.Block) []nodecommon.BlockSubmissionResponse {
	result := make([]nodecommon.BlockSubmissionResponse, len(blocks))
	for i, block := range blocks {
		// We skip over the genesis block, to avoid an attack whereby someone submits a block with the same hash as the
		// genesis block but different contents. Since we cannot insert the genesis block into our blockchain, this
		// checking would have to be skipped, potentially allowing an invalid block through.
		if e.isGenesisBlock(block) {
			continue
		}

		if ingestionFailedResponse := e.insertBlockIntoL1Chain(block); ingestionFailedResponse != nil {
			result[i] = *ingestionFailedResponse
			return result // We return early, as all descendant blocks will also fail verification.
		}

		e.storage.StoreBlock(block)
		bs := updateState(block, e.blockResolver, e.storage, e.txHandler)
		if bs == nil {
			result[i] = e.noBlockStateBlockSubmissionResponse(block)
		} else {
			var rollup nodecommon.ExtRollup
			if bs.foundNewRollup {
				rollup = bs.head.ToExtRollup()
			}
			result[i] = e.blockStateBlockSubmissionResponse(bs, rollup)
		}
	}

	return result
}

// SubmitBlock is used to update the enclave with an additional block.
func (e *enclaveImpl) SubmitBlock(block types.Block) nodecommon.BlockSubmissionResponse {
	// As when ingesting, we skip the genesis block.
	if e.isGenesisBlock(&block) {
		return nodecommon.BlockSubmissionResponse{IngestedBlock: false, BlockNotIngestedCause: "Block was genesis block."}
	}

	_, foundBlock := e.storage.FetchBlock(block.Hash())
	if foundBlock {
		return nodecommon.BlockSubmissionResponse{IngestedBlock: false, BlockNotIngestedCause: "Block already ingested."}
	}

	if ingestionFailedResponse := e.insertBlockIntoL1Chain(&block); ingestionFailedResponse != nil {
		return *ingestionFailedResponse
	}

	stored := e.storage.StoreBlock(&block)
	if !stored {
		return nodecommon.BlockSubmissionResponse{IngestedBlock: false}
	}

	_, f := e.storage.FetchBlock(block.Header().ParentHash)
	if !f && e.storage.HeightBlock(&block) > obscurocommon.L1GenesisHeight {
		return nodecommon.BlockSubmissionResponse{IngestedBlock: false, BlockNotIngestedCause: "Block parent not stored."}
	}

	blockState := updateState(&block, e.blockResolver, e.storage, e.txHandler)
	if blockState == nil {
		return e.noBlockStateBlockSubmissionResponse(&block)
	}

	// todo - A verifier node will not produce rollups, we can check the e.mining to get the node behaviour
	e.storage.RemoveMempoolTxs(historicTxs(blockState.head, e.storage))
	r := e.produceRollup(&block, blockState)
	// todo - should store proposal rollups in a different storage as they are ephemeral (round based)
	e.storage.StoreRollup(r)

	log.Log(fmt.Sprintf("Agg%d:> Processed block: b_%d", obscurocommon.ShortAddress(e.node), obscurocommon.ShortHash(block.Hash())))

	return e.blockStateBlockSubmissionResponse(blockState, r.ToExtRollup())
}

func (e *enclaveImpl) SubmitRollup(rollup nodecommon.ExtRollup) {
	r := Rollup{
		Header:       rollup.Header,
		Transactions: decryptTransactions(rollup.Txs),
	}

	// only store if the parent exists
	_, found := e.storage.FetchRollup(r.Header.ParentHash)
	if found {
		e.storage.StoreRollup(&r)
	} else {
		log.Log(fmt.Sprintf("Agg%d:> Received rollup with no parent: r_%d", obscurocommon.ShortAddress(e.node), obscurocommon.ShortHash(r.Hash())))
	}
}

func (e *enclaveImpl) SubmitTx(tx nodecommon.EncryptedTx) error {
	decryptedTx := DecryptTx(tx)
	err := verifySignature(&decryptedTx)
	if err != nil {
		return err
	}
	e.storage.AddMempoolTx(decryptedTx)
	e.txCh <- decryptedTx
	return nil
}

// Checks that the L2Tx has a valid signature.
func verifySignature(decryptedTx *nodecommon.L2Tx) error {
	signer := types.NewLondonSigner(big.NewInt(ChainID))
	_, err := types.Sender(signer, decryptedTx)
	return err
}

func (e *enclaveImpl) RoundWinner(parent obscurocommon.L2RootHash) (nodecommon.ExtRollup, bool, error) {
	head, found := e.storage.FetchRollup(parent)
	if !found {
		return nodecommon.ExtRollup{}, false, fmt.Errorf("rollup not found: r_%s", parent) //nolint
	}

	rollupsReceivedFromPeers := e.storage.FetchRollups(head.Header.Height + 1)
	// filter out rollups with a different Parent
	var usefulRollups []*Rollup
	for _, rol := range rollupsReceivedFromPeers {
		p := e.storage.ParentRollup(rol)
		if p.Hash() == head.Hash() {
			usefulRollups = append(usefulRollups, rol)
		}
	}

	parentState := e.storage.FetchRollupState(head.Hash())
	// determine the winner of the round
	winnerRollup, s := e.findRoundWinner(usefulRollups, head, parentState, e.storage, e.blockResolver)

	e.storage.SetRollupState(winnerRollup.Hash(), s)
	go e.notifySpeculative(winnerRollup)

	// we are the winner
	if winnerRollup.Header.Agg == e.node {
		v := winnerRollup.Proof(e.blockResolver)
		w := e.storage.ParentRollup(winnerRollup)
		log.Log(fmt.Sprintf(">   Agg%d: publish rollup=r_%d(%d)[r_%d]{proof=b_%d}. Txs: %d.  State=%v. ",
			obscurocommon.ShortAddress(e.node),
			obscurocommon.ShortHash(winnerRollup.Hash()), winnerRollup.Header.Height,
			obscurocommon.ShortHash(w.Hash()),
			obscurocommon.ShortHash(v.Hash()),
			len(winnerRollup.Transactions),
			winnerRollup.Header.State,
		))
		return winnerRollup.ToExtRollup(), true, nil
	}
	return nodecommon.ExtRollup{}, false, nil
}

func (e *enclaveImpl) notifySpeculative(winnerRollup *Rollup) {
	e.roundWinnerCh <- winnerRollup
}

func (e *enclaveImpl) Balance(address common.Address) uint64 {
	// todo user encryption
	return e.storage.FetchHeadState().state.balances[address]
}

func (e *enclaveImpl) produceRollup(b *types.Block, bs *blockState) *Rollup {
	// retrieve the speculatively calculated State based on the previous winner and the incoming transactions
	e.speculativeWorkInCh <- true
	speculativeRollup := <-e.speculativeWorkOutCh

	newRollupTxs := speculativeRollup.txs
	newRollupState := speculativeRollup.s
	newRollupHeader := speculativeRollup.h

	// the speculative execution has been processing on top of the wrong parent - due to failure in gossip or publishing to L1
	if !speculativeRollup.found || (speculativeRollup.r.Hash() != bs.head.Hash()) {
		if speculativeRollup.r != nil {
			log.Log(fmt.Sprintf(">   Agg%d: Recalculate. speculative=r_%d(%d), published=r_%d(%d)",
				obscurocommon.ShortAddress(e.node),
				obscurocommon.ShortHash(speculativeRollup.r.Hash()),
				speculativeRollup.r.Header.Height,
				obscurocommon.ShortHash(bs.head.Hash()),
				bs.head.Header.Height),
			)
			if e.statsCollector != nil {
				e.statsCollector.L2Recalc(e.node)
			}
		}

		newRollupHeader = newHeader(bs.head, bs.head.Header.Height+1, e.node)
		// determine transactions to include in new rollup and process them
		newRollupTxs = currentTxs(bs.head, e.storage.FetchMempoolTxs(), e.storage)
		newRollupState = executeTransactions(newRollupTxs, bs.state, newRollupHeader)
	}

	// always process deposits last
	// process deposits from the proof of the parent to the current block (which is the proof of the new rollup)
	proof := bs.head.Proof(e.blockResolver)
	depositTxs := processDeposits(proof, b, e.blockResolver, e.txHandler)
	newRollupState = executeTransactions(depositTxs, newRollupState, newRollupHeader)

	// Postprocessing - withdrawals
	withdrawals := rollupPostProcessingWithdrawals(bs.head, newRollupState)

	// Create a new rollup based on the proof of inclusion of the previous, including all new transactions
	r := NewRollupFromHeader(newRollupHeader, b.Hash(), newRollupTxs, withdrawals, obscurocommon.GenerateNonce(), serialize(newRollupState))
	return &r
}

func (e *enclaveImpl) GetTransaction(txHash common.Hash) *nodecommon.L2Tx {
	// todo add some sort of cache
	rollup := e.storage.FetchHeadState().head

	var found bool
	for {
		txs := rollup.Transactions
		for _, tx := range txs {
			if tx.Hash() == txHash {
				return &tx
			}
		}
		rollup = e.storage.ParentRollup(rollup)
		rollup, found = e.storage.FetchRollup(rollup.Hash())
		if !found {
			panic(fmt.Sprintf("Could not find rollup: r_%s", rollup.Hash()))
		}
		if rollup.Header.Height == obscurocommon.L2GenesisHeight {
			return nil
		}
	}
}

func (e *enclaveImpl) Stop() error {
	e.exitCh <- true
	return nil
}

func (e *enclaveImpl) Attestation() obscurocommon.AttestationReport {
	// Todo
	return obscurocommon.AttestationReport{Owner: e.node}
}

// GenerateSecret - the genesis enclave is responsible with generating the secret entropy
func (e *enclaveImpl) GenerateSecret() obscurocommon.EncryptedSharedEnclaveSecret {
	secret := make([]byte, 32)
	n, err := rand.Read(secret)
	if n != 32 || err != nil {
		panic(fmt.Sprintf("Could not generate secret: %s", err))
	}
	e.storage.StoreSecret(secret)
	return encryptSecret(secret)
}

// InitEnclave - initialise an enclave with a seed received by another enclave
func (e *enclaveImpl) InitEnclave(secret obscurocommon.EncryptedSharedEnclaveSecret) {
	e.storage.StoreSecret(decryptSecret(secret))
}

func (e *enclaveImpl) FetchSecret(obscurocommon.AttestationReport) obscurocommon.EncryptedSharedEnclaveSecret {
	return encryptSecret(e.storage.FetchSecret())
}

func (e *enclaveImpl) IsInitialised() bool {
	return e.storage.FetchSecret() != nil
}

func (e *enclaveImpl) isGenesisBlock(block *types.Block) bool {
	return e.l1Blockchain != nil && block.Hash() != e.l1Blockchain.Genesis().Hash()
}

// Inserts the block into the L1 chain if it exists and the block is not the genesis block. Returns a non-nil
// BlockSubmissionResponse if the insertion failed.
func (e *enclaveImpl) insertBlockIntoL1Chain(block *types.Block) *nodecommon.BlockSubmissionResponse {
	if e.l1Blockchain != nil {
		_, err := e.l1Blockchain.InsertChain(types.Blocks{block})
		if err != nil {
			causeMsg := fmt.Sprintf("Block was invalid: %v", err)
			return &nodecommon.BlockSubmissionResponse{IngestedBlock: false, BlockNotIngestedCause: causeMsg}
		}
	}
	return nil
}

func (e *enclaveImpl) noBlockStateBlockSubmissionResponse(block *types.Block) nodecommon.BlockSubmissionResponse {
	return nodecommon.BlockSubmissionResponse{
		L1Hash:            block.Hash(),
		L1Height:          e.blockResolver.HeightBlock(block),
		L1Parent:          block.ParentHash(),
		IngestedBlock:     true,
		IngestedNewRollup: false,
	}
}

func (e *enclaveImpl) blockStateBlockSubmissionResponse(bs *blockState, rollup nodecommon.ExtRollup) nodecommon.BlockSubmissionResponse {
	return nodecommon.BlockSubmissionResponse{
		L1Hash:            bs.block.Hash(),
		L1Height:          e.blockResolver.HeightBlock(bs.block),
		L1Parent:          bs.block.ParentHash(),
		L2Hash:            bs.head.Hash(),
		L2Height:          bs.head.Header.Height,
		L2Parent:          bs.head.Header.ParentHash,
		Withdrawals:       bs.head.Header.Withdrawals,
		ProducedRollup:    rollup,
		IngestedBlock:     true,
		IngestedNewRollup: bs.foundNewRollup,
	}
}

// Todo - implement with crypto
func decryptSecret(secret obscurocommon.EncryptedSharedEnclaveSecret) SharedEnclaveSecret {
	return SharedEnclaveSecret(secret)
}

// Todo - implement with crypto
func encryptSecret(secret SharedEnclaveSecret) obscurocommon.EncryptedSharedEnclaveSecret {
	return obscurocommon.EncryptedSharedEnclaveSecret(secret)
}

// internal structure to pass information.
type speculativeWork struct {
	found bool
	r     *Rollup
	s     *State
	h     *nodecommon.Header
	txs   []nodecommon.L2Tx
}

// internal structure used for the speculative execution.
type processingEnvironment struct {
	headRollup      *Rollup
	header          *nodecommon.Header
	state           *State
	processedTxs    []nodecommon.L2Tx
	processedTxsMap map[common.Hash]nodecommon.L2Tx
}

// NewEnclave creates a new enclave.
// `genesisJSON` is the configuration for the corresponding L1's genesis block. This is used to validate the blocks
// received from the L1 node if `validateBlocks` is set to true.
func NewEnclave(id common.Address, mining bool, txHandler rollupcontractlib.TxHandler, validateBlocks bool, genesisJSON []byte, collector StatsCollector) nodecommon.Enclave {
	storage := NewStorage()

	var l1Blockchain *core.BlockChain
	if validateBlocks {
		if genesisJSON == nil {
			panic("enclave was configured to validate blocks, but genesis JSON was nil")
		}
		l1Blockchain = NewL1Blockchain(genesisJSON)
	} else {
		log.Log(fmt.Sprintf("Enclave-%d: validateBlocks is set to false. L1 blocks will not be validated.", obscurocommon.ShortAddress(id)))
	}

	return &enclaveImpl{
		node:                 id,
		mining:               mining,
		storage:              storage,
		blockResolver:        storage,
		statsCollector:       collector,
		l1Blockchain:         l1Blockchain,
		txCh:                 make(chan nodecommon.L2Tx),
		roundWinnerCh:        make(chan *Rollup),
		exitCh:               make(chan bool),
		speculativeWorkInCh:  make(chan bool),
		speculativeWorkOutCh: make(chan speculativeWork),
		txHandler:            txHandler,
	}
}
