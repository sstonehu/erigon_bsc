package transactions

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	jsoniter "github.com/json-iterator/go"
	"github.com/ledgerwatch/erigon-lib/chain"
	libcommon "github.com/ledgerwatch/erigon-lib/common"
	"github.com/ledgerwatch/erigon-lib/kv"
	"github.com/ledgerwatch/erigon/consensus"
	"github.com/ledgerwatch/erigon/core"
	"github.com/ledgerwatch/erigon/core/state"
	"github.com/ledgerwatch/erigon/core/types"
	"github.com/ledgerwatch/erigon/core/vm"
	"github.com/ledgerwatch/erigon/core/vm/evmtypes"
	"github.com/ledgerwatch/erigon/eth/tracers"
	"github.com/ledgerwatch/erigon/eth/tracers/logger"
	"github.com/ledgerwatch/erigon/turbo/rpchelper"
	"github.com/ledgerwatch/erigon/turbo/services"
)

type BlockGetter interface {
	// GetBlockByHash retrieves a block from the database by hash, caching it if found.
	GetBlockByHash(hash libcommon.Hash) (*types.Block, error)
	// GetBlock retrieves a block from the database by hash and number,
	// caching it if found.
	GetBlock(hash libcommon.Hash, number uint64) *types.Block
}

// ComputeTxEnv returns the execution environment of a certain transaction.
func ComputeTxEnv(ctx context.Context, engine consensus.EngineReader, block *types.Block, cfg *chain.Config, headerReader services.HeaderReader, dbtx kv.Tx, txIndex int) (core.Message, evmtypes.BlockContext, evmtypes.TxContext, *state.IntraBlockState, state.StateReader, error) {
	reader, err := rpchelper.CreateHistoryStateReader(dbtx, block.NumberU64(), txIndex, cfg.ChainName)
	if err != nil {
		return nil, evmtypes.BlockContext{}, evmtypes.TxContext{}, nil, nil, err
	}

	// Create the parent state database
	statedb := state.New(reader)

	if txIndex == 0 && len(block.Transactions()) == 0 {
		return nil, evmtypes.BlockContext{}, evmtypes.TxContext{}, statedb, reader, nil
	}
	getHeader := func(hash libcommon.Hash, n uint64) *types.Header {
		h, _ := headerReader.HeaderByNumber(ctx, dbtx, n)
		return h
	}
	header := block.HeaderNoCopy()

	blockContext := core.NewEVMBlockContext(header, core.GetHashFn(header, getHeader), engine, nil)

	// Recompute transactions up to the target index.
	signer := types.MakeSigner(cfg, block.NumberU64(), block.Time())
	rules := cfg.Rules(blockContext.BlockNumber, blockContext.Time)
	txn := block.Transactions()[txIndex]
	statedb.SetTxContext(txn.Hash(), block.Hash(), txIndex)
	msg, _ := txn.AsMessage(*signer, block.BaseFee(), rules)
	if msg.FeeCap().IsZero() && engine != nil {
		syscall := func(contract libcommon.Address, data []byte) ([]byte, error) {
			return core.SysCallContract(contract, data, cfg, statedb, header, engine, true /* constCall */)
		}
		msg.SetIsFree(engine.IsServiceTransaction(msg.From(), syscall))
	}

	TxContext := core.NewEVMTxContext(msg)
	return msg, blockContext, TxContext, statedb, reader, nil
}

// TraceTx configures a new tracer according to the provided configuration, and
// executes the given message in the provided environment. The return value will
// be tracer dependent.
func TraceTx(
	ctx context.Context,
	message core.Message,
	blockCtx evmtypes.BlockContext,
	txCtx evmtypes.TxContext,
	ibs evmtypes.IntraBlockState,
	config *tracers.TraceConfig,
	chainConfig *chain.Config,
	stream *jsoniter.Stream,
	callTimeout time.Duration,
	intrinsicGas uint64,
) error {
	tracer, streaming, cancel, err := AssembleTracer(ctx, config, txCtx.TxHash, stream, callTimeout)
	if err != nil {
		stream.WriteNil()
		return err
	}

	defer cancel()

	execCb := func(evm *vm.EVM, refunds bool) (*core.ExecutionResult, error) {
		gp := new(core.GasPool).AddGas(message.Gas()).AddBlobGas(message.BlobGas())
		return core.ApplyMessage(evm, message, gp, refunds, false /* gasBailout */)
	}

	return ExecuteTraceTx(blockCtx, txCtx, ibs, config, chainConfig, stream, tracer, streaming, execCb, intrinsicGas)
}

func AssembleTracer(
	ctx context.Context,
	config *tracers.TraceConfig,
	txHash libcommon.Hash,
	stream *jsoniter.Stream,
	callTimeout time.Duration,
) (vm.EVMLogger, bool, context.CancelFunc, error) {
	// Assemble the structured logger or the JavaScript tracer
	switch {
	case config != nil && config.Tracer != nil:
		// Define a meaningful timeout of a single transaction trace
		timeout := callTimeout
		if config.Timeout != nil {
			var err error
			timeout, err = time.ParseDuration(*config.Timeout)
			if err != nil {
				return nil, false, func() {}, err
			}
		}

		// Construct the JavaScript tracer to execute with
		cfg := json.RawMessage("{}")
		if config != nil && config.TracerConfig != nil {
			cfg = *config.TracerConfig
		}
		tracer, err := tracers.New(*config.Tracer, &tracers.Context{TxHash: txHash}, cfg)
		if err != nil {
			return nil, false, func() {}, err
		}

		// Handle timeouts and RPC cancellations
		deadlineCtx, cancel := context.WithTimeout(ctx, timeout)
		go func() {
			<-deadlineCtx.Done()
			tracer.Stop(errors.New("execution timeout"))
		}()

		return tracer, false, cancel, nil
	case config == nil:
		return logger.NewJsonStreamLogger(nil, ctx, stream), true, func() {}, nil
	default:
		return logger.NewJsonStreamLogger(config.LogConfig, ctx, stream), true, func() {}, nil
	}
}

func ExecuteTraceTx(
	blockCtx evmtypes.BlockContext,
	txCtx evmtypes.TxContext,
	ibs evmtypes.IntraBlockState,
	config *tracers.TraceConfig,
	chainConfig *chain.Config,
	stream *jsoniter.Stream,
	tracer vm.EVMLogger,
	streaming bool,
	execCb func(evm *vm.EVM, refunds bool) (*core.ExecutionResult, error),
	intrinsicGas uint64,
) error {
	// Run the transaction with tracing enabled.
	evm := vm.NewEVM(blockCtx, txCtx, ibs, chainConfig, vm.Config{Debug: true, Tracer: tracer})

	var refunds = true
	if config != nil && config.NoRefunds != nil && *config.NoRefunds {
		refunds = false
	}

	if streaming {
		stream.WriteObjectStart()
		stream.WriteObjectField("structLogs")
		stream.WriteArrayStart()
	}

	result, err := execCb(evm, refunds)
	tracer.CaptureSystemTxEnd(intrinsicGas)
	if err != nil {
		if streaming {
			stream.WriteArrayEnd()
			stream.WriteObjectEnd()
		} else {
			stream.WriteNil()
		}
		return fmt.Errorf("tracing failed: %w", err)
	}
	// Depending on the tracer type, format and return the output
	if streaming {
		stream.WriteArrayEnd()
		stream.WriteMore()
		stream.WriteObjectField("gas")
		stream.WriteUint64(result.UsedGas - intrinsicGas)
		stream.WriteMore()
		stream.WriteObjectField("failed")
		stream.WriteBool(result.Failed())
		stream.WriteMore()
		// If the result contains a revert reason, return it.
		returnVal := hex.EncodeToString(result.Return())
		if len(result.Revert()) > 0 {
			returnVal = hex.EncodeToString(result.Revert())
		}
		stream.WriteObjectField("returnValue")
		stream.WriteString(returnVal)
		stream.WriteObjectEnd()
	} else {
		r, err := tracer.(tracers.Tracer).GetResult()
		if err != nil {
			stream.WriteNil()
			return err
		}

		_, err = stream.Write(r)
		if err != nil {
			stream.WriteNil()
			return err
		}
	}

	return nil
}
