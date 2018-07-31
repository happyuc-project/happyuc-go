// Copyright 2014 The go-irchain Authors
// This file is part of the go-irchain library.
//
// The go-irchain library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-irchain library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-irchain library. If not, see <http://www.gnu.org/licenses/>.

package core

import (
	"errors"
	"math/big"

	"github.com/irchain/go-irchain/common"
	"github.com/irchain/go-irchain/core/vm"
	"github.com/irchain/go-irchain/log"
	"github.com/irchain/go-irchain/params"
	"math"
)

var (
	errInsufficientGas           = errors.New("insufficient balance to pay for gas")
	errInsufficientGasByBalance  = errors.New("insufficient balance to pay for gas")
	errInsufficientGasByContract = errors.New("insufficient balance to pay for gas")
)

/*
The State Transitioning Model

A state transition is a change made when a transaction is applied to the current world state
The state transitioning model does all all the necessary work to work out a valid new state root.

1) Nonce handling
2) Pre pay gas
3) Create a new state object if the recipient is \0*32
4) Value transfer
== If contract creation ==
  4a) Attempt to run transaction data
  4b) If valid, use result as code for the new state object
== end ==
5) Run Script section
6) Derive new state root
*/
type StateTransition struct {
	gp         *GasPool
	msg        Message
	gas        uint64
	gasPrice   *big.Int
	initialGas uint64
	value      *big.Int
	data       []byte
	state      vm.StateDB
	evm        *vm.EVM
}

// Message represents a message sent to a contract.
type Message interface {
	From() common.Address
	// FromFrontier() (common.Address, error)
	To() *common.Address

	GasPrice() *big.Int
	Gas() uint64
	Value() *big.Int

	Nonce() uint64
	CheckNonce() bool
	Data() []byte
	// TODO support remark
	// Remark() []byte
}

// IntrinsicGas computes the 'intrinsic gas' for a message with the given data.
func IntrinsicGas(data []byte, contractCreation bool) (uint64, error) {
	// Set the starting gas for the raw transaction
	var gas uint64
	if contractCreation {
		gas = params.TxGasContractCreation
	} else {
		gas = params.TxGas
	}
	// Bump the required gas by the amount of transactional data
	if len(data) > 0 {
		// Zero and non-zero bytes are priced differently
		var nz uint64
		for _, byt := range data {
			if byt != 0 {
				nz++
			}
		}
		// Make sure we don't exceed uint64 for all data combinations
		if (math.MaxUint64-gas)/params.TxDataNonZeroGas < nz {
			return 0, vm.ErrOutOfGas
		}
		gas += nz * params.TxDataNonZeroGas

		z := uint64(len(data)) - nz
		if (math.MaxUint64-gas)/params.TxDataZeroGas < z {
			return 0, vm.ErrOutOfGas
		}
		gas += z * params.TxDataZeroGas
	}
	return gas, nil
}

// NewStateTransition initialises and returns a new state transition object.
func NewStateTransition(evm *vm.EVM, msg Message, gp *GasPool) *StateTransition {
	return &StateTransition{
		gp:       gp,
		evm:      evm,
		msg:      msg,
		gasPrice: msg.GasPrice(),
		value:    msg.Value(),
		data:     msg.Data(),
		state:    evm.StateDB,
	}
}

// ApplyMessage computes the new state by applying the given message
// against the old state within the environment.
//
// ApplyMessage returns the bytes returned by any EVM execution (if it took place),
// the gas used (which includes gas refunds) and an error if it failed. An error always
// indicates a core error meaning that the message would always fail for that particular
// state and would never be accepted within a block.
func ApplyMessage(evm *vm.EVM, msg Message, gp *GasPool) ([]byte, uint64, bool, error) {
	return NewStateTransition(evm, msg, gp).TransitionDb()
}

// to returns the recipient of the message.
func (st *StateTransition) to() common.Address {
	if st.msg == nil || st.msg.To() == nil /* contract creation */ {
		return common.Address{}
	}
	return *st.msg.To()
}

func (st *StateTransition) useGas(amount uint64) error {
	if st.gas < amount {
		return vm.ErrOutOfGas
	}
	st.gas -= amount

	return nil
}

// Transactions fee will be deducted from the recipient. Consider the recipient may
// not have ircer balance, fee will deducted from this transfer.
func (st *StateTransition) buyGas() error {
	var (
		assert *big.Int
		err    error
	)
	if len(st.data) == 0 {
		assert = st.value
		err = errInsufficientGas
	} else if st.msg.To() == nil {
		assert = st.state.GetBalance(st.msg.From())
		err = errInsufficientGasByBalance
	} else {
		err = errInsufficientGasByContract
		assert = st.state.GetBalance(*st.msg.To())
	}
	if assert.Cmp(new(big.Int).Mul(st.gasPrice, new(big.Int).SetUint64(st.msg.Gas()))) < 0 {
		return err
	}
	if err = st.gp.SubGas(st.msg.Gas()); err != nil {
		return err
	}

	st.gas += st.msg.Gas()
	st.initialGas = st.msg.Gas()

	return nil
}

func (st *StateTransition) preCheck() error {
	// Make sure this transaction's nonce is correct.
	if st.msg.CheckNonce() {
		nonce := st.state.GetNonce(st.msg.From())
		if nonce < st.msg.Nonce() {
			return ErrNonceTooHigh
		} else if nonce > st.msg.Nonce() {
			return ErrNonceTooLow
		}
	}
	return st.buyGas()
}

// TransitionDb will transition the state by applying the current message and
// returning the result including the the used gas. It returns an error if it
// failed. An error indicates a consensus issue.
func (st *StateTransition) TransitionDb() (ret []byte, usedGas uint64, failed bool, err error) {
	// pre check and pay deposit
	if err := st.preCheck(); err != nil {
		return nil, 0, false, err
	}

	// pay intrinsic gas
	if gas, err := IntrinsicGas(st.data, st.msg.To() == nil); err != nil {
		return nil, 0, false, err
	} else if err = st.useGas(gas); err != nil {
		return nil, 0, false, err
	}

	// do transaction
	ret, recipient, failed, err := st.transitionDb()
	if err == vm.ErrInsufficientBalance {
		return nil, 0, false, err
	}

	// refund deposit
	st.refundGas()
	st.state.SubBalance(recipient, new(big.Int).Mul(st.gasPrice, new(big.Int).SetUint64(st.gasUsed())))
	st.state.AddBalance(st.evm.Coinbase, new(big.Int).Mul(st.gasPrice, new(big.Int).SetUint64(st.gasUsed())))

	return ret, st.gasUsed(), failed, err
}

// vm errors do not effect consensus and are therefor not
// assigned to err, except for insufficient balance error.
func (st *StateTransition) transitionDb() (ret []byte, recipient common.Address, failed bool, err error) {
	sender := vm.AccountRef(st.msg.From())
	if st.msg.To() == nil {
		recipient = sender.Address()
		ret, _, st.gas, err = st.evm.Create(sender, st.data, st.gas, st.value)
	} else {
		// Increment the nonce for the next transaction
		recipient = *st.msg.To()
		st.state.SetNonce(st.msg.From(), st.state.GetNonce(sender.Address())+1)
		ret, st.gas, err = st.evm.Call(sender, st.to(), st.data, st.gas, st.value)
	}
	if err != nil {
		log.Debug("VM returned with error", "err", err)
		if err == vm.ErrInsufficientBalance {
			return nil, common.Address{}, false, err
		}
	}
	return ret, recipient, err != nil, err
}

func (st *StateTransition) refundGas() {
	// Apply refund counter, capped to half of the used gas.
	var refund = st.gasUsed() / 2
	if refund > st.state.GetRefund() {
		refund = st.state.GetRefund()
	}
	st.gas += refund

	// Retaining gas to the block gas counter so it is available for the next transaction.
	st.gp.AddGas(st.gas)
}

// gasUsed returns the amount of gas used up by the state transition.
func (st *StateTransition) gasUsed() uint64 {
	return st.initialGas - st.gas
}
