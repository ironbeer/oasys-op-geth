// Copyright 2023 The go-ethereum Authors
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

package txpool

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
	"github.com/ethereum/go-ethereum/consensus/misc/eip4844"
	"github.com/ethereum/go-ethereum/core"
	"github.com/ethereum/go-ethereum/core/state"
	"github.com/ethereum/go-ethereum/core/types"
	"github.com/ethereum/go-ethereum/crypto/kzg4844"
	"github.com/ethereum/go-ethereum/log"
	"github.com/ethereum/go-ethereum/params"
)

// L1 Info Gas Overhead is the amount of gas the the L1 info deposit consumes.
// It is removed from the tx pool max gas to better indicate that L2 transactions
// are not able to consume all of the gas in a L2 block as the L1 info deposit is always present.
const l1InfoGasOverhead = uint64(70_000)

var (
	// blobTxMinBlobGasPrice is the big.Int version of the configured protocol
	// parameter to avoid constructing a new big integer for every transaction.
	blobTxMinBlobGasPrice = big.NewInt(params.BlobTxMinBlobGasprice)
)

func EffectiveGasLimit(chainConfig *params.ChainConfig, gasLimit uint64, effectiveLimit uint64) uint64 {
	if effectiveLimit != 0 && effectiveLimit < gasLimit {
		gasLimit = effectiveLimit
	}
	if chainConfig.Optimism != nil {
		if l1InfoGasOverhead < gasLimit {
			gasLimit -= l1InfoGasOverhead
		} else {
			gasLimit = 0
		}
	}
	return gasLimit
}

// ValidationOptions define certain differences between transaction validation
// across the different pools without having to duplicate those checks.
type ValidationOptions struct {
	Config *params.ChainConfig // Chain configuration to selectively validate based on current fork rules

	Accept  uint8    // Bitmap of transaction types that should be accepted for the calling pool
	MaxSize uint64   // Maximum size of a transaction that the caller can meaningfully handle
	MinTip  *big.Int // Minimum gas tip needed to allow a transaction into the caller pool

	EffectiveGasCeil uint64 // if non-zero, a gas ceiling to enforce independent of the header's gaslimit value
}

// ValidationFunction is an method type which the pools use to perform the tx-validations which do not
// require state access. Production code typically uses ValidateTransaction, whereas testing-code
// might choose to instead use something else, e.g. to always fail or avoid heavy cpu usage.
type ValidationFunction func(tx *types.Transaction, head *types.Header, signer types.Signer, opts *ValidationOptions) error

// ValidateTransaction is a helper method to check whether a transaction is valid
// according to the consensus rules, but does not check state-dependent validation
// (balance, nonce, etc).
//
// This check is public to allow different transaction pools to check the basic
// rules without duplicating code and running the risk of missed updates.
func ValidateTransaction(tx *types.Transaction, head *types.Header, signer types.Signer, opts *ValidationOptions) error {
	// No unauthenticated deposits allowed in the transaction pool.
	// This is for spam protection, not consensus,
	// as the external engine-API user authenticates deposits.
	if tx.Type() == types.DepositTxType {
		return core.ErrTxTypeNotSupported
	}
	if opts.Config.IsOptimism() && tx.Type() == types.BlobTxType {
		return core.ErrTxTypeNotSupported
	}
	// Ensure transactions not implemented by the calling pool are rejected
	if opts.Accept&(1<<tx.Type()) == 0 {
		return fmt.Errorf("%w: tx type %v not supported by this pool", core.ErrTxTypeNotSupported, tx.Type())
	}
	// Before performing any expensive validations, sanity check that the tx is
	// smaller than the maximum limit the pool can meaningfully handle
	if tx.Size() > opts.MaxSize {
		return fmt.Errorf("%w: transaction size %v, limit %v", ErrOversizedData, tx.Size(), opts.MaxSize)
	}
	// Ensure only transactions that have been enabled are accepted
	rules := opts.Config.Rules(head.Number, head.Difficulty.Sign() == 0, head.Time)
	if !rules.IsBerlin && tx.Type() != types.LegacyTxType {
		return fmt.Errorf("%w: type %d rejected, pool not yet in Berlin", core.ErrTxTypeNotSupported, tx.Type())
	}
	if !rules.IsLondon && tx.Type() == types.DynamicFeeTxType {
		return fmt.Errorf("%w: type %d rejected, pool not yet in London", core.ErrTxTypeNotSupported, tx.Type())
	}
	if !rules.IsCancun && tx.Type() == types.BlobTxType {
		return fmt.Errorf("%w: type %d rejected, pool not yet in Cancun", core.ErrTxTypeNotSupported, tx.Type())
	}
	if !rules.IsPrague && tx.Type() == types.SetCodeTxType {
		return fmt.Errorf("%w: type %d rejected, pool not yet in Prague", core.ErrTxTypeNotSupported, tx.Type())
	}
	// Check whether the init code size has been exceeded
	if rules.IsShanghai && tx.To() == nil && len(tx.Data()) > params.MaxInitCodeSize {
		return fmt.Errorf("%w: code size %v, limit %v", core.ErrMaxInitCodeSizeExceeded, len(tx.Data()), params.MaxInitCodeSize)
	}
	// Transactions can't be negative. This may never happen using RLP decoded
	// transactions but may occur for transactions created using the RPC.
	if tx.Value().Sign() < 0 {
		return ErrNegativeValue
	}
	// Ensure the transaction doesn't exceed the current block limit gas
	if EffectiveGasLimit(opts.Config, head.GasLimit, opts.EffectiveGasCeil) < tx.Gas() {
		return ErrGasLimit
	}
	// Sanity check for extremely large numbers (supported by RLP or RPC)
	if tx.GasFeeCap().BitLen() > 256 {
		return core.ErrFeeCapVeryHigh
	}
	if tx.GasTipCap().BitLen() > 256 {
		return core.ErrTipVeryHigh
	}
	// Ensure gasFeeCap is greater than or equal to gasTipCap
	if tx.GasFeeCapIntCmp(tx.GasTipCap()) < 0 {
		return core.ErrTipAboveFeeCap
	}
	// Make sure the transaction is signed properly
	if _, err := types.Sender(signer, tx); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidSender, err)
	}
	// Ensure the transaction has more gas than the bare minimum needed to cover
	// the transaction metadata
	intrGas, err := core.IntrinsicGas(tx.Data(), tx.AccessList(), tx.SetCodeAuthorizations(), tx.To() == nil, true, rules.IsIstanbul, rules.IsShanghai)
	if err != nil {
		return err
	}
	if tx.Gas() < intrGas {
		return fmt.Errorf("%w: gas %v, minimum needed %v", core.ErrIntrinsicGas, tx.Gas(), intrGas)
	}
	// Ensure the transaction can cover floor data gas.
	if opts.Config.IsPrague(head.Number, head.Time) {
		floorDataGas, err := core.FloorDataGas(tx.Data())
		if err != nil {
			return err
		}
		if tx.Gas() < floorDataGas {
			return fmt.Errorf("%w: gas %v, minimum needed %v", core.ErrFloorDataGas, tx.Gas(), floorDataGas)
		}
	}
	// Ensure the gasprice is high enough to cover the requirement of the calling pool
	if tx.GasTipCapIntCmp(opts.MinTip) < 0 {
		return fmt.Errorf("%w: gas tip cap %v, minimum needed %v", ErrUnderpriced, tx.GasTipCap(), opts.MinTip)
	}
	if tx.Type() == types.BlobTxType {
		// Ensure the blob fee cap satisfies the minimum blob gas price
		if tx.BlobGasFeeCapIntCmp(blobTxMinBlobGasPrice) < 0 {
			return fmt.Errorf("%w: blob fee cap %v, minimum needed %v", ErrUnderpriced, tx.BlobGasFeeCap(), blobTxMinBlobGasPrice)
		}
		sidecar := tx.BlobTxSidecar()
		if sidecar == nil {
			return errors.New("missing sidecar in blob transaction")
		}
		// Ensure the number of items in the blob transaction and various side
		// data match up before doing any expensive validations
		hashes := tx.BlobHashes()
		if len(hashes) == 0 {
			return errors.New("blobless blob transaction")
		}
		maxBlobs := eip4844.MaxBlobsPerBlock(opts.Config, head.Time)
		if len(hashes) > maxBlobs {
			return fmt.Errorf("too many blobs in transaction: have %d, permitted %d", len(hashes), maxBlobs)
		}
		// Ensure commitments, proofs and hashes are valid
		if err := validateBlobSidecar(hashes, sidecar); err != nil {
			return err
		}
	}
	if tx.Type() == types.SetCodeTxType {
		if len(tx.SetCodeAuthorizations()) == 0 {
			return fmt.Errorf("set code tx must have at least one authorization tuple")
		}
	}
	return nil
}

func validateBlobSidecar(hashes []common.Hash, sidecar *types.BlobTxSidecar) error {
	if len(sidecar.Blobs) != len(hashes) {
		return fmt.Errorf("invalid number of %d blobs compared to %d blob hashes", len(sidecar.Blobs), len(hashes))
	}
	if len(sidecar.Commitments) != len(hashes) {
		return fmt.Errorf("invalid number of %d blob commitments compared to %d blob hashes", len(sidecar.Commitments), len(hashes))
	}
	if len(sidecar.Proofs) != len(hashes) {
		return fmt.Errorf("invalid number of %d blob proofs compared to %d blob hashes", len(sidecar.Proofs), len(hashes))
	}
	// Blob quantities match up, validate that the provers match with the
	// transaction hash before getting to the cryptography
	hasher := sha256.New()
	for i, vhash := range hashes {
		computed := kzg4844.CalcBlobHashV1(hasher, &sidecar.Commitments[i])
		if vhash != computed {
			return fmt.Errorf("blob %d: computed hash %#x mismatches transaction one %#x", i, computed, vhash)
		}
	}
	// Blob commitments match with the hashes in the transaction, verify the
	// blobs themselves via KZG
	for i := range sidecar.Blobs {
		if err := kzg4844.VerifyBlobProof(&sidecar.Blobs[i], sidecar.Commitments[i], sidecar.Proofs[i]); err != nil {
			return fmt.Errorf("invalid blob %d: %v", i, err)
		}
	}
	return nil
}

// ValidationOptionsWithState define certain differences between stateful transaction
// validation across the different pools without having to duplicate those checks.
type ValidationOptionsWithState struct {
	State *state.StateDB // State database to check nonces and balances against

	// FirstNonceGap is an optional callback to retrieve the first nonce gap in
	// the list of pooled transactions of a specific account. If this method is
	// set, nonce gaps will be checked and forbidden. If this method is not set,
	// nonce gaps will be ignored and permitted.
	FirstNonceGap func(addr common.Address) uint64

	// UsedAndLeftSlots is an optional callback to retrieve the number of tx slots
	// used and the number still permitted for an account. New transactions will
	// be rejected once the number of remaining slots reaches zero.
	UsedAndLeftSlots func(addr common.Address) (int, int)

	// ExistingExpenditure is a mandatory callback to retrieve the cumulative
	// cost of the already pooled transactions to check for overdrafts.
	ExistingExpenditure func(addr common.Address) *big.Int

	// ExistingCost is a mandatory callback to retrieve an already pooled
	// transaction's cost with the given nonce to check for overdrafts.
	ExistingCost func(addr common.Address, nonce uint64) *big.Int

	// RollupCostFn is an optional extension, to validate total rollup costs of a tx
	RollupCostFn RollupCostFunc

	// Flag to indicate that the L2 fee is zero
	IsFeeZero bool
}

// ValidateTransactionWithState is a helper method to check whether a transaction
// is valid according to the pool's internal state checks (balance, nonce, gaps).
//
// This check is public to allow different transaction pools to check the stateful
// rules without duplicating code and running the risk of missed updates.
func ValidateTransactionWithState(tx *types.Transaction, signer types.Signer, opts *ValidationOptionsWithState) error {
	// Ensure the transaction adheres to nonce ordering
	from, err := types.Sender(signer, tx) // already validated (and cached), but cleaner to check
	if err != nil {
		log.Error("Transaction sender recovery failed", "err", err)
		return err
	}
	next := opts.State.GetNonce(from)
	if next > tx.Nonce() {
		return fmt.Errorf("%w: next nonce %v, tx nonce %v", core.ErrNonceTooLow, next, tx.Nonce())
	}
	// Ensure the transaction doesn't produce a nonce gap in pools that do not
	// support arbitrary orderings
	if opts.FirstNonceGap != nil {
		if gap := opts.FirstNonceGap(from); gap < tx.Nonce() {
			return fmt.Errorf("%w: tx nonce %v, gapped nonce %v", core.ErrNonceTooHigh, tx.Nonce(), gap)
		}
	}
	// Skip balance validation if zero fee
	if opts.IsFeeZero {
		return nil
	}
	// Ensure the transactor has enough funds to cover the transaction costs
	var (
		balance           = opts.State.GetBalance(from).ToBig()
		cost256, overflow = TotalTxCost(tx, opts.RollupCostFn)
	)
	if overflow {
		return fmt.Errorf("%w: total tx cost overflow", core.ErrInsufficientFunds)
	}
	cost := cost256.ToBig()
	if balance.Cmp(cost) < 0 {
		return fmt.Errorf("%w: balance %v, tx cost %v, overshot %v", core.ErrInsufficientFunds, balance, cost, new(big.Int).Sub(cost, balance))
	}
	// Ensure the transactor has enough funds to cover for replacements or nonce
	// expansions without overdrafts
	spent := opts.ExistingExpenditure(from)
	if prev := opts.ExistingCost(from, tx.Nonce()); prev != nil {
		bump := new(big.Int).Sub(cost, prev)
		need := new(big.Int).Add(spent, bump)
		if balance.Cmp(need) < 0 {
			return fmt.Errorf("%w: balance %v, queued cost %v, tx bumped %v, overshot %v", core.ErrInsufficientFunds, balance, spent, bump, new(big.Int).Sub(need, balance))
		}
	} else {
		need := new(big.Int).Add(spent, cost)
		if balance.Cmp(need) < 0 {
			return fmt.Errorf("%w: balance %v, queued cost %v, tx cost %v, overshot %v", core.ErrInsufficientFunds, balance, spent, cost, new(big.Int).Sub(need, balance))
		}
		// Transaction takes a new nonce value out of the pool. Ensure it doesn't
		// overflow the number of permitted transactions from a single account
		// (i.e. max cancellable via out-of-bound transaction).
		if opts.UsedAndLeftSlots != nil {
			if used, left := opts.UsedAndLeftSlots(from); left <= 0 {
				return fmt.Errorf("%w: pooled %d txs", ErrAccountLimitExceeded, used)
			}
		}
	}
	return nil
}
