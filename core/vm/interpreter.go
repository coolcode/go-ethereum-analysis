// Copyright 2014 The go-ethereum Authors
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

package vm

import (
	"fmt"
	"sync/atomic"

	"go-ethereum/common/math"
	"go-ethereum/params"
)

// Config are the configuration options for the Interpreter
type Config struct {
	// Debug enabled debugging Interpreter options
	Debug bool
	// Tracer is the op code logger
	Tracer Tracer
	// NoRecursion disabled Interpreter call, callcode,
	// delegate call and create.
	NoRecursion bool
	// Enable recording of SHA3/keccak preimages
	EnablePreimageRecording bool
	// JumpTable contains the EVM instruction table. This
	// may be left uninitialised and will be set to the default
	// table.
	JumpTable [256]operation
}

// Interpreter is used to run Ethereum based contracts and will utilise the
// passed environment to query external sources for state information.
// The Interpreter will run the byte code VM based on the passed
// configuration.
type Interpreter interface {
	// Run loops and evaluates the contract's code with the given input data and returns
	// the return byte-slice and an error if one occurred.
	Run(contract *Contract, input []byte) ([]byte, error)
	// CanRun tells if the contract, passed as an argument, can be
	// run by the current interpreter. This is meant so that the
	// caller can do something like:
	//
	// ```golang
	// for _, interpreter := range interpreters {
	//   if interpreter.CanRun(contract.code) {
	//     interpreter.Run(contract.code, input)
	//   }
	// }
	// ```
	CanRun([]byte) bool
	// IsReadOnly reports if the interpreter is in read only mode.
	IsReadOnly() bool
	// SetReadOnly sets (or unsets) read only mode in the interpreter.
	SetReadOnly(bool)
}

// EVMInterpreter represents an EVM interpreter
type EVMInterpreter struct {
	evm      *EVM
	cfg      Config
	gasTable params.GasTable
	intPool  *intPool

	readOnly   bool   // Whether to throw on stateful modifications 只读标识位
	returnData []byte // Last CALL's return data for subsequent reuse 最后一个CALL的返回数据，供后续重用
}

// NewEVMInterpreter returns a new instance of the Interpreter.
func NewEVMInterpreter(evm *EVM, cfg Config) *EVMInterpreter {
	// We use the STOP instruction whether to see
	// the jump table was initialised. If it was not
	// we'll set the default jump table.
	/**
	我们使用STOP指令是否看到跳转表已初始化。 如果不是，我们将设置默认跳转表。
	因为全局的 STOP 执行初始化时，才会把自己的 valid 字段设置为true
	查看 jump_table.go 的 newFrontierInstructionSet() 函数中自明
	 */
	if !cfg.JumpTable[STOP].valid {
		switch {
		//君士坦丁堡 版本
		case evm.ChainConfig().IsConstantinople(evm.BlockNumber):
			//收集命令集
			cfg.JumpTable = constantinopleInstructionSet
		// 拜占庭版本
		case evm.ChainConfig().IsByzantium(evm.BlockNumber):
			//收集命令集
			cfg.JumpTable = byzantiumInstructionSet
		// 家园版本
		case evm.ChainConfig().IsHomestead(evm.BlockNumber):
			//收集命令集
			cfg.JumpTable = homesteadInstructionSet
		default:
			// 前沿 版本（第一个版本）
			//收集命令集
			cfg.JumpTable = frontierInstructionSet
		}
	}

	return &EVMInterpreter{
		// EVM的 执行器又把EVM自身注入
		evm:      evm,
		cfg:      cfg,
		// 收集对应版本的gas 表
		gasTable: evm.ChainConfig().GasTable(evm.BlockNumber),
	}
}

func (in *EVMInterpreter) enforceRestrictions(op OpCode, operation operation, stack *Stack) error {
	if in.evm.chainRules.IsByzantium {
		if in.readOnly {
			// If the interpreter is operating in readonly mode, make sure no
			// state-modifying operation is performed. The 3rd stack item
			// for a call operation is the value. Transferring value from one
			// account to the others means the state is modified and should also
			// return with an error.
			if operation.writes || (op == CALL && stack.Back(2).BitLen() > 0) {
				return errWriteProtection
			}
		}
	}
	return nil
}

// Run loops and evaluates the contract's code with the given input data and returns
// the return byte-slice and an error if one occurred.
//
// It's important to note that any errors returned by the interpreter should be
// considered a revert-and-consume-all-gas operation except for
// errExecutionReverted which means revert-and-keep-gas-left.

/** 执行 EVM  */
func (in *EVMInterpreter) Run(contract *Contract, input []byte) (ret []byte, err error) {
	// 刚开始这个肯定为 nil， 因为实例化 执行器时没有复制该字段
	if in.intPool == nil {
		// 从栈池的池子中获取一个栈池
		in.intPool = poolOfIntPools.get()
		defer func() {
			// 在 该func 执行完后，把该栈池放回栈池的池子中，
			// 目的就是为了重复利用 栈池
			poolOfIntPools.put(in.intPool)
			// 清空当前引用
			in.intPool = nil
		}()
	}

	// Increment the call depth which is restricted to 1024
	/** 增加调用深度，限制为1024 */
	in.evm.depth++
	// 函数调用结束后需要 恢复调用深度
	defer func() { in.evm.depth-- }()

	// Reset the previous call's return data. It's unimportant to preserve the old buffer
	// as every returning call will return new data anyway.
	in.returnData = nil

	// Don't bother with the execution if there's no code.
	if len(contract.Code) == 0 {
		return nil, nil
	}

	var (
		// 当前执行码 (临时变量)
		op    OpCode        // current opcode
		// 初始化EVM执行时需要的内存
		mem   = NewMemory() // bound memory
		// 初始化一个 底层为1024 cap 的 big.int 切片的 栈空间
		stack = newstack()  // local stack
		// For optimisation reason we're using uint64 as the program counter.
		// It's theoretically possible to go above 2^64. The YP defines the PC
		// to be uint256. Practically much less so feasible.
		/**
		出于优化原因，我们使用uint64作为程序计数器。
		理论上可以超过 2 ^ 64。 YP将PC定义为 uint256。 实际上更不可行。
		【指令位置】
		*/
		pc   = uint64(0) // program counter
		// gas 花费
		cost uint64
		// copies used by tracer
		/** 下面两个 *Copy 用于 tracer 追踪用 */
		// debug 使用
		pcCopy  uint64 // needed for the deferred Tracer
		gasCopy uint64 // for Tracer to log gas remaining before execution
		logged  bool   // deferred Tracer should ignore already logged steps
	)
	contract.Input = input

	// Reclaim the stack as an int pool when the execution stops
	defer func() { in.intPool.put(stack.data...) }()

	if in.cfg.Debug {
		defer func() {
			if err != nil {
				if !logged {
					in.cfg.Tracer.CaptureState(in.evm, pcCopy, op, gasCopy, cost, mem, stack, contract, in.evm.depth, err)
				} else {
					in.cfg.Tracer.CaptureFault(in.evm, pcCopy, op, gasCopy, cost, mem, stack, contract, in.evm.depth, err)
				}
			}
		}()
	}
	// The Interpreter main run loop (contextual). This loop runs until either an
	// explicit STOP, RETURN or SELFDESTRUCT is executed, an error occurred during
	// the execution of one of the operations or until the done flag is set by the
	// parent context.
	for atomic.LoadInt32(&in.evm.abort) == 0 {
		if in.cfg.Debug {
			// Capture pre-execution values for tracing.
			logged, pcCopy, gasCopy = false, pc, contract.Gas
		}

		// Get the operation from the jump table and validate the stack to ensure there are
		// enough stack items available to perform the operation.
		/* 获取一条指令及指令对应的操作 */
		op = contract.GetOp(pc)
		operation := in.cfg.JumpTable[op]
		// valid校验
		if !operation.valid {
			return nil, fmt.Errorf("invalid opcode 0x%x", int(op))
		}
		// 栈校验
		if err := operation.validateStack(stack); err != nil {
			return nil, err
		}
		// If the operation is valid, enforce and write restrictions
		// 修改检查
		if err := in.enforceRestrictions(op, operation, stack); err != nil {
			return nil, err
		}

		var memorySize uint64
		// calculate the new memory size and expand the memory to fit
		// the operation
		// 计算内存 按操作所需要的操作数来算
		if operation.memorySize != nil {
			memSize, overflow := bigUint64(operation.memorySize(stack))
			if overflow {
				return nil, errGasUintOverflow
			}
			// memory is expanded in words of 32 bytes. Gas
			// is also calculated in words.
			/** 内存以32字节的字扩展。 Gas 也按 字 来计算。 */
			if memorySize, overflow = math.SafeMul(toWordSize(memSize), 32); overflow {
				return nil, errGasUintOverflow
			}
		}
		// consume the gas and return an error if not enough gas is available.
		// cost is explicitly set so that the capture state defer method can get the proper cost
		/** 校验cost 调用前面提到的costfunc 计算本次操作cost消耗 */
		cost, err = operation.gasCost(in.gasTable, in.evm, contract, stack, mem, memorySize)
		if err != nil || !contract.UseGas(cost) {
			return nil, ErrOutOfGas
		}
		if memorySize > 0 {
			// 如果本次操作需要消耗memory ，扩展memory
			mem.Resize(memorySize)
		}

		if in.cfg.Debug {
			in.cfg.Tracer.CaptureState(in.evm, pc, op, gasCopy, cost, mem, stack, contract, in.evm.depth, err)
			logged = true
		}

		// execute the operation
		/* 执行操作 */
		res, err := operation.execute(&pc, in, contract, mem, stack)
		// verifyPool is a build flag. Pool verification makes sure the integrity
		// of the integer pool by comparing values to a default value.
		if verifyPool {
			verifyIntegerPool(in.intPool)
		}
		// if the operation clears the return data (e.g. it has returning data)
		// set the last return to the result of the operation.
		// 如果遇到return 设置返回值
		if operation.returns {
			in.returnData = res
		}

		switch {
		//报错
		case err != nil:
			return nil, err
		//出错回滚
		case operation.reverts:
			return res, errExecutionReverted
		//停止
		case operation.halts:
			return res, nil
		// 跳转
		case !operation.jumps:
			pc++
		}
	}
	return nil, nil
}

// CanRun tells if the contract, passed as an argument, can be
// run by the current interpreter.
func (in *EVMInterpreter) CanRun(code []byte) bool {
	return true
}

// IsReadOnly reports if the interpreter is in read only mode.
func (in *EVMInterpreter) IsReadOnly() bool {
	return in.readOnly
}

// SetReadOnly sets (or unsets) read only mode in the interpreter.
func (in *EVMInterpreter) SetReadOnly(ro bool) {
	in.readOnly = ro
}