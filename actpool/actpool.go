// Copyright (c) 2018 IoTeX
// This is an alpha (internal) release and is not suitable for production. This source code is provided 'as is' and no
// warranties are given as to title or non-infringement, merchantability or fitness for purpose and, to the extent
// permitted by law, all liability for your use of the code is disclaimed. This source code is governed by Apache
// License 2.0 that can be found in the LICENSE file.

package actpool

import (
	"fmt"
	"sync"

	"github.com/pkg/errors"

	"github.com/iotexproject/iotex-core/action"
	"github.com/iotexproject/iotex-core/blockchain"
	"github.com/iotexproject/iotex-core/config"
	"github.com/iotexproject/iotex-core/iotxaddress"
	"github.com/iotexproject/iotex-core/logger"
	"github.com/iotexproject/iotex-core/pkg/hash"
)

const (
	// TransferSizeLimit is the maximum size of transfer allowed
	TransferSizeLimit = 32 * 1024
	// VoteSizeLimit is the maximum size of vote allowed
	VoteSizeLimit = 278
	// ExecutionSizeLimit is the maximum size of execution allowed
	ExecutionSizeLimit = 32 * 1024
)

var (
	// ErrActPool indicates the error of actpool
	ErrActPool = errors.New("invalid actpool")
	// ErrGasHigherThanLimit indicates the error of gas value
	ErrGasHigherThanLimit = errors.New("invalid gas for action")
	// ErrInsufficientGas indicates the error of insufficient gas value for data storage
	ErrInsufficientGas = errors.New("insufficient intrinsic gas value")
	// ErrTransfer indicates the error of transfer
	ErrTransfer = errors.New("invalid transfer")
	// ErrNonce indicates the error of nonce
	ErrNonce = errors.New("invalid nonce")
	// ErrBalance indicates the error of balance
	ErrBalance = errors.New("invalid balance")
	// ErrVotee indicates the error of votee
	ErrVotee = errors.New("votee is not a candidate")
	// ErrHash indicates the error of action's hash
	ErrHash = errors.New("invalid hash")
)

// ActPool is the interface of actpool
type ActPool interface {
	// Reset resets actpool state
	Reset()
	// PickActs returns all currently accepted transfers and votes in actpool
	PickActs() ([]*action.Transfer, []*action.Vote, []*action.Execution, []action.Action)
	// AddTsf adds an transfer into the pool after passing validation
	AddTsf(tsf *action.Transfer) error
	// AddVote adds a vote into the pool after passing validation
	AddVote(vote *action.Vote) error
	// AddExecution adds an execution into the pool after passing validation
	AddExecution(execution *action.Execution) error
	// Add adds an action into the pool after passing validation
	Add(act action.Action) error
	// GetPendingNonce returns pending nonce in pool given an account address
	GetPendingNonce(addr string) (uint64, error)
	// GetUnconfirmedActs returns unconfirmed actions in pool given an account address
	GetUnconfirmedActs(addr string) []action.Action
	// GetActionByHash returns the pending action in pool given action's hash
	GetActionByHash(hash hash.Hash32B) (action.Action, error)
	// GetSize returns the act pool size
	GetSize() uint64
	// GetCapacity returns the act pool capacity
	GetCapacity() uint64
}

// ActionValidator is the interface of validating an action
type ActionValidator interface {
	Validate(action.Action) error
}

// actPool implements ActPool interface
type actPool struct {
	mutex       sync.RWMutex
	cfg         config.ActPool
	bc          blockchain.Blockchain
	accountActs map[string]ActQueue
	allActions  map[hash.Hash32B]action.Action
	validators  []ActionValidator
}

// NewActPool constructs a new actpool
func NewActPool(bc blockchain.Blockchain, cfg config.ActPool, validators ...ActionValidator) (ActPool, error) {
	if bc == nil {
		return nil, errors.New("Try to attach a nil blockchain")
	}
	ap := &actPool{
		cfg:         cfg,
		bc:          bc,
		accountActs: make(map[string]ActQueue),
		allActions:  make(map[hash.Hash32B]action.Action),
		validators:  validators,
	}
	return ap, nil
}

// Reset resets actpool state
// Step I: remove all the actions in actpool that have already been committed to block
// Step II: update pending balance of each account if it still exists in pool
// Step III: update queue's status in each account and remove invalid actions following queue's update
// Specifically, first reset the pending nonce based on confirmed nonce in order to prevent omitting reevaluation of
// unconfirmed but pending actions in pool after update of pending balance
// Then starting from the current confirmed nonce, iteratively update pending nonce if nonces are consecutive and pending
// balance is sufficient, and remove all the subsequent actions once the pending balance becomes insufficient
func (ap *actPool) Reset() {
	ap.mutex.Lock()
	defer ap.mutex.Unlock()

	// Remove confirmed actions in actpool
	ap.removeConfirmedActs()
	for from, queue := range ap.accountActs {
		// Reset pending balance for each account
		balance, err := ap.bc.Balance(from)
		if err != nil {
			logger.Error().Err(err).Msg("Error when resetting actpool state")
			return
		}
		queue.SetPendingBalance(balance)

		// Reset pending nonce and remove invalid actions for each account
		confirmedNonce, err := ap.bc.Nonce(from)
		if err != nil {
			logger.Error().Err(err).Msg("Error when resetting actpool state")
			return
		}
		pendingNonce := confirmedNonce + 1
		queue.SetStartNonce(pendingNonce)
		queue.SetPendingNonce(pendingNonce)
		ap.updateAccount(from)
	}
}

// PickActs returns all currently accepted transfers and votes for all accounts
func (ap *actPool) PickActs() ([]*action.Transfer, []*action.Vote, []*action.Execution, []action.Action) {
	ap.mutex.Lock()
	defer ap.mutex.Unlock()

	numActs := uint64(0)
	transfers := make([]*action.Transfer, 0)
	votes := make([]*action.Vote, 0)
	executions := make([]*action.Execution, 0)
	actions := make([]action.Action, 0)
	for _, queue := range ap.accountActs {
		for _, act := range queue.PendingActs() {
			switch act.(type) {
			case *action.Transfer:
				transfers = append(transfers, act.(*action.Transfer))
			case *action.Vote:
				votes = append(votes, act.(*action.Vote))
			case *action.Execution:
				executions = append(executions, act.(*action.Execution))

			default:
				actions = append(actions, act)
			}
			numActs++
			if ap.cfg.MaxNumActsToPick > 0 && numActs >= ap.cfg.MaxNumActsToPick {
				logger.Debug().
					Uint64("limit", ap.cfg.MaxNumActsToPick).
					Msg("reach the max number of actions to pick")
				return transfers, votes, executions, actions
			}
		}
	}
	return transfers, votes, executions, actions
}

// AddTsf inserts a new transfer into account queue if it passes validation
func (ap *actPool) AddTsf(tsf *action.Transfer) error {
	ap.mutex.Lock()
	defer ap.mutex.Unlock()

	hash := tsf.Hash()
	// Reject transfer if it already exists in pool
	if ap.allActions[hash] != nil {
		logger.Error().
			Hex("hash", hash[:]).
			Msg("Rejecting existed transfer")
		return fmt.Errorf("existed transfer: %x", hash)
	}
	// Reject transfer if it fails validation
	if err := ap.validateTsf(tsf); err != nil {
		logger.Error().
			Hex("hash", hash[:]).
			Err(err).
			Msg("Rejecting invalid transfer")
		return err
	}
	// Reject transfer if pool space is full
	if uint64(len(ap.allActions)) >= ap.cfg.MaxNumActsPerPool {
		logger.Warn().
			Hex("hash", hash[:]).
			Msg("Rejecting transfer due to insufficient space")
		return errors.Wrapf(ErrActPool, "insufficient space for transfer")
	}

	return ap.enqueueAction(tsf.Sender(), tsf, hash, tsf.Nonce())
}

// AddVote inserts a new vote into account queue if it passes validation
func (ap *actPool) AddVote(vote *action.Vote) error {
	ap.mutex.Lock()
	defer ap.mutex.Unlock()

	hash := vote.Hash()
	// Reject vote if it already exists in pool
	if ap.allActions[hash] != nil {
		logger.Error().
			Hex("hash", hash[:]).
			Msg("Rejecting existed vote")
		return fmt.Errorf("existed vote: %x", hash)
	}
	// Reject vote if it fails validation
	if err := ap.validateVote(vote); err != nil {
		logger.Error().
			Hex("hash", hash[:]).
			Err(err).
			Msg("Rejecting invalid vote")
		return err
	}
	// Reject vote if pool space is full
	if uint64(len(ap.allActions)) >= ap.cfg.MaxNumActsPerPool {
		logger.Warn().
			Hex("hash", hash[:]).
			Msg("Rejecting vote due to insufficient space")
		return errors.Wrapf(ErrActPool, "insufficient space for vote")
	}

	return ap.enqueueAction(vote.Voter(), vote, hash, vote.Nonce())
}

// AddExecution inserts a new execution into account queue if it passes validation
func (ap *actPool) AddExecution(exec *action.Execution) error {
	ap.mutex.Lock()
	defer ap.mutex.Unlock()
	hash := exec.Hash()
	// Reject execution if it already exists in pool
	if ap.allActions[hash] != nil {
		logger.Error().
			Hex("hash", hash[:]).
			Msg("Rejecting existed execution")
		return fmt.Errorf("existed execution: %x", hash)
	}
	// Reject transfer if it fails validation
	if err := ap.validateExecution(exec); err != nil {
		logger.Error().
			Hex("hash", hash[:]).
			Err(err).
			Msg("Rejecting invalid execution")
		return err
	}
	// Reject execution if pool space is full
	if uint64(len(ap.allActions)) >= ap.cfg.MaxNumActsPerPool {
		logger.Warn().
			Hex("hash", hash[:]).
			Msg("Rejecting execution due to insufficient space")
		return errors.Wrapf(ErrActPool, "insufficient space for execution")
	}

	return ap.enqueueAction(exec.Executor(), exec, hash, exec.Nonce())
}

func (ap *actPool) Add(act action.Action) error {
	ap.mutex.Lock()
	defer ap.mutex.Unlock()
	// Reject action if pool space is full
	if uint64(len(ap.allActions)) >= ap.cfg.MaxNumActsPerPool {
		return errors.Wrapf(ErrActPool, "insufficient space for execution")
	}
	hash := act.Hash()
	// Reject action if it already exists in pool
	if ap.allActions[hash] != nil {
		return fmt.Errorf("reject existing execution: %x", hash)
	}
	// Reject action if it's invalid
	for _, validator := range ap.validators {
		if err := validator.Validate(act); err != nil {
			return errors.Wrapf(err, "reject invalid execution: %x", hash)
		}
	}
	return ap.enqueueAction(act.SrcAddr(), act, hash, act.Nonce())
}

// GetPendingNonce returns pending nonce in pool or confirmed nonce given an account address
func (ap *actPool) GetPendingNonce(addr string) (uint64, error) {
	ap.mutex.Lock()
	defer ap.mutex.Unlock()

	if queue, ok := ap.accountActs[addr]; ok {
		return queue.PendingNonce(), nil
	}
	confirmedNonce, err := ap.bc.Nonce(addr)
	pendingNonce := confirmedNonce + 1
	return pendingNonce, err
}

// GetUnconfirmedActs returns unconfirmed actions in pool given an account address
func (ap *actPool) GetUnconfirmedActs(addr string) []action.Action {
	ap.mutex.Lock()
	defer ap.mutex.Unlock()

	if queue, ok := ap.accountActs[addr]; ok {
		return queue.AllActs()
	}
	return make([]action.Action, 0)
}

// GetActionByHash returns the pending action in pool given action's hash
func (ap *actPool) GetActionByHash(hash hash.Hash32B) (action.Action, error) {
	ap.mutex.RLock()
	defer ap.mutex.RUnlock()

	action, ok := ap.allActions[hash]
	if !ok {
		return nil, errors.Wrapf(ErrHash, "action hash %x does not exist in pool", hash)
	}
	return action, nil
}

// GetSize returns the act pool size
func (ap *actPool) GetSize() uint64 {
	ap.mutex.RLock()
	defer ap.mutex.RUnlock()

	return uint64(len(ap.allActions))
}

// GetCapacity returns the act pool capacity
func (ap *actPool) GetCapacity() uint64 {
	return ap.cfg.MaxNumActsPerPool
}

//======================================
// private functions
//======================================
// validateTsf checks whether a tranfer is valid
func (ap *actPool) validateTsf(tsf *action.Transfer) error {
	// Reject coinbase transfer
	if tsf.IsCoinbase() {
		logger.Error().Msg("Error when validating whether transfer is coinbase")
		return errors.Wrapf(ErrTransfer, "coinbase transfer")
	}
	// Reject oversized transfer
	if tsf.TotalSize() > TransferSizeLimit {
		logger.Error().Msg("Error when validating transfer's data size")
		return errors.Wrapf(ErrActPool, "oversized data")
	}
	// Reject over-gassed transfer
	if tsf.GasLimit() > action.GasLimit {
		logger.Error().Msg("Error when validating transfer's gas limit")
		return errors.Wrapf(ErrGasHigherThanLimit, "gas is higher than gas limit")
	}
	// Reject transfer of insufficient gas limit
	intrinsicGas, err := tsf.IntrinsicGas()
	if intrinsicGas > tsf.GasLimit() || err != nil {
		logger.Error().Msg("Error when validating transfer's gas limit")
		return errors.Wrapf(ErrInsufficientGas, "insufficient gas for transfer")
	}
	// Reject transfer of negative amount
	if tsf.Amount().Sign() < 0 {
		logger.Error().Msg("Error when validating transfer's amount")
		return errors.Wrapf(ErrBalance, "negative value")
	}

	// check if sender's address is valid
	if _, err := iotxaddress.GetPubkeyHash(tsf.Sender()); err != nil {
		logger.Error().Msg("Error when validating transfer sender's address")
		return errors.Wrapf(err, "error when validating sender's address %s", tsf.Sender())
	}
	// check if recipient's address is valid
	if _, err := iotxaddress.GetPubkeyHash(tsf.Recipient()); err != nil {
		logger.Error().Msg("Error when validating transfer recipient's address")
		return errors.Wrapf(err, "error when validating recipient's address %s", tsf.Recipient())
	}

	// Verify transfer using sender's public key
	if err := action.Verify(tsf); err != nil {
		logger.Error().Err(err).Msg("Error when validating transfer's signature")
		return errors.Wrapf(err, "failed to verify Transfer signature")
	}
	// Reject transfer if nonce is too low
	confirmedNonce, err := ap.bc.Nonce(tsf.Sender())
	if err != nil {
		logger.Error().Err(err).Msg("Error when validating transfer's nonce")
		return errors.Wrapf(err, "invalid nonce value")
	}
	pendingNonce := confirmedNonce + 1
	if pendingNonce > tsf.Nonce() {
		logger.Error().Msg("Error when validating transfer's nonce")
		return errors.Wrapf(ErrNonce, "nonce too low")
	}
	return nil
}

func (ap *actPool) validateExecution(exec *action.Execution) error {
	// Reject oversized exeuction
	if exec.TotalSize() > ExecutionSizeLimit {
		logger.Error().Msg("Error when validating execution's data size")
		return errors.Wrapf(ErrActPool, "oversized data")
	}
	// Reject over-gassed execution
	if exec.GasLimit() > action.GasLimit {
		logger.Error().Msg("Error when validating execution's gas limit")
		return errors.Wrapf(ErrGasHigherThanLimit, "gas is higher than gas limit")
	}
	// Reject execution of insufficient gas limit
	intrinsicGas, err := exec.IntrinsicGas()
	if intrinsicGas > exec.GasLimit() || err != nil {
		logger.Error().Msg("Error when validating execution's gas limit")
		return errors.Wrapf(ErrInsufficientGas, "insufficient gas for execution")
	}
	// Reject execution of negative amount
	if exec.Amount().Sign() < 0 {
		logger.Error().Msg("Error when validating execution's amount")
		return errors.Wrapf(ErrBalance, "negative value")
	}

	// check if executor's address is valid
	if _, err := iotxaddress.GetPubkeyHash(exec.Executor()); err != nil {
		logger.Error().Msg("Error when validating executor's address")
		return errors.Wrapf(err, "error when validating executor's address %s", exec.Executor())
	}
	// check if contract's address is valid
	if exec.Contract() != action.EmptyAddress {
		if _, err := iotxaddress.GetPubkeyHash(exec.Contract()); err != nil {
			logger.Error().Msg("Error when validating contract's address")
			return errors.Wrapf(err, "error when validating contract's address %s", exec.Contract())
		}
	}

	// Verify transfer using executor's public key
	if err := action.Verify(exec); err != nil {
		logger.Error().Err(err).Msg("Error when validating execution's signature")
		return errors.Wrapf(err, "failed to verify Execution signature")
	}
	// Reject transfer if nonce is too low
	confirmedNonce, err := ap.bc.Nonce(exec.Executor())
	if err != nil {
		logger.Error().Err(err).Msg("Error when validating execution's nonce")
		return errors.Wrapf(err, "invalid nonce value")
	}
	pendingNonce := confirmedNonce + 1
	if pendingNonce > exec.Nonce() {
		logger.Error().Msg("Error when validating execution's nonce")
		return errors.Wrapf(ErrNonce, "nonce too low")
	}
	return nil
}

// validateVote checks whether a vote is valid
func (ap *actPool) validateVote(vote *action.Vote) error {
	// Reject oversized vote
	if vote.TotalSize() > VoteSizeLimit {
		logger.Error().Msg("Error when validating vote's data size")
		return errors.Wrapf(ErrActPool, "oversized data")
	}
	// Reject over-gassed transfer
	if vote.GasLimit() > action.GasLimit {
		logger.Error().Msg("Error when validating vote's gas limit")
		return errors.Wrapf(ErrGasHigherThanLimit, "gas is higher than gas limit")
	}
	// Reject transfer of insufficient gas limit
	intrinsicGas, err := vote.IntrinsicGas()
	if intrinsicGas > vote.GasLimit() || err != nil {
		logger.Error().Msg("Error when validating vote's gas limit")
		return errors.Wrapf(ErrInsufficientGas, "insufficient gas for vote")
	}
	// check if voter's address is valid
	if _, err := iotxaddress.GetPubkeyHash(vote.Voter()); err != nil {
		logger.Error().Err(err).Msg("Error when validating voter's address")
		return errors.Wrapf(err, "error when validating voter's address %s", vote.Voter())
	}
	// check if votee's address is valid
	if vote.Votee() != action.EmptyAddress {
		if _, err := iotxaddress.GetPubkeyHash(vote.Votee()); err != nil {
			logger.Error().Err(err).Msg("Error when validating votee's address")
			return errors.Wrapf(err, "error when validating votee's address %s", vote.Votee())
		}
	}

	// Verify vote using voter's public key
	if err := action.Verify(vote); err != nil {
		logger.Error().Err(err).Msg("Error when validating vote's signature")
		return errors.Wrapf(err, "failed to verify vote signature")
	}

	// Reject vote if nonce is too low
	confirmedNonce, err := ap.bc.Nonce(vote.Voter())
	if err != nil {
		logger.Error().Err(err).Msg("Error when validating vote's nonce")
		return errors.Wrapf(err, "invalid nonce value")
	}

	if vote.Votee() != "" {
		// Reject vote if votee is not a candidate
		voteeState, err := ap.bc.StateByAddr(vote.Votee())
		if err != nil {
			logger.Error().
				Err(err).
				Str("voter", vote.Voter()).
				Str("votee", vote.Votee()).
				Msg("Error when validating votee's state")
			return errors.Wrapf(err, "cannot find votee's state: %s", vote.Votee())
		}
		if vote.Voter() != vote.Votee() && !voteeState.IsCandidate {
			logger.Error().
				Err(ErrVotee).
				Str("voter", vote.Voter()).
				Str("votee", vote.Votee()).
				Msg("Error when validating votee's state")
			return errors.Wrapf(ErrVotee, "votee has not self-nominated: %s", vote.Votee())
		}
	}

	pendingNonce := confirmedNonce + 1
	if pendingNonce > vote.Nonce() {
		logger.Error().Msg("Error when validating vote's nonce")
		return errors.Wrapf(ErrNonce, "nonce too low")
	}
	return nil
}

func (ap *actPool) enqueueAction(sender string, act action.Action, hash hash.Hash32B, actNonce uint64) error {
	queue := ap.accountActs[sender]
	if queue == nil {
		queue = NewActQueue()
		ap.accountActs[sender] = queue
		confirmedNonce, err := ap.bc.Nonce(sender)
		if err != nil {
			logger.Error().Err(err).Msg("Error when adding action")
			return err
		}
		// Initialize pending nonce for new account
		pendingNonce := confirmedNonce + 1
		queue.SetPendingNonce(pendingNonce)
		queue.SetStartNonce(pendingNonce)
		// Initialize balance for new account
		balance, err := ap.bc.Balance(sender)
		if err != nil {
			logger.Error().Err(err).Msg("Error when adding action")
			return err
		}
		queue.SetPendingBalance(balance)
	}
	if queue.Overlaps(act) {
		// Nonce already exists
		logger.Error().
			Hex("hash", hash[:]).
			Msg("Rejecting action because replacement action is not supported")
		return errors.Wrapf(ErrNonce, "duplicate nonce")
	}

	if actNonce-queue.StartNonce() >= ap.cfg.MaxNumActsPerAcct {
		// Nonce exceeds current range
		logger.Debug().
			Hex("hash", hash[:]).
			Uint64("startNonce", queue.StartNonce()).Uint64("actNonce", actNonce).
			Msg("Rejecting action because nonce is too large")
		return errors.Wrapf(ErrNonce, "nonce too large")
	}

	switch act.(type) {
	case *action.Transfer:
		cost, err := act.Cost()
		if err != nil {
			logger.Error().Err(err).Msg("Error when adding action")
			return errors.Wrap(err, "failed to get cost of transfer")
		}
		if queue.PendingBalance().Cmp(cost) < 0 {
			// Pending balance is insufficient
			logger.Warn().
				Hex("hash", hash[:]).
				Msg("Rejecting transfer due to insufficient balance")
			return errors.Wrapf(ErrBalance, "insufficient balance for transfer")
		}
	case *action.Vote:
		cost, err := act.Cost()
		if err != nil {
			logger.Error().Err(err).Msg("Error when adding action")
			return errors.Wrap(err, "failed to get cost of vote")
		}
		if queue.PendingBalance().Cmp(cost) < 0 {
			logger.Warn().
				Hex("hash", hash[:]).
				Msg("Rejecting vote due to insufficient balance")
			return errors.Wrapf(ErrBalance, "insufficient balance for vote")
		}
	case *action.Execution:
		cost, _ := act.Cost()
		if queue.PendingBalance().Cmp(cost) < 0 {
			logger.Warn().
				Hex("hash", hash[:]).
				Msg("Rejecting execution due to insufficient balance")
			return errors.Wrapf(ErrBalance, "insufficient balance for execution")
		}
	}

	err := queue.Put(act)
	if err != nil {
		logger.Warn().
			Hex("hash", hash[:]).
			Err(err).
			Msg("cannot put act into ActQueue")
		return errors.Wrap(err, "cannot put act into ActQueue")
	}
	ap.allActions[hash] = act
	// If the pending nonce equals this nonce, update queue
	nonce := queue.PendingNonce()
	if actNonce == nonce {
		ap.updateAccount(sender)
	}
	return nil
}

// removeConfirmedActs removes processed (committed to block) actions from pool
func (ap *actPool) removeConfirmedActs() {
	for from, queue := range ap.accountActs {
		confirmedNonce, err := ap.bc.Nonce(from)
		if err != nil {
			logger.Error().Err(err).Msg("Error when removing confirmed actions")
			return
		}
		pendingNonce := confirmedNonce + 1
		// Remove all actions that are committed to new block
		acts := queue.FilterNonce(pendingNonce)
		ap.removeInvalidActs(acts)

		// Delete the queue entry if it becomes empty
		if queue.Empty() {
			delete(ap.accountActs, from)
		}
	}
}

func (ap *actPool) removeInvalidActs(acts []action.Action) {
	for _, act := range acts {
		hash := act.Hash()
		logger.Debug().
			Hex("hash", hash[:]).
			Msg("Removed invalidated action")
		delete(ap.allActions, hash)
	}
}

// updateAccount updates queue's status and remove invalidated actions from pool if necessary
func (ap *actPool) updateAccount(sender string) {
	queue := ap.accountActs[sender]
	acts := queue.UpdateQueue(queue.PendingNonce())
	if len(acts) > 0 {
		ap.removeInvalidActs(acts)
	}

	// Delete the queue entry if it becomes empty
	if queue.Empty() {
		delete(ap.accountActs, sender)
	}
}
