// Copyright 2021 The go-ethereum Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

// Package catalyst implements the temporary eth1/eth2 RPC integration.
package catalyst

import (
	"bytes"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"

	"github.com/ethereum/go-ethereum/beacon/engine"
	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/common/hexutil"
	"github.com/ethereum/go-ethereum/consensus/misc/eip1559"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/rawdb"
	"github.com/ethereum/go-ethereum/core/stateless"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/core/vm"
	"github.com/ethereum/go-ethereum/eth"
	"github.com/ethereum/go-ethereum/eth/ethconfig"
	"github.com/ethereum/go-ethereum/internal/version"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/miner"
	"github.com/ethereum/go-ethereum/node"
	"github.com/ethereum/go-ethereum/params/forks"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/ethereum/go-ethereum/rpc"
)

// Register adds the engine API to the full node.
func Register(stack *node.Node, backend *eth.Ethereum) error {
	log.Warn("Engine API enabled", "protocol", "eth")
	stack.RegisterAPIs([]rpc.API{
		{
			Namespace:     "engine",
			Service:       NewConsensusAPI(backend),
			Authenticated: true,
		},
	})
	return nil
}

const (
	// invalidBlockHitEviction is the number of times an invalid block can be
	// referenced in forkchoice update or new payload before it is attempted
	// to be reprocessed again.
	invalidBlockHitEviction = 128

	// invalidTipsetsCap is the max number of recent block hashes tracked that
	// have lead to some bad ancestor block. It's just an OOM protection.
	invalidTipsetsCap = 512

	// beaconUpdateStartupTimeout is the time to wait for a beacon client to get
	// attached before starting to issue warnings.
	beaconUpdateStartupTimeout = 30 * time.Second

	// beaconUpdateConsensusTimeout is the max time allowed for a beacon client
	// to send a consensus update before it's considered offline and the user is
	// warned.
	beaconUpdateConsensusTimeout = 2 * time.Minute

	// beaconUpdateWarnFrequency is the frequency at which to warn the user that
	// the beacon client is offline.
	beaconUpdateWarnFrequency = 5 * time.Minute
)

// All methods provided over the engine endpoint.
var caps = []string{
	"engine_forkchoiceUpdatedV1",
	"engine_forkchoiceUpdatedV2",
	"engine_forkchoiceUpdatedV3",
	"engine_forkchoiceUpdatedWithWitnessV1",
	"engine_forkchoiceUpdatedWithWitnessV2",
	"engine_forkchoiceUpdatedWithWitnessV3",
	"engine_exchangeTransitionConfigurationV1",
	"engine_getPayloadV1",
	"engine_getPayloadV2",
	"engine_getPayloadV3",
	"engine_getPayloadV4",
	"engine_getBlobsV1",
	"engine_newPayloadV1",
	"engine_newPayloadV2",
	"engine_newPayloadV3",
	"engine_newPayloadV4",
	"engine_newPayloadWithWitnessV1",
	"engine_newPayloadWithWitnessV2",
	"engine_newPayloadWithWitnessV3",
	"engine_newPayloadWithWitnessV4",
	"engine_executeStatelessPayloadV1",
	"engine_executeStatelessPayloadV2",
	"engine_executeStatelessPayloadV3",
	"engine_executeStatelessPayloadV4",
	"engine_getPayloadBodiesByHashV1",
	"engine_getPayloadBodiesByHashV2",
	"engine_getPayloadBodiesByRangeV1",
	"engine_getPayloadBodiesByRangeV2",
	"engine_getClientVersionV1",
}

type ConsensusAPI struct {
	eth *eth.Ethereum

	remoteBlocks *headerQueue  // Cache of remote payloads received
	localBlocks  *payloadQueue // Cache of local payloads generated

	// The forkchoice update and new payload method require us to return the
	// latest valid hash in an invalid chain. To support that return, we need
	// to track historical bad blocks as well as bad tipsets in case a chain
	// is constantly built on it.
	//
	// There are a few important caveats in this mechanism:
	//   - The bad block tracking is ephemeral, in-memory only. We must never
	//     persist any bad block information to disk as a bug in Geth could end
	//     up blocking a valid chain, even if a later Geth update would accept
	//     it.
	//   - Bad blocks will get forgotten after a certain threshold of import
	//     attempts and will be retried. The rationale is that if the network
	//     really-really-really tries to feed us a block, we should give it a
	//     new chance, perhaps us being racey instead of the block being legit
	//     bad (this happened in Geth at a point with import vs. pending race).
	//   - Tracking all the blocks built on top of the bad one could be a bit
	//     problematic, so we will only track the head chain segment of a bad
	//     chain to allow discarding progressing bad chains and side chains,
	//     without tracking too much bad data.
	invalidBlocksHits map[common.Hash]int           // Ephemeral cache to track invalid blocks and their hit count
	invalidTipsets    map[common.Hash]*types.Header // Ephemeral cache to track invalid tipsets and their bad ancestor
	invalidLock       sync.Mutex                    // Protects the invalid maps from concurrent access

	// Geth can appear to be stuck or do strange things if the beacon client is
	// offline or is sending us strange data. Stash some update stats away so
	// that we can warn the user and not have them open issues on our tracker.
	lastTransitionUpdate time.Time
	lastTransitionLock   sync.Mutex
	lastForkchoiceUpdate time.Time
	lastForkchoiceLock   sync.Mutex
	lastNewPayloadUpdate time.Time
	lastNewPayloadLock   sync.Mutex

	forkchoiceLock sync.Mutex // Lock for the forkChoiceUpdated method
	newPayloadLock sync.Mutex // Lock for the NewPayload method
}

// NewConsensusAPI creates a new consensus api for the given backend.
// The underlying blockchain needs to have a valid terminal total difficulty set.
func NewConsensusAPI(eth *eth.Ethereum) *ConsensusAPI {
	api := newConsensusAPIWithoutHeartbeat(eth)
	go api.heartbeat()
	return api
}

// newConsensusAPIWithoutHeartbeat creates a new consensus api for the SimulatedBeacon Node.
func newConsensusAPIWithoutHeartbeat(eth *eth.Ethereum) *ConsensusAPI {
	if eth.BlockChain().Config().TerminalTotalDifficulty == nil {
		log.Warn("Engine API started but chain not configured for merge yet")
	}
	api := &ConsensusAPI{
		eth:               eth,
		remoteBlocks:      newHeaderQueue(),
		localBlocks:       newPayloadQueue(),
		invalidBlocksHits: make(map[common.Hash]int),
		invalidTipsets:    make(map[common.Hash]*types.Header),
	}
	eth.Downloader().SetBadBlockCallback(api.setInvalidAncestor)
	return api
}

// ForkchoiceUpdatedV1 has several responsibilities:
//
// We try to set our blockchain to the headBlock.
//
// If the method is called with an empty head block: we return success, which can be used
// to check if the engine API is enabled.
//
// If the total difficulty was not reached: we return INVALID.
//
// If the finalizedBlockHash is set: we check if we have the finalizedBlockHash in our db,
// if not we start a sync.
//
// If there are payloadAttributes: we try to assemble a block with the payloadAttributes
// and return its payloadID.
func (api *ConsensusAPI) ForkchoiceUpdatedV1(update engine.ForkchoiceStateV1, payloadAttributes *engine.PayloadAttributes) (engine.ForkChoiceResponse, error) {
	if payloadAttributes != nil {
		if payloadAttributes.Withdrawals != nil || payloadAttributes.BeaconRoot != nil {
			return engine.STATUS_INVALID, engine.InvalidParams.With(errors.New("withdrawals and beacon root not supported in V1"))
		}
		if api.eth.BlockChain().Config().IsShanghai(api.eth.BlockChain().Config().LondonBlock, payloadAttributes.Timestamp) {
			return engine.STATUS_INVALID, engine.InvalidParams.With(errors.New("forkChoiceUpdateV1 called post-shanghai"))
		}
	}
	return api.forkchoiceUpdated(update, payloadAttributes, engine.PayloadV1, false)
}

// ForkchoiceUpdatedV2 is equivalent to V1 with the addition of withdrawals in the payload
// attributes. It supports both PayloadAttributesV1 and PayloadAttributesV2.
func (api *ConsensusAPI) ForkchoiceUpdatedV2(update engine.ForkchoiceStateV1, params *engine.PayloadAttributes) (engine.ForkChoiceResponse, error) {
	if params != nil {
		if params.BeaconRoot != nil {
			return engine.STATUS_INVALID, engine.InvalidPayloadAttributes.With(errors.New("unexpected beacon root"))
		}
		switch api.eth.BlockChain().Config().LatestFork(params.Timestamp) {
		case forks.Paris:
			if params.Withdrawals != nil {
				return engine.STATUS_INVALID, engine.InvalidPayloadAttributes.With(errors.New("withdrawals before shanghai"))
			}
		case forks.Shanghai:
			if params.Withdrawals == nil {
				return engine.STATUS_INVALID, engine.InvalidPayloadAttributes.With(errors.New("missing withdrawals"))
			}
		default:
			return engine.STATUS_INVALID, engine.UnsupportedFork.With(errors.New("forkchoiceUpdatedV2 must only be called with paris and shanghai payloads"))
		}
	}
	return api.forkchoiceUpdated(update, params, engine.PayloadV2, false)
}

// ForkchoiceUpdatedV3 is equivalent to V2 with the addition of parent beacon block root
// in the payload attributes. It supports only PayloadAttributesV3.
func (api *ConsensusAPI) ForkchoiceUpdatedV3(update engine.ForkchoiceStateV1, params *engine.PayloadAttributes) (engine.ForkChoiceResponse, error) {
	if params != nil {
		if params.Withdrawals == nil {
			return engine.STATUS_INVALID, engine.InvalidPayloadAttributes.With(errors.New("missing withdrawals"))
		}
		if params.BeaconRoot == nil {
			return engine.STATUS_INVALID, engine.InvalidPayloadAttributes.With(errors.New("missing beacon root"))
		}
		if api.eth.BlockChain().Config().LatestFork(params.Timestamp) != forks.Cancun && api.eth.BlockChain().Config().LatestFork(params.Timestamp) != forks.Prague {
			return engine.STATUS_INVALID, engine.UnsupportedFork.With(errors.New("forkchoiceUpdatedV3 must only be called for cancun payloads"))
		}
	}
	// TODO(matt): the spec requires that fcu is applied when called on a valid
	// hash, even if params are wrong. To do this we need to split up
	// forkchoiceUpdate into a function that only updates the head and then a
	// function that kicks off block construction.
	return api.forkchoiceUpdated(update, params, engine.PayloadV3, false)
}

// ForkchoiceUpdatedWithWitnessV1 is analogous to ForkchoiceUpdatedV1, only it
// generates an execution witness too if block building was requested.
func (api *ConsensusAPI) ForkchoiceUpdatedWithWitnessV1(update engine.ForkchoiceStateV1, payloadAttributes *engine.PayloadAttributes) (engine.ForkChoiceResponse, error) {
	if payloadAttributes != nil {
		if payloadAttributes.Withdrawals != nil || payloadAttributes.BeaconRoot != nil {
			return engine.STATUS_INVALID, engine.InvalidParams.With(errors.New("withdrawals and beacon root not supported in V1"))
		}
		if api.eth.BlockChain().Config().IsShanghai(api.eth.BlockChain().Config().LondonBlock, payloadAttributes.Timestamp) {
			return engine.STATUS_INVALID, engine.InvalidParams.With(errors.New("forkChoiceUpdateV1 called post-shanghai"))
		}
	}
	return api.forkchoiceUpdated(update, payloadAttributes, engine.PayloadV1, true)
}

// ForkchoiceUpdatedWithWitnessV2 is analogous to ForkchoiceUpdatedV2, only it
// generates an execution witness too if block building was requested.
func (api *ConsensusAPI) ForkchoiceUpdatedWithWitnessV2(update engine.ForkchoiceStateV1, params *engine.PayloadAttributes) (engine.ForkChoiceResponse, error) {
	if params != nil {
		if params.BeaconRoot != nil {
			return engine.STATUS_INVALID, engine.InvalidPayloadAttributes.With(errors.New("unexpected beacon root"))
		}
		switch api.eth.BlockChain().Config().LatestFork(params.Timestamp) {
		case forks.Paris:
			if params.Withdrawals != nil {
				return engine.STATUS_INVALID, engine.InvalidPayloadAttributes.With(errors.New("withdrawals before shanghai"))
			}
		case forks.Shanghai:
			if params.Withdrawals == nil {
				return engine.STATUS_INVALID, engine.InvalidPayloadAttributes.With(errors.New("missing withdrawals"))
			}
		default:
			return engine.STATUS_INVALID, engine.UnsupportedFork.With(errors.New("forkchoiceUpdatedV2 must only be called with paris and shanghai payloads"))
		}
	}
	return api.forkchoiceUpdated(update, params, engine.PayloadV2, true)
}

// ForkchoiceUpdatedWithWitnessV3 is analogous to ForkchoiceUpdatedV3, only it
// generates an execution witness too if block building was requested.
func (api *ConsensusAPI) ForkchoiceUpdatedWithWitnessV3(update engine.ForkchoiceStateV1, params *engine.PayloadAttributes) (engine.ForkChoiceResponse, error) {
	if params != nil {
		if params.Withdrawals == nil {
			return engine.STATUS_INVALID, engine.InvalidPayloadAttributes.With(errors.New("missing withdrawals"))
		}
		if params.BeaconRoot == nil {
			return engine.STATUS_INVALID, engine.InvalidPayloadAttributes.With(errors.New("missing beacon root"))
		}
		if api.eth.BlockChain().Config().LatestFork(params.Timestamp) != forks.Cancun && api.eth.BlockChain().Config().LatestFork(params.Timestamp) != forks.Prague {
			return engine.STATUS_INVALID, engine.UnsupportedFork.With(errors.New("forkchoiceUpdatedV3 must only be called for cancun payloads"))
		}
	}
	// TODO(matt): the spec requires that fcu is applied when called on a valid
	// hash, even if params are wrong. To do this we need to split up
	// forkchoiceUpdate into a function that only updates the head and then a
	// function that kicks off block construction.
	return api.forkchoiceUpdated(update, params, engine.PayloadV3, true)
}

func (api *ConsensusAPI) forkchoiceUpdated(update engine.ForkchoiceStateV1, payloadAttributes *engine.PayloadAttributes, payloadVersion engine.PayloadVersion, payloadWitness bool) (engine.ForkChoiceResponse, error) {
	api.forkchoiceLock.Lock()
	defer api.forkchoiceLock.Unlock()

	log.Trace("Engine API request received", "method", "ForkchoiceUpdated", "head", update.HeadBlockHash, "finalized", update.FinalizedBlockHash, "safe", update.SafeBlockHash)
	if update.HeadBlockHash == (common.Hash{}) {
		log.Warn("Forkchoice requested update to zero hash")
		return engine.STATUS_INVALID, nil // TODO(karalabe): Why does someone send us this?
	}
	// Stash away the last update to warn the user if the beacon client goes offline
	api.lastForkchoiceLock.Lock()
	api.lastForkchoiceUpdate = time.Now()
	api.lastForkchoiceLock.Unlock()

	// Check whether we have the block yet in our database or not. If not, we'll
	// need to either trigger a sync, or to reject this forkchoice update for a
	// reason.
	block := api.eth.BlockChain().GetBlockByHash(update.HeadBlockHash)
	if block == nil {
		// If this block was previously invalidated, keep rejecting it here too
		if res := api.checkInvalidAncestor(update.HeadBlockHash, update.HeadBlockHash); res != nil {
			return engine.ForkChoiceResponse{PayloadStatus: *res, PayloadID: nil}, nil
		}
		// If the head hash is unknown (was not given to us in a newPayload request),
		// we cannot resolve the header, so not much to do. This could be extended in
		// the future to resolve from the `eth` network, but it's an unexpected case
		// that should be fixed, not papered over.
		header := api.remoteBlocks.get(update.HeadBlockHash)
		if header == nil {
			log.Warn("Forkchoice requested unknown head", "hash", update.HeadBlockHash)
			return engine.STATUS_SYNCING, nil
		}
		// If the finalized hash is known, we can direct the downloader to move
		// potentially more data to the freezer from the get go.
		finalized := api.remoteBlocks.get(update.FinalizedBlockHash)

		// Header advertised via a past newPayload request. Start syncing to it.
		context := []interface{}{"number", header.Number, "hash", header.Hash()}
		if update.FinalizedBlockHash != (common.Hash{}) {
			if finalized == nil {
				context = append(context, []interface{}{"finalized", "unknown"}...)
			} else {
				context = append(context, []interface{}{"finalized", finalized.Number}...)
			}
		}
		log.Info("Forkchoice requested sync to new head", context...)
		if err := api.eth.Downloader().BeaconSync(api.eth.SyncMode(), header, finalized); err != nil {
			return engine.STATUS_SYNCING, err
		}
		return engine.STATUS_SYNCING, nil
	}
	// Block is known locally, just sanity check that the beacon client does not
	// attempt to push us back to before the merge.
	if block.Difficulty().BitLen() > 0 && block.NumberU64() > 0 {
		ph := api.eth.BlockChain().GetHeader(block.ParentHash(), block.NumberU64()-1)
		if ph == nil {
			return engine.STATUS_INVALID, errors.New("parent unavailable for difficulty check")
		}
		if ph.Difficulty.Sign() == 0 && block.Difficulty().Sign() > 0 {
			log.Error("Parent block is already post-ttd", "number", block.NumberU64(), "hash", update.HeadBlockHash, "diff", block.Difficulty(), "age", common.PrettyAge(time.Unix(int64(block.Time()), 0)))
			return engine.ForkChoiceResponse{PayloadStatus: engine.INVALID_TERMINAL_BLOCK, PayloadID: nil}, nil
		}
	}
	valid := func(id *engine.PayloadID) engine.ForkChoiceResponse {
		return engine.ForkChoiceResponse{
			PayloadStatus: engine.PayloadStatusV1{Status: engine.VALID, LatestValidHash: &update.HeadBlockHash},
			PayloadID:     id,
		}
	}
	if rawdb.ReadCanonicalHash(api.eth.ChainDb(), block.NumberU64()) != update.HeadBlockHash {
		// Block is not canonical, set head.
		if latestValid, err := api.eth.BlockChain().SetCanonical(block); err != nil {
			return engine.ForkChoiceResponse{PayloadStatus: engine.PayloadStatusV1{Status: engine.INVALID, LatestValidHash: &latestValid}}, err
		}
	} else if api.eth.BlockChain().CurrentBlock().Hash() == update.HeadBlockHash {
		// If the specified head matches with our local head, do nothing and keep
		// generating the payload. It's a special corner case that a few slots are
		// missing and we are requested to generate the payload in slot.
	} else if api.eth.BlockChain().Config().Optimism == nil { // minor Engine API divergence: allow proposers to reorg their own chain
		// If the head block is already in our canonical chain, the beacon client is
		// probably resyncing. Ignore the update.
		log.Info("Ignoring beacon update to old head", "number", block.NumberU64(), "hash", update.HeadBlockHash, "age", common.PrettyAge(time.Unix(int64(block.Time()), 0)), "have", api.eth.BlockChain().CurrentBlock().Number)
		return valid(nil), nil
	}
	api.eth.SetSynced()

	// If the beacon client also advertised a finalized block, mark the local
	// chain final and completely in PoS mode.
	if update.FinalizedBlockHash != (common.Hash{}) {
		// If the finalized block is not in our canonical tree, something is wrong
		finalBlock := api.eth.BlockChain().GetBlockByHash(update.FinalizedBlockHash)
		if finalBlock == nil {
			log.Warn("Final block not available in database", "hash", update.FinalizedBlockHash)
			return engine.STATUS_INVALID, engine.InvalidForkChoiceState.With(errors.New("final block not available in database"))
		} else if rawdb.ReadCanonicalHash(api.eth.ChainDb(), finalBlock.NumberU64()) != update.FinalizedBlockHash {
			log.Warn("Final block not in canonical chain", "number", finalBlock.NumberU64(), "hash", update.FinalizedBlockHash)
			return engine.STATUS_INVALID, engine.InvalidForkChoiceState.With(errors.New("final block not in canonical chain"))
		}
		// Set the finalized block
		api.eth.BlockChain().SetFinalized(finalBlock.Header())
	}
	// Check if the safe block hash is in our canonical tree, if not something is wrong
	if update.SafeBlockHash != (common.Hash{}) {
		safeBlock := api.eth.BlockChain().GetBlockByHash(update.SafeBlockHash)
		if safeBlock == nil {
			log.Warn("Safe block not available in database")
			return engine.STATUS_INVALID, engine.InvalidForkChoiceState.With(errors.New("safe block not available in database"))
		}
		if rawdb.ReadCanonicalHash(api.eth.ChainDb(), safeBlock.NumberU64()) != update.SafeBlockHash {
			log.Warn("Safe block not in canonical chain")
			return engine.STATUS_INVALID, engine.InvalidForkChoiceState.With(errors.New("safe block not in canonical chain"))
		}
		// Set the safe block
		api.eth.BlockChain().SetSafe(safeBlock.Header())
	}
	// If payload generation was requested, create a new block to be potentially
	// sealed by the beacon client. The payload will be requested later, and we
	// will replace it arbitrarily many times in between.

	if payloadAttributes != nil {
		var eip1559Params []byte
		if api.eth.BlockChain().Config().Optimism != nil {
			if payloadAttributes.GasLimit == nil {
				return engine.STATUS_INVALID, engine.InvalidPayloadAttributes.With(errors.New("gasLimit parameter is required"))
			}
			if api.eth.BlockChain().Config().IsHolocene(payloadAttributes.Timestamp) {
				if err := eip1559.ValidateHolocene1559Params(payloadAttributes.EIP1559Params); err != nil {
					return engine.STATUS_INVALID, engine.InvalidPayloadAttributes.With(err)
				}
				eip1559Params = bytes.Clone(payloadAttributes.EIP1559Params)
			} else if len(payloadAttributes.EIP1559Params) != 0 {
				return engine.STATUS_INVALID,
					engine.InvalidPayloadAttributes.With(errors.New("eip155Params not supported prior to Holocene upgrade"))
			}
		}
		transactions := make(types.Transactions, 0, len(payloadAttributes.Transactions))
		for i, otx := range payloadAttributes.Transactions {
			var tx types.Transaction
			if err := tx.UnmarshalBinary(otx); err != nil {
				return engine.STATUS_INVALID, fmt.Errorf("transaction %d is not valid: %v", i, err)
			}
			transactions = append(transactions, &tx)
		}
		args := &miner.BuildPayloadArgs{
			Parent:        update.HeadBlockHash,
			Timestamp:     payloadAttributes.Timestamp,
			FeeRecipient:  payloadAttributes.SuggestedFeeRecipient,
			Random:        payloadAttributes.Random,
			Withdrawals:   payloadAttributes.Withdrawals,
			BeaconRoot:    payloadAttributes.BeaconRoot,
			NoTxPool:      payloadAttributes.NoTxPool,
			Transactions:  transactions,
			GasLimit:      payloadAttributes.GasLimit,
			Version:       payloadVersion,
			EIP1559Params: eip1559Params,
		}
		id := args.Id()
		// If we already are busy generating this work, then we do not need
		// to start a second process.
		if api.localBlocks.has(id) {
			return valid(&id), nil
		}
		payload, err := api.eth.Miner().BuildPayload(args, payloadWitness)
		if err != nil {
			log.Error("Failed to build payload", "err", err)
			return valid(nil), engine.InvalidPayloadAttributes.With(err)
		}
		api.localBlocks.put(id, payload)
		return valid(&id), nil
	}
	return valid(nil), nil
}

// ExchangeTransitionConfigurationV1 checks the given configuration against
// the configuration of the node.
func (api *ConsensusAPI) ExchangeTransitionConfigurationV1(config engine.TransitionConfigurationV1) (*engine.TransitionConfigurationV1, error) {
	log.Trace("Engine API request received", "method", "ExchangeTransitionConfiguration", "ttd", config.TerminalTotalDifficulty)
	if config.TerminalTotalDifficulty == nil {
		return nil, errors.New("invalid terminal total difficulty")
	}
	// Stash away the last update to warn the user if the beacon client goes offline
	api.lastTransitionLock.Lock()
	api.lastTransitionUpdate = time.Now()
	api.lastTransitionLock.Unlock()

	ttd := api.eth.BlockChain().Config().TerminalTotalDifficulty
	if ttd == nil || ttd.Cmp(config.TerminalTotalDifficulty.ToInt()) != 0 {
		log.Warn("Invalid TTD configured", "geth", ttd, "beacon", config.TerminalTotalDifficulty)
		return nil, fmt.Errorf("invalid ttd: execution %v consensus %v", ttd, config.TerminalTotalDifficulty)
	}
	if config.TerminalBlockHash != (common.Hash{}) {
		if hash := api.eth.BlockChain().GetCanonicalHash(uint64(config.TerminalBlockNumber)); hash == config.TerminalBlockHash {
			return &engine.TransitionConfigurationV1{
				TerminalTotalDifficulty: (*hexutil.Big)(ttd),
				TerminalBlockHash:       config.TerminalBlockHash,
				TerminalBlockNumber:     config.TerminalBlockNumber,
			}, nil
		}
		return nil, errors.New("invalid terminal block hash")
	}
	return &engine.TransitionConfigurationV1{TerminalTotalDifficulty: (*hexutil.Big)(ttd)}, nil
}

// GetPayloadV1 returns a cached payload by id.
func (api *ConsensusAPI) GetPayloadV1(payloadID engine.PayloadID) (*engine.ExecutableData, error) {
	if !payloadID.Is(engine.PayloadV1) {
		return nil, engine.UnsupportedFork
	}
	data, err := api.getPayload(payloadID, false)
	if err != nil {
		return nil, err
	}
	return data.ExecutionPayload, nil
}

// GetPayloadV2 returns a cached payload by id.
func (api *ConsensusAPI) GetPayloadV2(payloadID engine.PayloadID) (*engine.ExecutionPayloadEnvelope, error) {
	if !payloadID.Is(engine.PayloadV1, engine.PayloadV2) {
		return nil, engine.UnsupportedFork
	}
	return api.getPayload(payloadID, false)
}

// GetPayloadV3 returns a cached payload by id.
func (api *ConsensusAPI) GetPayloadV3(payloadID engine.PayloadID) (*engine.ExecutionPayloadEnvelope, error) {
	if !payloadID.Is(engine.PayloadV3) {
		return nil, engine.UnsupportedFork
	}
	return api.getPayload(payloadID, false)
}

// GetPayloadV4 returns a cached payload by id.
func (api *ConsensusAPI) GetPayloadV4(payloadID engine.PayloadID) (*engine.ExecutionPayloadEnvelope, error) {
	if !payloadID.Is(engine.PayloadV3) {
		return nil, engine.UnsupportedFork
	}
	return api.getPayload(payloadID, false)
}

func (api *ConsensusAPI) getPayload(payloadID engine.PayloadID, full bool) (*engine.ExecutionPayloadEnvelope, error) {
	log.Trace("Engine API request received", "method", "GetPayload", "id", payloadID)
	data := api.localBlocks.get(payloadID, full)
	if data == nil {
		return nil, engine.UnknownPayload
	}
	return data, nil
}

// GetBlobsV1 returns a blob from the transaction pool.
func (api *ConsensusAPI) GetBlobsV1(hashes []common.Hash) ([]*engine.BlobAndProofV1, error) {
	if len(hashes) > 128 {
		return nil, engine.TooLargeRequest.With(fmt.Errorf("requested blob count too large: %v", len(hashes)))
	}
	res := make([]*engine.BlobAndProofV1, len(hashes))

	blobs, proofs := api.eth.TxPool().GetBlobs(hashes)
	for i := 0; i < len(blobs); i++ {
		if blobs[i] != nil {
			res[i] = &engine.BlobAndProofV1{
				Blob:  (*blobs[i])[:],
				Proof: (*proofs[i])[:],
			}
		}
	}
	return res, nil
}

// NewPayloadV1 creates an Eth1 block, inserts it in the chain, and returns the status of the chain.
func (api *ConsensusAPI) NewPayloadV1(params engine.ExecutableData) (engine.PayloadStatusV1, error) {
	if params.Withdrawals != nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("withdrawals not supported in V1"))
	}
	return api.newPayload(params, nil, nil, nil, false)
}

// NewPayloadV2 creates an Eth1 block, inserts it in the chain, and returns the status of the chain.
func (api *ConsensusAPI) NewPayloadV2(params engine.ExecutableData) (engine.PayloadStatusV1, error) {
	if api.eth.BlockChain().Config().IsCancun(api.eth.BlockChain().Config().LondonBlock, params.Timestamp) {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("can't use newPayloadV2 post-cancun"))
	}
	if api.eth.BlockChain().Config().LatestFork(params.Timestamp) == forks.Shanghai {
		if params.Withdrawals == nil {
			return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil withdrawals post-shanghai"))
		}
	} else {
		if params.Withdrawals != nil {
			return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("non-nil withdrawals pre-shanghai"))
		}
	}
	if params.ExcessBlobGas != nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("non-nil excessBlobGas pre-cancun"))
	}
	if params.BlobGasUsed != nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("non-nil blobGasUsed pre-cancun"))
	}
	return api.newPayload(params, nil, nil, nil, false)
}

// NewPayloadV3 creates an Eth1 block, inserts it in the chain, and returns the status of the chain.
func (api *ConsensusAPI) NewPayloadV3(params engine.ExecutableData, versionedHashes []common.Hash, beaconRoot *common.Hash) (engine.PayloadStatusV1, error) {
	if params.Withdrawals == nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil withdrawals post-shanghai"))
	}
	if params.ExcessBlobGas == nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil excessBlobGas post-cancun"))
	}
	if params.BlobGasUsed == nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil blobGasUsed post-cancun"))
	}

	if versionedHashes == nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil versionedHashes post-cancun"))
	}
	if beaconRoot == nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil beaconRoot post-cancun"))
	}

	if api.eth.BlockChain().Config().LatestFork(params.Timestamp) != forks.Cancun {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.UnsupportedFork.With(errors.New("newPayloadV3 must only be called for cancun payloads"))
	}

	return api.newPayload(params, versionedHashes, beaconRoot, nil, false)
}

// NewPayloadV4 creates an Eth1 block, inserts it in the chain, and returns the status of the chain.
func (api *ConsensusAPI) NewPayloadV4(params engine.ExecutableData, versionedHashes []common.Hash, beaconRoot *common.Hash, executionRequests []hexutil.Bytes) (engine.PayloadStatusV1, error) {
	if params.Withdrawals == nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil withdrawals post-shanghai"))
	}
	if params.ExcessBlobGas == nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil excessBlobGas post-cancun"))
	}
	if params.BlobGasUsed == nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil blobGasUsed post-cancun"))
	}

	if versionedHashes == nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil versionedHashes post-cancun"))
	}
	if beaconRoot == nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil beaconRoot post-cancun"))
	}
	if executionRequests == nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil executionRequests post-prague"))
	}

	if api.eth.BlockChain().Config().LatestFork(params.Timestamp) != forks.Prague {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.UnsupportedFork.With(errors.New("newPayloadV4 must only be called for prague payloads"))
	}

	if api.eth.BlockChain().Config().IsIsthmus(params.Timestamp) && params.WithdrawalsRoot == nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil withdrawalsRoot post-isthmus"))
	}

	requests := convertRequests(executionRequests)
	if err := validateRequests(requests); err != nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(err)
	}
	return api.newPayload(params, versionedHashes, beaconRoot, requests, false)
}

// NewPayloadWithWitnessV1 is analogous to NewPayloadV1, only it also generates
// and returns a stateless witness after running the payload.
func (api *ConsensusAPI) NewPayloadWithWitnessV1(params engine.ExecutableData) (engine.PayloadStatusV1, error) {
	if params.Withdrawals != nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("withdrawals not supported in V1"))
	}
	return api.newPayload(params, nil, nil, nil, true)
}

// NewPayloadWithWitnessV2 is analogous to NewPayloadV2, only it also generates
// and returns a stateless witness after running the payload.
func (api *ConsensusAPI) NewPayloadWithWitnessV2(params engine.ExecutableData) (engine.PayloadStatusV1, error) {
	if api.eth.BlockChain().Config().IsCancun(api.eth.BlockChain().Config().LondonBlock, params.Timestamp) {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("can't use newPayloadV2 post-cancun"))
	}
	if api.eth.BlockChain().Config().LatestFork(params.Timestamp) == forks.Shanghai {
		if params.Withdrawals == nil {
			return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil withdrawals post-shanghai"))
		}
	} else {
		if params.Withdrawals != nil {
			return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("non-nil withdrawals pre-shanghai"))
		}
	}
	if params.ExcessBlobGas != nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("non-nil excessBlobGas pre-cancun"))
	}
	if params.BlobGasUsed != nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("non-nil blobGasUsed pre-cancun"))
	}
	return api.newPayload(params, nil, nil, nil, true)
}

// NewPayloadWithWitnessV3 is analogous to NewPayloadV3, only it also generates
// and returns a stateless witness after running the payload.
func (api *ConsensusAPI) NewPayloadWithWitnessV3(params engine.ExecutableData, versionedHashes []common.Hash, beaconRoot *common.Hash) (engine.PayloadStatusV1, error) {
	if params.Withdrawals == nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil withdrawals post-shanghai"))
	}
	if params.ExcessBlobGas == nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil excessBlobGas post-cancun"))
	}
	if params.BlobGasUsed == nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil blobGasUsed post-cancun"))
	}

	if versionedHashes == nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil versionedHashes post-cancun"))
	}
	if beaconRoot == nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil beaconRoot post-cancun"))
	}

	if api.eth.BlockChain().Config().LatestFork(params.Timestamp) != forks.Cancun {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.UnsupportedFork.With(errors.New("newPayloadWithWitnessV3 must only be called for cancun payloads"))
	}
	return api.newPayload(params, versionedHashes, beaconRoot, nil, true)
}

// NewPayloadWithWitnessV4 is analogous to NewPayloadV4, only it also generates
// and returns a stateless witness after running the payload.
func (api *ConsensusAPI) NewPayloadWithWitnessV4(params engine.ExecutableData, versionedHashes []common.Hash, beaconRoot *common.Hash, executionRequests []hexutil.Bytes) (engine.PayloadStatusV1, error) {
	if params.Withdrawals == nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil withdrawals post-shanghai"))
	}
	if params.ExcessBlobGas == nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil excessBlobGas post-cancun"))
	}
	if params.BlobGasUsed == nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil blobGasUsed post-cancun"))
	}

	if versionedHashes == nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil versionedHashes post-cancun"))
	}
	if beaconRoot == nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil beaconRoot post-cancun"))
	}
	if executionRequests == nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil executionRequests post-prague"))
	}

	if api.eth.BlockChain().Config().LatestFork(params.Timestamp) != forks.Prague {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.UnsupportedFork.With(errors.New("newPayloadWithWitnessV4 must only be called for prague payloads"))
	}
	requests := convertRequests(executionRequests)
	if err := validateRequests(requests); err != nil {
		return engine.PayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(err)
	}
	return api.newPayload(params, versionedHashes, beaconRoot, requests, true)
}

// ExecuteStatelessPayloadV1 is analogous to NewPayloadV1, only it operates in
// a stateless mode on top of a provided witness instead of the local database.
func (api *ConsensusAPI) ExecuteStatelessPayloadV1(params engine.ExecutableData, opaqueWitness hexutil.Bytes) (engine.StatelessPayloadStatusV1, error) {
	if params.Withdrawals != nil {
		return engine.StatelessPayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("withdrawals not supported in V1"))
	}
	return api.executeStatelessPayload(params, nil, nil, nil, opaqueWitness)
}

// ExecuteStatelessPayloadV2 is analogous to NewPayloadV2, only it operates in
// a stateless mode on top of a provided witness instead of the local database.
func (api *ConsensusAPI) ExecuteStatelessPayloadV2(params engine.ExecutableData, opaqueWitness hexutil.Bytes) (engine.StatelessPayloadStatusV1, error) {
	if api.eth.BlockChain().Config().IsCancun(api.eth.BlockChain().Config().LondonBlock, params.Timestamp) {
		return engine.StatelessPayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("can't use newPayloadV2 post-cancun"))
	}
	if api.eth.BlockChain().Config().LatestFork(params.Timestamp) == forks.Shanghai {
		if params.Withdrawals == nil {
			return engine.StatelessPayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil withdrawals post-shanghai"))
		}
	} else {
		if params.Withdrawals != nil {
			return engine.StatelessPayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("non-nil withdrawals pre-shanghai"))
		}
	}
	if params.ExcessBlobGas != nil {
		return engine.StatelessPayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("non-nil excessBlobGas pre-cancun"))
	}
	if params.BlobGasUsed != nil {
		return engine.StatelessPayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("non-nil blobGasUsed pre-cancun"))
	}
	return api.executeStatelessPayload(params, nil, nil, nil, opaqueWitness)
}

// ExecuteStatelessPayloadV3 is analogous to NewPayloadV3, only it operates in
// a stateless mode on top of a provided witness instead of the local database.
func (api *ConsensusAPI) ExecuteStatelessPayloadV3(params engine.ExecutableData, versionedHashes []common.Hash, beaconRoot *common.Hash, opaqueWitness hexutil.Bytes) (engine.StatelessPayloadStatusV1, error) {
	if params.Withdrawals == nil {
		return engine.StatelessPayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil withdrawals post-shanghai"))
	}
	if params.ExcessBlobGas == nil {
		return engine.StatelessPayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil excessBlobGas post-cancun"))
	}
	if params.BlobGasUsed == nil {
		return engine.StatelessPayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil blobGasUsed post-cancun"))
	}

	if versionedHashes == nil {
		return engine.StatelessPayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil versionedHashes post-cancun"))
	}
	if beaconRoot == nil {
		return engine.StatelessPayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil beaconRoot post-cancun"))
	}

	if api.eth.BlockChain().Config().LatestFork(params.Timestamp) != forks.Cancun {
		return engine.StatelessPayloadStatusV1{Status: engine.INVALID}, engine.UnsupportedFork.With(errors.New("executeStatelessPayloadV3 must only be called for cancun payloads"))
	}
	return api.executeStatelessPayload(params, versionedHashes, beaconRoot, nil, opaqueWitness)
}

// ExecuteStatelessPayloadV4 is analogous to NewPayloadV4, only it operates in
// a stateless mode on top of a provided witness instead of the local database.
func (api *ConsensusAPI) ExecuteStatelessPayloadV4(params engine.ExecutableData, versionedHashes []common.Hash, beaconRoot *common.Hash, executionRequests []hexutil.Bytes, opaqueWitness hexutil.Bytes) (engine.StatelessPayloadStatusV1, error) {
	if params.Withdrawals == nil {
		return engine.StatelessPayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil withdrawals post-shanghai"))
	}
	if params.ExcessBlobGas == nil {
		return engine.StatelessPayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil excessBlobGas post-cancun"))
	}
	if params.BlobGasUsed == nil {
		return engine.StatelessPayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil blobGasUsed post-cancun"))
	}

	if versionedHashes == nil {
		return engine.StatelessPayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil versionedHashes post-cancun"))
	}
	if beaconRoot == nil {
		return engine.StatelessPayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil beaconRoot post-cancun"))
	}
	if executionRequests == nil {
		return engine.StatelessPayloadStatusV1{Status: engine.INVALID}, engine.InvalidParams.With(errors.New("nil executionRequests post-prague"))
	}

	if api.eth.BlockChain().Config().LatestFork(params.Timestamp) != forks.Prague {
		return engine.StatelessPayloadStatusV1{Status: engine.INVALID}, engine.UnsupportedFork.With(errors.New("executeStatelessPayloadV4 must only be called for prague payloads"))
	}
	requests := convertRequests(executionRequests)
	return api.executeStatelessPayload(params, versionedHashes, beaconRoot, requests, opaqueWitness)
}

func (api *ConsensusAPI) newPayload(params engine.ExecutableData, versionedHashes []common.Hash, beaconRoot *common.Hash, requests [][]byte, witness bool) (engine.PayloadStatusV1, error) {
	// The locking here is, strictly, not required. Without these locks, this can happen:
	//
	// 1. NewPayload( execdata-N ) is invoked from the CL. It goes all the way down to
	//      api.eth.BlockChain().InsertBlockWithoutSetHead, where it is blocked on
	//      e.g database compaction.
	// 2. The call times out on the CL layer, which issues another NewPayload (execdata-N) call.
	//    Similarly, this also get stuck on the same place. Importantly, since the
	//    first call has not gone through, the early checks for "do we already have this block"
	//    will all return false.
	// 3. When the db compaction ends, then N calls inserting the same payload are processed
	//    sequentially.
	// Hence, we use a lock here, to be sure that the previous call has finished before we
	// check whether we already have the block locally.

	// OP-Stack diff: payload must have empty extraData before Holocene and hold eip-1559 params after Holocene.
	if cfg := api.eth.BlockChain().Config(); cfg.IsHolocene(params.Timestamp) {
		if err := eip1559.ValidateHoloceneExtraData(params.ExtraData); err != nil {
			return api.invalid(err, nil), nil
		}
	} else if cfg.IsOptimism() {
		if len(params.ExtraData) > 0 {
			return api.invalid(errors.New("extraData must be empty before Holocene"), nil), nil
		}
	}

	api.newPayloadLock.Lock()
	defer api.newPayloadLock.Unlock()

	log.Trace("Engine API request received", "method", "NewPayload", "number", params.Number, "hash", params.BlockHash)
	block, err := engine.ExecutableDataToBlock(params, versionedHashes, beaconRoot, requests, api.eth.BlockChain().Config())
	if err != nil {
		bgu := "nil"
		if params.BlobGasUsed != nil {
			bgu = strconv.Itoa(int(*params.BlobGasUsed))
		}
		ebg := "nil"
		if params.ExcessBlobGas != nil {
			ebg = strconv.Itoa(int(*params.ExcessBlobGas))
		}
		log.Warn("Invalid NewPayload params",
			"params.Number", params.Number,
			"params.ParentHash", params.ParentHash,
			"params.BlockHash", params.BlockHash,
			"params.StateRoot", params.StateRoot,
			"params.FeeRecipient", params.FeeRecipient,
			"params.LogsBloom", common.PrettyBytes(params.LogsBloom),
			"params.Random", params.Random,
			"params.GasLimit", params.GasLimit,
			"params.GasUsed", params.GasUsed,
			"params.Timestamp", params.Timestamp,
			"params.ExtraData", common.PrettyBytes(params.ExtraData),
			"params.BaseFeePerGas", params.BaseFeePerGas,
			"params.BlobGasUsed", bgu,
			"params.ExcessBlobGas", ebg,
			"len(params.Transactions)", len(params.Transactions),
			"len(params.Withdrawals)", len(params.Withdrawals),
			"params.WithdrawalsRoot", params.WithdrawalsRoot,
			"beaconRoot", beaconRoot,
			"len(requests)", len(requests),
			"error", err)
		return api.invalid(err, nil), nil
	}
	// Stash away the last update to warn the user if the beacon client goes offline
	api.lastNewPayloadLock.Lock()
	api.lastNewPayloadUpdate = time.Now()
	api.lastNewPayloadLock.Unlock()

	// If we already have the block locally, ignore the entire execution and just
	// return a fake success.
	if block := api.eth.BlockChain().GetBlockByHash(params.BlockHash); block != nil {
		log.Warn("Ignoring already known beacon payload", "number", params.Number, "hash", params.BlockHash, "age", common.PrettyAge(time.Unix(int64(block.Time()), 0)))
		hash := block.Hash()
		return engine.PayloadStatusV1{Status: engine.VALID, LatestValidHash: &hash}, nil
	}
	// If this block was rejected previously, keep rejecting it
	if res := api.checkInvalidAncestor(block.Hash(), block.Hash()); res != nil {
		return *res, nil
	}
	// If the parent is missing, we - in theory - could trigger a sync, but that
	// would also entail a reorg. That is problematic if multiple sibling blocks
	// are being fed to us, and even more so, if some semi-distant uncle shortens
	// our live chain. As such, payload execution will not permit reorgs and thus
	// will not trigger a sync cycle. That is fine though, if we get a fork choice
	// update after legit payload executions.
	parent := api.eth.BlockChain().GetBlock(block.ParentHash(), block.NumberU64()-1)
	if parent == nil {
		return api.delayPayloadImport(block), nil
	}
	if block.Time() <= parent.Time() {
		log.Warn("Invalid timestamp", "parent", block.Time(), "block", block.Time())
		return api.invalid(errors.New("invalid timestamp"), parent.Header()), nil
	}
	// Another corner case: if the node is in snap sync mode, but the CL client
	// tries to make it import a block. That should be denied as pushing something
	// into the database directly will conflict with the assumptions of snap sync
	// that it has an empty db that it can fill itself.
	if api.eth.SyncMode() != ethconfig.FullSync {
		return api.delayPayloadImport(block), nil
	}
	if !api.eth.BlockChain().HasBlockAndState(block.ParentHash(), block.NumberU64()-1) {
		api.remoteBlocks.put(block.Hash(), block.Header())
		log.Warn("State not available, ignoring new payload")
		return engine.PayloadStatusV1{Status: engine.ACCEPTED}, nil
	}
	log.Trace("Inserting block without sethead", "hash", block.Hash(), "number", block.Number())
	proofs, err := api.eth.BlockChain().InsertBlockWithoutSetHead(block, witness)
	if err != nil {
		log.Warn("NewPayload: inserting block failed", "error", err)

		api.invalidLock.Lock()
		api.invalidBlocksHits[block.Hash()] = 1
		api.invalidTipsets[block.Hash()] = block.Header()
		api.invalidLock.Unlock()

		return api.invalid(err, parent.Header()), nil
	}
	hash := block.Hash()

	// If witness collection was requested, inject that into the result too
	var ow *hexutil.Bytes
	if proofs != nil {
		ow = new(hexutil.Bytes)
		*ow, _ = rlp.EncodeToBytes(proofs)
	}
	return engine.PayloadStatusV1{Status: engine.VALID, Witness: ow, LatestValidHash: &hash}, nil
}

func (api *ConsensusAPI) executeStatelessPayload(params engine.ExecutableData, versionedHashes []common.Hash, beaconRoot *common.Hash, requests [][]byte, opaqueWitness hexutil.Bytes) (engine.StatelessPayloadStatusV1, error) {
	log.Trace("Engine API request received", "method", "ExecuteStatelessPayload", "number", params.Number, "hash", params.BlockHash)
	block, err := engine.ExecutableDataToBlockNoHash(params, versionedHashes, beaconRoot, requests, api.eth.BlockChain().Config())
	if err != nil {
		bgu := "nil"
		if params.BlobGasUsed != nil {
			bgu = strconv.Itoa(int(*params.BlobGasUsed))
		}
		ebg := "nil"
		if params.ExcessBlobGas != nil {
			ebg = strconv.Itoa(int(*params.ExcessBlobGas))
		}
		log.Warn("Invalid ExecuteStatelessPayload params",
			"params.Number", params.Number,
			"params.ParentHash", params.ParentHash,
			"params.BlockHash", params.BlockHash,
			"params.StateRoot", params.StateRoot,
			"params.FeeRecipient", params.FeeRecipient,
			"params.LogsBloom", common.PrettyBytes(params.LogsBloom),
			"params.Random", params.Random,
			"params.GasLimit", params.GasLimit,
			"params.GasUsed", params.GasUsed,
			"params.Timestamp", params.Timestamp,
			"params.ExtraData", common.PrettyBytes(params.ExtraData),
			"params.BaseFeePerGas", params.BaseFeePerGas,
			"params.BlobGasUsed", bgu,
			"params.ExcessBlobGas", ebg,
			"len(params.Transactions)", len(params.Transactions),
			"len(params.Withdrawals)", len(params.Withdrawals),
			"params.WithdrawalsRoot", params.WithdrawalsRoot,
			"beaconRoot", beaconRoot,
			"len(requests)", len(requests),
			"error", err)
		errorMsg := err.Error()
		return engine.StatelessPayloadStatusV1{Status: engine.INVALID, ValidationError: &errorMsg}, nil
	}
	witness := new(stateless.Witness)
	if err := rlp.DecodeBytes(opaqueWitness, witness); err != nil {
		log.Warn("Invalid ExecuteStatelessPayload witness", "err", err)
		errorMsg := err.Error()
		return engine.StatelessPayloadStatusV1{Status: engine.INVALID, ValidationError: &errorMsg}, nil
	}
	// Stash away the last update to warn the user if the beacon client goes offline
	api.lastNewPayloadLock.Lock()
	api.lastNewPayloadUpdate = time.Now()
	api.lastNewPayloadLock.Unlock()

	log.Trace("Executing block statelessly", "number", block.Number(), "hash", params.BlockHash)
	stateRoot, receiptRoot, err := core.ExecuteStateless(api.eth.BlockChain().Config(), vm.Config{}, block, witness)
	if err != nil {
		log.Warn("ExecuteStatelessPayload: execution failed", "err", err)
		errorMsg := err.Error()
		return engine.StatelessPayloadStatusV1{Status: engine.INVALID, ValidationError: &errorMsg}, nil
	}
	return engine.StatelessPayloadStatusV1{Status: engine.VALID, StateRoot: stateRoot, ReceiptsRoot: receiptRoot}, nil
}

// delayPayloadImport stashes the given block away for import at a later time,
// either via a forkchoice update or a sync extension. This method is meant to
// be called by the newpayload command when the block seems to be ok, but some
// prerequisite prevents it from being processed (e.g. no parent, or snap sync).
func (api *ConsensusAPI) delayPayloadImport(block *types.Block) engine.PayloadStatusV1 {
	// Sanity check that this block's parent is not on a previously invalidated
	// chain. If it is, mark the block as invalid too.
	if res := api.checkInvalidAncestor(block.ParentHash(), block.Hash()); res != nil {
		return *res
	}
	// Stash the block away for a potential forced forkchoice update to it
	// at a later time.
	api.remoteBlocks.put(block.Hash(), block.Header())

	// Although we don't want to trigger a sync, if there is one already in
	// progress, try to extend it with the current payload request to relieve
	// some strain from the forkchoice update.
	err := api.eth.Downloader().BeaconExtend(api.eth.SyncMode(), block.Header())
	if err == nil {
		log.Debug("Payload accepted for sync extension", "number", block.NumberU64(), "hash", block.Hash())
		return engine.PayloadStatusV1{Status: engine.SYNCING}
	}
	// Either no beacon sync was started yet, or it rejected the delivered
	// payload as non-integratable on top of the existing sync. We'll just
	// have to rely on the beacon client to forcefully update the head with
	// a forkchoice update request.
	if api.eth.SyncMode() == ethconfig.FullSync {
		// In full sync mode, failure to import a well-formed block can only mean
		// that the parent state is missing and the syncer rejected extending the
		// current cycle with the new payload.
		log.Warn("Ignoring payload with missing parent", "number", block.NumberU64(), "hash", block.Hash(), "parent", block.ParentHash(), "reason", err)
	} else {
		// In non-full sync mode (i.e. snap sync) all payloads are rejected until
		// snap sync terminates as snap sync relies on direct database injections
		// and cannot afford concurrent out-if-band modifications via imports.
		log.Warn("Ignoring payload while snap syncing", "number", block.NumberU64(), "hash", block.Hash(), "reason", err)
	}
	return engine.PayloadStatusV1{Status: engine.SYNCING}
}

// setInvalidAncestor is a callback for the downloader to notify us if a bad block
// is encountered during the async sync.
func (api *ConsensusAPI) setInvalidAncestor(invalid *types.Header, origin *types.Header) {
	api.invalidLock.Lock()
	defer api.invalidLock.Unlock()

	api.invalidTipsets[origin.Hash()] = invalid
	api.invalidBlocksHits[invalid.Hash()]++
}

// checkInvalidAncestor checks whether the specified chain end links to a known
// bad ancestor. If yes, it constructs the payload failure response to return.
func (api *ConsensusAPI) checkInvalidAncestor(check common.Hash, head common.Hash) *engine.PayloadStatusV1 {
	api.invalidLock.Lock()
	defer api.invalidLock.Unlock()

	// If the hash to check is unknown, return valid
	invalid, ok := api.invalidTipsets[check]
	if !ok {
		return nil
	}
	// If the bad hash was hit too many times, evict it and try to reprocess in
	// the hopes that we have a data race that we can exit out of.
	badHash := invalid.Hash()

	api.invalidBlocksHits[badHash]++
	if api.invalidBlocksHits[badHash] >= invalidBlockHitEviction {
		log.Warn("Too many bad block import attempt, trying", "number", invalid.Number, "hash", badHash)
		delete(api.invalidBlocksHits, badHash)

		for descendant, badHeader := range api.invalidTipsets {
			if badHeader.Hash() == badHash {
				delete(api.invalidTipsets, descendant)
			}
		}
		return nil
	}
	// Not too many failures yet, mark the head of the invalid chain as invalid
	if check != head {
		log.Warn("Marked new chain head as invalid", "hash", head, "badnumber", invalid.Number, "badhash", badHash)
		for len(api.invalidTipsets) >= invalidTipsetsCap {
			for key := range api.invalidTipsets {
				delete(api.invalidTipsets, key)
				break
			}
		}
		api.invalidTipsets[head] = invalid
	}
	// If the last valid hash is the terminal pow block, return 0x0 for latest valid hash
	lastValid := &invalid.ParentHash
	if header := api.eth.BlockChain().GetHeader(invalid.ParentHash, invalid.Number.Uint64()-1); header != nil && header.Difficulty.Sign() != 0 {
		lastValid = &common.Hash{}
	}
	failure := "links to previously rejected block"
	return &engine.PayloadStatusV1{
		Status:          engine.INVALID,
		LatestValidHash: lastValid,
		ValidationError: &failure,
	}
}

// invalid returns a response "INVALID" with the latest valid hash supplied by latest.
func (api *ConsensusAPI) invalid(err error, latestValid *types.Header) engine.PayloadStatusV1 {
	var currentHash *common.Hash
	if latestValid != nil {
		if latestValid.Difficulty.BitLen() != 0 {
			// Set latest valid hash to 0x0 if parent is PoW block
			currentHash = &common.Hash{}
		} else {
			// Otherwise set latest valid hash to parent hash
			h := latestValid.Hash()
			currentHash = &h
		}
	}
	errorMsg := err.Error()
	return engine.PayloadStatusV1{Status: engine.INVALID, LatestValidHash: currentHash, ValidationError: &errorMsg}
}

// heartbeat loops indefinitely, and checks if there have been beacon client updates
// received in the last while. If not - or if they but strange ones - it warns the
// user that something might be off with their consensus node.
//
// TODO(karalabe): Spin this goroutine down somehow
func (api *ConsensusAPI) heartbeat() {
	if api.eth.BlockChain().Config().Optimism != nil { // don't start the api heartbeat, there is no transition
		return
	}
	// Sleep a bit on startup since there's obviously no beacon client yet
	// attached, so no need to print scary warnings to the user.
	time.Sleep(beaconUpdateStartupTimeout)

	// If the network is not yet merged/merging, don't bother continuing.
	if api.eth.BlockChain().Config().TerminalTotalDifficulty == nil {
		return
	}

	var offlineLogged time.Time

	for {
		// Sleep a bit and retrieve the last known consensus updates
		time.Sleep(5 * time.Second)

		api.lastTransitionLock.Lock()
		lastTransitionUpdate := api.lastTransitionUpdate
		api.lastTransitionLock.Unlock()

		api.lastForkchoiceLock.Lock()
		lastForkchoiceUpdate := api.lastForkchoiceUpdate
		api.lastForkchoiceLock.Unlock()

		api.lastNewPayloadLock.Lock()
		lastNewPayloadUpdate := api.lastNewPayloadUpdate
		api.lastNewPayloadLock.Unlock()

		// If there have been no updates for the past while, warn the user
		// that the beacon client is probably offline
		if time.Since(lastForkchoiceUpdate) <= beaconUpdateConsensusTimeout || time.Since(lastNewPayloadUpdate) <= beaconUpdateConsensusTimeout {
			offlineLogged = time.Time{}
			continue
		}
		if time.Since(offlineLogged) > beaconUpdateWarnFrequency {
			if lastForkchoiceUpdate.IsZero() && lastNewPayloadUpdate.IsZero() {
				if lastTransitionUpdate.IsZero() {
					log.Warn("Post-merge network, but no beacon client seen. Please launch one to follow the chain!")
				} else {
					log.Warn("Beacon client online, but never received consensus updates. Please ensure your beacon client is operational to follow the chain!")
				}
			} else {
				log.Warn("Beacon client online, but no consensus updates received in a while. Please fix your beacon client to follow the chain!")
			}
			offlineLogged = time.Now()
		}
		continue
	}
}

// ExchangeCapabilities returns the current methods provided by this node.
func (api *ConsensusAPI) ExchangeCapabilities([]string) []string {
	return caps
}

// GetClientVersionV1 exchanges client version data of this node.
func (api *ConsensusAPI) GetClientVersionV1(info engine.ClientVersionV1) []engine.ClientVersionV1 {
	log.Trace("Engine API request received", "method", "GetClientVersionV1", "info", info.String())
	commit := make([]byte, 4)
	if vcs, ok := version.VCS(); ok {
		commit = common.FromHex(vcs.Commit)[0:4]
	}
	return []engine.ClientVersionV1{
		{
			Code:    engine.ClientCode,
			Name:    engine.ClientName,
			Version: version.WithMeta,
			Commit:  hexutil.Encode(commit),
		},
	}
}

// GetPayloadBodiesByHashV1 implements engine_getPayloadBodiesByHashV1 which allows for retrieval of a list
// of block bodies by the engine api.
func (api *ConsensusAPI) GetPayloadBodiesByHashV1(hashes []common.Hash) []*engine.ExecutionPayloadBody {
	bodies := make([]*engine.ExecutionPayloadBody, len(hashes))
	for i, hash := range hashes {
		block := api.eth.BlockChain().GetBlockByHash(hash)
		bodies[i] = getBody(block)
	}
	return bodies
}

// GetPayloadBodiesByHashV2 implements engine_getPayloadBodiesByHashV1 which allows for retrieval of a list
// of block bodies by the engine api.
func (api *ConsensusAPI) GetPayloadBodiesByHashV2(hashes []common.Hash) []*engine.ExecutionPayloadBody {
	bodies := make([]*engine.ExecutionPayloadBody, len(hashes))
	for i, hash := range hashes {
		block := api.eth.BlockChain().GetBlockByHash(hash)
		bodies[i] = getBody(block)
	}
	return bodies
}

// GetPayloadBodiesByRangeV1 implements engine_getPayloadBodiesByRangeV1 which allows for retrieval of a range
// of block bodies by the engine api.
func (api *ConsensusAPI) GetPayloadBodiesByRangeV1(start, count hexutil.Uint64) ([]*engine.ExecutionPayloadBody, error) {
	return api.getBodiesByRange(start, count)
}

// GetPayloadBodiesByRangeV2 implements engine_getPayloadBodiesByRangeV1 which allows for retrieval of a range
// of block bodies by the engine api.
func (api *ConsensusAPI) GetPayloadBodiesByRangeV2(start, count hexutil.Uint64) ([]*engine.ExecutionPayloadBody, error) {
	return api.getBodiesByRange(start, count)
}

func (api *ConsensusAPI) getBodiesByRange(start, count hexutil.Uint64) ([]*engine.ExecutionPayloadBody, error) {
	if start == 0 || count == 0 {
		return nil, engine.InvalidParams.With(fmt.Errorf("invalid start or count, start: %v count: %v", start, count))
	}
	if count > 1024 {
		return nil, engine.TooLargeRequest.With(fmt.Errorf("requested count too large: %v", count))
	}
	// limit count up until current
	current := api.eth.BlockChain().CurrentBlock().Number.Uint64()
	last := uint64(start) + uint64(count) - 1
	if last > current {
		last = current
	}
	bodies := make([]*engine.ExecutionPayloadBody, 0, uint64(count))
	for i := uint64(start); i <= last; i++ {
		block := api.eth.BlockChain().GetBlockByNumber(i)
		bodies = append(bodies, getBody(block))
	}
	return bodies, nil
}

func getBody(block *types.Block) *engine.ExecutionPayloadBody {
	if block == nil {
		return nil
	}

	var result engine.ExecutionPayloadBody

	result.TransactionData = make([]hexutil.Bytes, len(block.Transactions()))
	for j, tx := range block.Transactions() {
		result.TransactionData[j], _ = tx.MarshalBinary()
	}

	// Post-shanghai withdrawals MUST be set to empty slice instead of nil
	result.Withdrawals = block.Withdrawals()
	if block.Withdrawals() == nil && block.Header().WithdrawalsHash != nil {
		result.Withdrawals = []*types.Withdrawal{}
	}

	return &result
}

// convertRequests converts a hex requests slice to plain [][]byte.
func convertRequests(hex []hexutil.Bytes) [][]byte {
	if hex == nil {
		return nil
	}
	req := make([][]byte, len(hex))
	for i := range hex {
		req[i] = hex[i]
	}
	return req
}

// validateRequests checks that requests are ordered by their type and are not empty.
func validateRequests(requests [][]byte) error {
	for i, req := range requests {
		// No empty requests.
		if len(req) < 2 {
			return fmt.Errorf("empty request: %v", req)
		}
		// Check that requests are ordered by their type.
		// Each type must appear only once.
		if i > 0 && req[0] <= requests[i-1][0] {
			return fmt.Errorf("invalid request order: %v", req)
		}
	}
	return nil
}
