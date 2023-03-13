package contract

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/sei-protocol/sei-chain/utils/logging"
	"github.com/sei-protocol/sei-chain/utils/tracing"

	sdk "github.com/cosmos/cosmos-sdk/types"
	"github.com/sei-protocol/sei-chain/store/whitelist/multi"
	seisync "github.com/sei-protocol/sei-chain/sync"
	"github.com/sei-protocol/sei-chain/utils/datastructures"
	dexcache "github.com/sei-protocol/sei-chain/x/dex/cache"
	"github.com/sei-protocol/sei-chain/x/dex/keeper"
	dexkeeperabci "github.com/sei-protocol/sei-chain/x/dex/keeper/abci"
	dexkeeperutils "github.com/sei-protocol/sei-chain/x/dex/keeper/utils"
	"github.com/sei-protocol/sei-chain/x/dex/types"
	dextypesutils "github.com/sei-protocol/sei-chain/x/dex/types/utils"
	dextypeswasm "github.com/sei-protocol/sei-chain/x/dex/types/wasm"
	dexutils "github.com/sei-protocol/sei-chain/x/dex/utils"
	"github.com/sei-protocol/sei-chain/x/store"
	otrace "go.opentelemetry.io/otel/trace"
)

const LogRunnerRunAfter = 10 * time.Second
const LogExecSigSendAfter = 2 * time.Second

type environment struct {
	validContractsInfo          []types.ContractInfoV2
	failedContractAddresses     datastructures.SyncSet[string]
	finalizeBlockMessages       *datastructures.TypedSyncMap[string, *dextypeswasm.SudoFinalizeBlockMsg]
	settlementsByContract       *datastructures.TypedSyncMap[string, []*types.SettlementEntry]
	executionTerminationSignals *datastructures.TypedSyncMap[string, chan struct{}]
	registeredPairs             *datastructures.TypedSyncMap[string, []types.Pair]
	orderBooks                  *datastructures.TypedNestedSyncMap[string, dextypesutils.PairString, *types.OrderBook]

	finalizeMsgMutex  *sync.Mutex
	eventManagerMutex *sync.Mutex
}

func EndBlockerAtomic(ctx sdk.Context, keeper *keeper.Keeper, validContractsInfo []types.ContractInfoV2, tracingInfo *tracing.Info) ([]types.ContractInfoV2, sdk.Context, bool) {
	tracer := tracingInfo.Tracer
	spanCtx, span := tracingInfo.Start("DexEndBlockerAtomic")
	defer span.End()
	startTime := time.Now().UnixMicro()
	env := newEnv(ctx, validContractsInfo, keeper)
	cachedCtx, msCached := cacheContext(ctx, env)
	memStateCopy := dexutils.GetMemState(cachedCtx.Context()).DeepCopy()
	handleDeposits(spanCtx, cachedCtx, env, keeper, tracer)
	handleDepositCompleteTime := time.Now().UnixMicro()

	runner := NewParallelRunner(func(contract types.ContractInfoV2) {
		orderMatchingRunnable(spanCtx, cachedCtx, env, keeper, contract, tracer)
	}, validContractsInfo, cachedCtx)

	_, err := logging.LogIfNotDoneAfter(ctx.Logger(), func() (struct{}, error) {
		runner.Run()
		return struct{}{}, nil
	}, LogRunnerRunAfter, "runner run")
	orderMatchCompleteTime := time.Now().UnixMicro()
	if err != nil {
		// this should never happen
		panic(err)
	}

	handleSettlements(spanCtx, cachedCtx, env, keeper, tracer)
	handleSettlementsCompleteTime := time.Now().UnixMicro()
	handleUnfulfilledMarketOrders(spanCtx, cachedCtx, env, keeper, tracer)
	handleFinalizedBlocks(spanCtx, cachedCtx, env, keeper, tracer)
	handleFinalizeBlocksTime := time.Now().UnixMicro()
	// No error is thrown for any contract. This should happen most of the time.

	completeTime := time.Now().UnixMicro()
	totalLatency := completeTime - startTime
	handleFinalizeBlockLatency := handleFinalizeBlocksTime - handleSettlementsCompleteTime
	handleSettlementsLatency := handleSettlementsCompleteTime - orderMatchCompleteTime
	orderMatchLatency := orderMatchCompleteTime - handleDepositCompleteTime
	handleDepositLatency := handleDepositCompleteTime - startTime
	ctx.Logger().Info(fmt.Sprintf("[SeiChain-Debug] EndBlock total latency: %d, handleDeposit %d, orderMatch %d, handleSettlements %d, handleFinalizeBlock %d",
		totalLatency, handleDepositLatency, orderMatchLatency, handleSettlementsLatency, handleFinalizeBlockLatency))

	if env.failedContractAddresses.Size() == 0 {
		msCached.Write()
		return env.validContractsInfo, ctx, true
	}

	// persistent contract rent charges for failed contracts and discard everything else
	for _, failedContractAddress := range env.failedContractAddresses.ToOrderedSlice(datastructures.StringComparator) {
		cachedContract, err := keeper.GetContract(cachedCtx, failedContractAddress)
		if err != nil {
			ctx.Logger().Error(fmt.Sprintf("error %s when getting updated contract %s to persist rent balance", err, failedContractAddress))
			continue
		}
		contract, err := keeper.GetContract(ctx, failedContractAddress)
		if err != nil {
			ctx.Logger().Error(fmt.Sprintf("error %s when getting contract %s to persist rent balance", err, failedContractAddress))
			continue
		}
		contract.RentBalance = cachedContract.RentBalance
		err = keeper.SetContract(ctx, &contract)
		if err != nil {
			ctx.Logger().Error(fmt.Sprintf("error %s when persisting contract %s's rent balance", err, failedContractAddress))
			continue
		}
	}
	// restore keeper in-memory state
	newGoContext := context.WithValue(ctx.Context(), dexutils.DexMemStateContextKey, memStateCopy)

	return filterNewValidContracts(ctx, env), ctx.WithContext(newGoContext), false
}

func newEnv(ctx sdk.Context, validContractsInfo []types.ContractInfoV2, keeper *keeper.Keeper) *environment {
	finalizeBlockMessages := datastructures.NewTypedSyncMap[string, *dextypeswasm.SudoFinalizeBlockMsg]()
	settlementsByContract := datastructures.NewTypedSyncMap[string, []*types.SettlementEntry]()
	executionTerminationSignals := datastructures.NewTypedSyncMap[string, chan struct{}]()
	registeredPairs := datastructures.NewTypedSyncMap[string, []types.Pair]()
	orderBooks := datastructures.NewTypedNestedSyncMap[string, dextypesutils.PairString, *types.OrderBook]()
	for _, contract := range validContractsInfo {
		finalizeBlockMessages.Store(contract.ContractAddr, dextypeswasm.NewSudoFinalizeBlockMsg())
		settlementsByContract.Store(contract.ContractAddr, []*types.SettlementEntry{})
		executionTerminationSignals.Store(contract.ContractAddr, make(chan struct{}, 1))
		contractPairs := keeper.GetAllRegisteredPairs(ctx, contract.ContractAddr)
		registeredPairs.Store(contract.ContractAddr, contractPairs)
		for _, pair := range contractPairs {
			pair := pair
			orderBooks.StoreNested(contract.ContractAddr, dextypesutils.GetPairString(&pair), dexkeeperutils.PopulateOrderbook(
				ctx, keeper, dextypesutils.ContractAddress(contract.ContractAddr), pair,
			))
		}
	}
	return &environment{
		validContractsInfo:          validContractsInfo,
		failedContractAddresses:     datastructures.NewSyncSet([]string{}),
		finalizeBlockMessages:       finalizeBlockMessages,
		settlementsByContract:       settlementsByContract,
		executionTerminationSignals: executionTerminationSignals,
		registeredPairs:             registeredPairs,
		orderBooks:                  orderBooks,
		finalizeMsgMutex:            &sync.Mutex{},
		eventManagerMutex:           &sync.Mutex{},
	}
}

func cacheContext(ctx sdk.Context, env *environment) (sdk.Context, sdk.CacheMultiStore) {
	cachedCtx, msCached := store.GetCachedContext(ctx)
	goCtx := context.WithValue(cachedCtx.Context(), dexcache.CtxKeyExecTermSignal, env.executionTerminationSignals)
	cachedCtx = cachedCtx.WithContext(goCtx)
	return cachedCtx, msCached
}

func decorateContextForContract(ctx sdk.Context, contractInfo types.ContractInfoV2, gasLimit uint64) sdk.Context {
	goCtx := context.WithValue(ctx.Context(), dexcache.CtxKeyExecutingContract, contractInfo)
	whitelistedStore := multi.NewStore(ctx.MultiStore(), GetWhitelistMap(contractInfo.ContractAddr))
	newEventManager := sdk.NewEventManager()
	return ctx.WithContext(goCtx).WithMultiStore(whitelistedStore).WithEventManager(newEventManager).WithGasMeter(
		seisync.NewGasWrapper(dexutils.GetGasMeterForLimit(gasLimit)),
	)
}

func handleDeposits(spanCtx context.Context, ctx sdk.Context, env *environment, keeper *keeper.Keeper, tracer *otrace.Tracer) {
	// Handle deposit sequentially since they mutate `bank` state which is shared by all contracts
	_, span := (*tracer).Start(spanCtx, "handleDeposits")
	defer span.End()
	keeperWrapper := dexkeeperabci.KeeperWrapper{Keeper: keeper}
	contractInfos := env.validContractsInfo
	startTime := time.Now().UnixMicro()
	for _, contract := range contractInfos {
		if !contract.NeedOrderMatching {
			continue
		}
		if err := keeperWrapper.HandleEBDeposit(ctx.Context(), ctx, tracer, contract.ContractAddr); err != nil {
			env.failedContractAddresses.Add(contract.ContractAddr)
		}
	}
	endTime := time.Now().UnixMicro()
	ctx.Logger().Info(fmt.Sprintf("[SeiChain-Debug] EndBlock handleDeposits over %d contracts takes %d time", len(contractInfos), endTime-startTime))
}

func handleSettlements(ctx context.Context, sdkCtx sdk.Context, env *environment, keeper *keeper.Keeper, tracer *otrace.Tracer) {
	_, span := (*tracer).Start(ctx, "DexEndBlockerHandleSettlements")
	defer span.End()
	contractsNeedOrderMatching := datastructures.NewSyncSet([]string{})
	for _, contract := range env.validContractsInfo {
		if contract.NeedOrderMatching {
			contractsNeedOrderMatching.Add(contract.ContractAddr)
		}
	}
	env.settlementsByContract.Range(func(contractAddr string, settlements []*types.SettlementEntry) bool {
		if !contractsNeedOrderMatching.Contains(contractAddr) {
			return true
		}
		if err := HandleSettlements(sdkCtx, contractAddr, keeper, settlements); err != nil {
			sdkCtx.Logger().Error(fmt.Sprintf("Error handling settlements for %s", contractAddr))
			env.failedContractAddresses.Add(contractAddr)
		}
		return true
	})
}

func handleUnfulfilledMarketOrders(ctx context.Context, sdkCtx sdk.Context, env *environment, keeper *keeper.Keeper, tracer *otrace.Tracer) {
	// Cancel unfilled market orders
	for _, contract := range env.validContractsInfo {
		if contract.NeedOrderMatching {
			registeredPairs, found := env.registeredPairs.Load(contract.ContractAddr)
			if !found {
				continue
			}
			if err := CancelUnfulfilledMarketOrders(ctx, sdkCtx, contract.ContractAddr, keeper, registeredPairs, tracer); err != nil {
				sdkCtx.Logger().Error(fmt.Sprintf("Error cancelling unfulfilled market orders for %s", contract.ContractAddr))
				env.failedContractAddresses.Add(contract.ContractAddr)
			}
		}
	}
}

func handleFinalizedBlocks(ctx context.Context, sdkCtx sdk.Context, env *environment, keeper *keeper.Keeper, tracer *otrace.Tracer) {
	_, span := (*tracer).Start(ctx, "DexEndBlockerHandleFinalizedBlocks")
	defer span.End()
	contractsNeedHook := datastructures.NewSyncSet([]string{})
	for _, contract := range env.validContractsInfo {
		if contract.NeedHook {
			contractsNeedHook.Add(contract.ContractAddr)
		}
	}
	env.finalizeBlockMessages.Range(func(contractAddr string, finalizeBlockMsg *dextypeswasm.SudoFinalizeBlockMsg) bool {
		if !contractsNeedHook.Contains(contractAddr) {
			return true
		}
		if len(finalizeBlockMsg.FinalizeBlock.Results) == 0 {
			return true
		}
		if _, err := dexkeeperutils.CallContractSudo(sdkCtx, keeper, contractAddr, finalizeBlockMsg, dexutils.ZeroUserProvidedGas); err != nil {
			sdkCtx.Logger().Error(fmt.Sprintf("Error calling FinalizeBlock of %s", contractAddr))
			env.failedContractAddresses.Add(contractAddr)
		}
		return true
	})
}

func orderMatchingRunnable(ctx context.Context, sdkContext sdk.Context, env *environment, keeper *keeper.Keeper, contractInfo types.ContractInfoV2, tracer *otrace.Tracer) {
	_, span := (*tracer).Start(ctx, "orderMatchingRunnable")
	defer span.End()
	defer func() {
		if channel, ok := env.executionTerminationSignals.Load(contractInfo.ContractAddr); ok {
			_, err := logging.LogIfNotDoneAfter(sdkContext.Logger(), func() (struct{}, error) {
				channel <- struct{}{}
				return struct{}{}, nil
			}, LogExecSigSendAfter, fmt.Sprintf("send execution terminal signal for %s", contractInfo.ContractAddr))
			if err != nil {
				// this should never happen
				panic(err)
			}
		}
	}()
	if !contractInfo.NeedOrderMatching {
		return
	}
	parentSdkContext := sdkContext
	sdkContext = decorateContextForContract(sdkContext, contractInfo, keeper.GetParams(sdkContext).EndBlockGasLimit)
	sdkContext.Logger().Debug(fmt.Sprintf("End block for %s with balance of %d", contractInfo.ContractAddr, contractInfo.RentBalance))
	pairs, pairFound := env.registeredPairs.Load(contractInfo.ContractAddr)
	orderBooks, found := env.orderBooks.Load(contractInfo.ContractAddr)

	if !pairFound || !found {
		sdkContext.Logger().Error(fmt.Sprintf("No pair or order book for %s", contractInfo.ContractAddr))
		env.failedContractAddresses.Add(contractInfo.ContractAddr)
	} else if orderResultsMap, settlements, err := HandleExecutionForContract(ctx, sdkContext, contractInfo, keeper, pairs, orderBooks, tracer); err != nil {
		sdkContext.Logger().Error(fmt.Sprintf("Error for EndBlock of %s", contractInfo.ContractAddr))
		env.failedContractAddresses.Add(contractInfo.ContractAddr)
	} else {
		for account, orderResults := range orderResultsMap {
			// only add to finalize message for contract addresses
			if msg, ok := env.finalizeBlockMessages.Load(account); ok {
				// ordering of `AddContractResult` among multiple orderMatchingRunnable instances doesn't matter
				// since it's not persisted as state, and it's only used for invoking registered contracts'
				// FinalizeBlock sudo endpoints, whose state updates are gated by whitelist stores anyway.
				msg.AddContractResult(orderResults, env.finalizeMsgMutex)
			}
		}
		env.settlementsByContract.Store(contractInfo.ContractAddr, settlements)
	}

	// ordering of events doesn't matter since events aren't part of consensus
	env.eventManagerMutex.Lock()
	defer env.eventManagerMutex.Unlock()
	parentSdkContext.EventManager().EmitEvents(sdkContext.EventManager().Events())
}

func filterNewValidContracts(ctx sdk.Context, env *environment) []types.ContractInfoV2 {
	newValidContracts := []types.ContractInfoV2{}
	for _, contract := range env.validContractsInfo {
		if !env.failedContractAddresses.Contains(contract.ContractAddr) {
			newValidContracts = append(newValidContracts, contract)
		}
	}
	for _, failedContractAddress := range env.failedContractAddresses.ToOrderedSlice(datastructures.StringComparator) {
		dexutils.GetMemState(ctx.Context()).DeepFilterAccount(ctx, failedContractAddress)
	}
	return newValidContracts
}
