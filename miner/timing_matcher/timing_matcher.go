/*

  Copyright 2017 Loopring Project Ltd (Loopring Foundation).

  Licensed under the Apache License, Version 2.0 (the "License");
  you may not use this file except in compliance with the License.
  You may obtain a copy of the License at

  http://www.apache.org/licenses/LICENSE-2.0

  Unless required by applicable law or agreed to in writing, software
  distributed under the License is distributed on an "AS IS" BASIS,
  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
  See the License for the specific language governing permissions and
  limitations under the License.

*/

package timing_matcher

import (
	"github.com/Loopring/relay/config"
	"github.com/Loopring/relay/eventemiter"
	"github.com/Loopring/relay/log"
	marketLib "github.com/Loopring/relay/market"
	marketUtilLib "github.com/Loopring/relay/market/util"
	"github.com/Loopring/relay/miner"
	"github.com/Loopring/relay/ordermanager"
	"github.com/Loopring/relay/types"
	"github.com/ethereum/go-ethereum/common"
	"math/big"
	"sync"
)

/**
定时从ordermanager中拉取n条order数据进行匹配成环，如果成环则通过调用evaluator进行费用估计，然后提交到submitter进行提交到以太坊
*/

type minedRing struct {
	ringHash    common.Hash
	orderHashes []common.Hash
}

type RoundState struct {
	round          *big.Int
	ringHash       common.Hash
	matchedAmountS *big.Rat
	matchedAmountB *big.Rat
}

type OrderMatchState struct {
	orderState types.OrderState
	rounds     []*RoundState
}

type TimingMatcher struct {
	MatchedOrders   map[common.Hash]*OrderMatchState
	MinedRings      map[common.Hash]*minedRing
	mtx             sync.RWMutex
	StopChan        chan bool
	markets         []*Market
	submitter       *miner.RingSubmitter
	evaluator       *miner.Evaluator
	lastBlockNumber *big.Int
	duration        *big.Int
	roundOrderCount int
	delayedNumber   int64
	accounts        map[common.Address]*miner.Account
	accountManager  *marketLib.AccountManager

	afterSubmitWatcher *eventemitter.Watcher
	blockTriger        *eventemitter.Watcher
}

type Market struct {
	matcher         *TimingMatcher
	om              ordermanager.OrderManager
	protocolAddress common.Address
	lrcAddress      common.Address

	TokenA     common.Address
	TokenB     common.Address
	AtoBOrders map[common.Hash]*types.OrderState
	BtoAOrders map[common.Hash]*types.OrderState

	AtoBOrderHashesExcludeNextRound []common.Hash
	BtoAOrderHashesExcludeNextRound []common.Hash
}

func NewTimingMatcher(matcherOptions *config.TimingMatcher, submitter *miner.RingSubmitter, evaluator *miner.Evaluator, om ordermanager.OrderManager, accountManager *marketLib.AccountManager) *TimingMatcher {
	matcher := &TimingMatcher{}
	matcher.submitter = submitter
	matcher.evaluator = evaluator
	matcher.accountManager = accountManager
	matcher.roundOrderCount = matcherOptions.RoundOrdersCount
	matcher.MatchedOrders = make(map[common.Hash]*OrderMatchState)
	matcher.markets = []*Market{}
	matcher.duration = big.NewInt(matcherOptions.Duration)
	matcher.delayedNumber = matcherOptions.DelayedNumber

	matcher.lastBlockNumber = big.NewInt(0)
	pairs := make(map[common.Address]common.Address)
	for _, pair := range marketUtilLib.AllTokenPairs {
		if addr, ok := pairs[pair.TokenS]; !ok || addr != pair.TokenB {
			if addr1, ok1 := pairs[pair.TokenB]; !ok1 || addr1 != pair.TokenS {
				for _, protocolAddress := range matcher.submitter.Accessor.ProtocolAddresses {
					pairs[pair.TokenS] = pair.TokenB
					m := &Market{}
					m.protocolAddress = protocolAddress.ContractAddress
					m.lrcAddress = protocolAddress.LrcTokenAddress
					m.om = om
					m.matcher = matcher
					m.TokenA = pair.TokenS
					m.TokenB = pair.TokenB
					m.AtoBOrderHashesExcludeNextRound = []common.Hash{}
					m.BtoAOrderHashesExcludeNextRound = []common.Hash{}
					matcher.markets = append(matcher.markets, m)
				}
			}
		} else {
			log.Debugf("miner,timing matcher cann't find tokenPair tokenS:%s, tokenB:%s", pair.TokenS.Hex(), pair.TokenB.Hex())
		}
	}

	return matcher
}

func (matcher *TimingMatcher) Start() {
	matcher.afterSubmitWatcher = &eventemitter.Watcher{Concurrent: false, Handle: matcher.afterSubmit}
	//todo:the topic should contain submit success
	eventemitter.On(eventemitter.OrderManagerExtractorRingMined, matcher.afterSubmitWatcher)
	eventemitter.On(eventemitter.Miner_RingSubmitFailed, matcher.afterSubmitWatcher)
	matcher.blockTriger = &eventemitter.Watcher{Concurrent: false, Handle: matcher.blockTrigger}
	eventemitter.On(eventemitter.Block_New, matcher.blockTriger)
}

func (matcher *TimingMatcher) blockTrigger(eventData eventemitter.EventData) error {
	blockEvent := eventData.(*types.BlockEvent)
	nextBlockNumber := new(big.Int).Add(matcher.duration, matcher.lastBlockNumber)
	if nextBlockNumber.Cmp(blockEvent.BlockNumber) <= 0 {
		matcher.lastBlockNumber = blockEvent.BlockNumber
		//accounts must be reset every round
		matcher.accounts = make(map[common.Address]*miner.Account)
		var wg sync.WaitGroup
		for _, market := range matcher.markets {
			wg.Add(1)
			go func(market *Market) {
				defer func() {
					wg.Add(-1)
				}()
				market.match()
			}(market)
		}
		wg.Wait()
	}
	return nil
}

func (matcher *TimingMatcher) getAccountBalance(address common.Address, tokenAddress common.Address) (*miner.TokenBalance, error) {
	matcher.mtx.Lock()
	defer matcher.mtx.Unlock()
	var (
		account *miner.Account
		exists  bool
	)
	if account, exists = matcher.accounts[address]; !exists {
		account = &miner.Account{}
		account.Tokens = make(map[common.Address]*miner.TokenBalance)
		matcher.accounts[address] = account
	}

	if b, e := account.Tokens[tokenAddress]; !e {
		if balance, allowance, err := matcher.accountManager.GetBalanceByTokenAddress(address, tokenAddress); nil != err {
			return nil, err
		} else {
			b = &miner.TokenBalance{}
			b.Allowance = new(big.Int).Set(balance)
			b.Balance = new(big.Int).Set(allowance)
			account.Tokens[tokenAddress] = b
			return b, nil
		}
	} else {
		return b, nil
	}
}

/**
get orders from ordermanager
*/
func (market *Market) getOrdersForMatching(protocolAddress common.Address) {
	market.AtoBOrders = make(map[common.Hash]*types.OrderState)
	market.BtoAOrders = make(map[common.Hash]*types.OrderState)

	// log.Debugf("timing matcher,market tokenA:%s, tokenB:%s, atob hash length:%d, btoa hash length:%d", market.TokenA.Hex(), market.TokenB.Hex(), len(market.AtoBOrderHashesExcludeNextRound), len(market.BtoAOrderHashesExcludeNextRound))

	atoBOrders := market.om.MinerOrders(protocolAddress, market.TokenA, market.TokenB, market.matcher.roundOrderCount, &types.OrderDelayList{OrderHash: market.AtoBOrderHashesExcludeNextRound, DelayedCount: market.matcher.delayedNumber})
	btoAOrders := market.om.MinerOrders(protocolAddress, market.TokenB, market.TokenA, market.matcher.roundOrderCount, &types.OrderDelayList{OrderHash: market.BtoAOrderHashesExcludeNextRound, DelayedCount: market.matcher.delayedNumber})

	market.AtoBOrderHashesExcludeNextRound = []common.Hash{}
	market.BtoAOrderHashesExcludeNextRound = []common.Hash{}

	for _, order := range atoBOrders {
		market.reduceRemainedAmountBeforeMatch(order)
		if !market.om.IsOrderFullFinished(order) {
			market.AtoBOrders[order.RawOrder.Hash] = order
		} else {
			market.AtoBOrderHashesExcludeNextRound = append(market.AtoBOrderHashesExcludeNextRound, order.RawOrder.Hash)
		}
	}

	for _, order := range btoAOrders {
		market.reduceRemainedAmountBeforeMatch(order)
		if !market.om.IsOrderFullFinished(order) {
			market.BtoAOrders[order.RawOrder.Hash] = order
		} else {
			market.BtoAOrderHashesExcludeNextRound = append(market.BtoAOrderHashesExcludeNextRound, order.RawOrder.Hash)
		}
	}
}

//sub the matched amount in new round.
func (market *Market) reduceRemainedAmountBeforeMatch(orderState *types.OrderState) {
	orderHash := orderState.RawOrder.Hash

	if matchedOrder, ok := market.matcher.MatchedOrders[orderHash]; ok {
		//if len(matchedOrder.rounds) <= 0 {
		//	delete(market.AtoBOrders, orderHash)
		//	delete(market.BtoAOrders, orderHash)
		//} else {
		for _, matchedRound := range matchedOrder.rounds {
			orderState.DealtAmountB.Add(orderState.DealtAmountB, intFromRat(matchedRound.matchedAmountB))
			orderState.DealtAmountS.Add(orderState.DealtAmountS, intFromRat(matchedRound.matchedAmountS))
		}
		//}
	}

}

func (market *Market) reduceAmountAfterFilled(filledOrder *types.FilledOrder) *types.OrderState {
	filledOrderState := filledOrder.OrderState
	var orderState *types.OrderState
	if filledOrderState.RawOrder.TokenS == market.TokenA {
		orderState = market.AtoBOrders[filledOrderState.RawOrder.Hash]
		orderState.DealtAmountB.Add(orderState.DealtAmountB, intFromRat(filledOrder.FillAmountB))
		orderState.DealtAmountS.Add(orderState.DealtAmountS, intFromRat(filledOrder.FillAmountS))
	} else {
		orderState = market.BtoAOrders[filledOrderState.RawOrder.Hash]
		orderState.DealtAmountB.Add(orderState.DealtAmountB, intFromRat(filledOrder.FillAmountB))
		orderState.DealtAmountS.Add(orderState.DealtAmountS, intFromRat(filledOrder.FillAmountS))
	}
	return orderState
}

func (market *Market) match() {
	market.getOrdersForMatching(market.protocolAddress)
	matchedOrderHashes := make(map[common.Hash]bool) //true:fullfilled, false:partfilled
	ringStates := []*types.RingSubmitInfo{}
	for _, a2BOrder := range market.AtoBOrders {
		var ringForSubmit *types.RingSubmitInfo
		for _, b2AOrder := range market.BtoAOrders {
			if miner.PriceValid(a2BOrder, b2AOrder) {
				filledOrders := []*types.FilledOrder{}
				var (
					a2BLrcToken *miner.TokenBalance
					a2BTokenS   *miner.TokenBalance
					b2ALrcToken *miner.TokenBalance
					b2ATokenS   *miner.TokenBalance
					err         error
				)
				if a2BLrcToken, err = market.matcher.getAccountBalance(a2BOrder.RawOrder.Owner, market.lrcAddress); nil != err {
					log.Errorf("err:%s", err.Error())
					continue
				}
				if a2BTokenS, err = market.matcher.getAccountBalance(a2BOrder.RawOrder.Owner, a2BOrder.RawOrder.TokenS); nil != err {
					log.Errorf("err:%s", err.Error())
					continue
				}
				filledOrders = append(filledOrders, miner.ConvertOrderStateToFilledOrder(*a2BOrder, a2BLrcToken.Available(), a2BTokenS.Available()))

				if b2ALrcToken, err = market.matcher.getAccountBalance(b2AOrder.RawOrder.Owner, market.lrcAddress); nil != err {
					log.Errorf("err:%s", err.Error())
					continue
				}
				if b2ATokenS, err = market.matcher.getAccountBalance(b2AOrder.RawOrder.Owner, b2AOrder.RawOrder.TokenS); nil != err {
					log.Errorf("err:%s", err.Error())
					continue
				}
				filledOrders = append(filledOrders, miner.ConvertOrderStateToFilledOrder(*b2AOrder, b2ALrcToken.Available(), b2ATokenS.Available()))

				ringTmp := miner.NewRing(filledOrders)
				market.matcher.evaluator.ComputeRing(ringTmp)
				ringForSubmitTmp, err := market.matcher.submitter.GenerateRingSubmitInfo(ringTmp)
				if nil != err {
					log.Errorf("err: %s", err.Error())
				} else {
					if nil == ringForSubmit || ringForSubmit.Received.Cmp(ringForSubmitTmp.Received) < 0 {
						ringForSubmit = ringForSubmitTmp
					}
				}
			}
		}

		//对每个order标记已匹配以及减去已匹配的金额
		if nil != ringForSubmit {
			for _, filledOrder := range ringForSubmit.RawRing.Orders {
				orderState := market.reduceAmountAfterFilled(filledOrder)
				matchedOrderHashes[filledOrder.OrderState.RawOrder.Hash] = market.om.IsOrderFullFinished(orderState)
				market.matcher.addMatchedOrder(filledOrder, ringForSubmit.RawRing.Hash)
			}
			ringStates = append(ringStates, ringForSubmit)
		}
	}

	for orderHash, _ := range market.AtoBOrders {
		if fullFilled, exists := matchedOrderHashes[orderHash]; !exists || fullFilled {
			market.AtoBOrderHashesExcludeNextRound = append(market.AtoBOrderHashesExcludeNextRound, orderHash)
		}
	}

	for orderHash, _ := range market.BtoAOrders {
		if fullFilled, exists := matchedOrderHashes[orderHash]; !exists || fullFilled {
			market.BtoAOrderHashesExcludeNextRound = append(market.BtoAOrderHashesExcludeNextRound, orderHash)
		}
	}
	eventemitter.Emit(eventemitter.Miner_NewRing, ringStates)
}

func (matcher *TimingMatcher) afterSubmit(eventData eventemitter.EventData) error {
	matcher.mtx.Lock()
	defer matcher.mtx.Unlock()

	e := eventData.(*types.RingMinedEvent)
	ringHash := e.Ringhash
	if ringState, ok := matcher.MinedRings[ringHash]; ok {
		delete(matcher.MinedRings, ringHash)
		for _, orderHash := range ringState.orderHashes {
			if minedState, ok := matcher.MatchedOrders[orderHash]; ok {
				if len(minedState.rounds) <= 1 {
					delete(matcher.MatchedOrders, orderHash)
				} else {
					for idx, s := range minedState.rounds {
						if s.ringHash == ringHash {
							round1 := append(minedState.rounds[:idx], minedState.rounds[idx+1:]...)
							minedState.rounds = round1
						}
					}
				}
			}
		}
	}
	return nil
}

func (matcher *TimingMatcher) Stop() {
	eventemitter.Un(eventemitter.Miner_RingMined, matcher.afterSubmitWatcher)
	eventemitter.Un(eventemitter.Miner_RingSubmitFailed, matcher.afterSubmitWatcher)
	eventemitter.Un(eventemitter.Block_New, matcher.blockTriger)
}

func (matcher *TimingMatcher) addMatchedOrder(filledOrder *types.FilledOrder, ringiHash common.Hash) {
	matcher.mtx.Lock()
	defer matcher.mtx.Unlock()

	var matchState *OrderMatchState
	if matchState1, ok := matcher.MatchedOrders[filledOrder.OrderState.RawOrder.Hash]; !ok {
		matchState = &OrderMatchState{}
		matchState.orderState = filledOrder.OrderState
		matchState.rounds = []*RoundState{}
	} else {
		matchState = matchState1
	}

	roundState := &RoundState{
		round:          matcher.lastBlockNumber,
		ringHash:       ringiHash,
		matchedAmountB: filledOrder.FillAmountB,
		matchedAmountS: filledOrder.FillAmountS,
	}

	matchState.rounds = append(matchState.rounds, roundState)
	matcher.MatchedOrders[filledOrder.OrderState.RawOrder.Hash] = matchState
}

func intFromRat(rat *big.Rat) *big.Int {
	return new(big.Int).Div(rat.Num(), rat.Denom())
}
