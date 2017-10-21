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

package orderbook

import (
	"encoding/json"
	"errors"
	"github.com/Loopring/ringminer/config"
	"github.com/Loopring/ringminer/db"
	"github.com/Loopring/ringminer/log"
	"github.com/Loopring/ringminer/types"
	"math/big"
	"sync"
)

/**
todo:
1. filter
2. chain event
3. 事件执行到第几个块等信息数据
4. 订单完成的标志，以及需要发送到miner
*/

const (
	FINISH_TABLE_NAME  = "finished"
	PARTIAL_TABLE_NAME = "partial"
)

type Whisper struct {
	PeerOrderChan   chan *types.Order
	EngineOrderChan chan *types.OrderState
	ChainOrderChan  chan *types.OrderState
}

type OrderBook struct {
	options       config.OrderBookOptions
	commOpts      config.CommonOptions
	filters       []Filter
	db            db.Database
	finishTable   db.Database
	partialTable  db.Database
	whisper       *Whisper
	lock          sync.RWMutex
	minAmount     *big.Int
}

func NewOrderBook(options config.OrderBookOptions, commOpts config.CommonOptions, database db.Database, whisper *Whisper) *OrderBook {
	ob := &OrderBook{}

	ob.options = options
	ob.commOpts = commOpts
	ob.db = database
	ob.finishTable = db.NewTable(database, FINISH_TABLE_NAME)
	ob.partialTable = db.NewTable(database, PARTIAL_TABLE_NAME)
	ob.whisper = whisper

	//todo:filters init
	filters := []Filter{}
	baseFilter := &BaseFilter{MinLrcFee: big.NewInt(options.Filters.BaseFilter.MinLrcFee)}
	filters = append(filters, baseFilter)
	tokenSFilter := &TokenSFilter{}
	tokenBFilter := &TokenBFilter{}

	filters = append(filters, tokenSFilter)
	filters = append(filters, tokenBFilter)

	return ob
}

func (ob *OrderBook) recoverOrder() error {
	iterator := ob.partialTable.NewIterator(nil, nil)
	for iterator.Next() {
		dataBytes := iterator.Value()
		state := &types.OrderState{}
		if err := json.Unmarshal(dataBytes, state); nil != err {
			log.Errorf("err:%s", err.Error())
		} else {
			ob.whisper.EngineOrderChan <- state
		}
	}
	return nil
}

func (ob *OrderBook) filter(o *types.Order) (bool, error) {
	valid := true
	var err error
	for _, filter := range ob.filters {
		valid, err = filter.filter(o)
		if !valid {
			return valid, err
		}
	}
	return valid, nil
}

// Start start orderbook as a service
func (ob *OrderBook) Start() {
	//ob.recoverOrder()

	go func() {
		for {
			select {
			case ord := <-ob.whisper.PeerOrderChan:
				ob.peerOrderHook(ord)
			case ord := <-ob.whisper.ChainOrderChan:
				ob.chainOrderHook(ord)
			}
		}
	}()
}

func (ob *OrderBook) Stop() {
	ob.lock.Lock()
	defer ob.lock.Unlock()

	ob.finishTable.Close()
	ob.partialTable.Close()
	//ob.db.Close()
}

// 来自ipfs的新订单
// 所有来自ipfs的订单都是新订单
func (ob *OrderBook) peerOrderHook(ord *types.Order) {
	ob.lock.Lock()
	defer ob.lock.Unlock()

	log.Debugf("accept data from peer:%s", ord.Protocol.Hex())
	if valid, err := ob.filter(ord); !valid {
		log.Errorf("receive order but valid failed:%s", err.Error())
	}

	state := &types.OrderState{}
	state.RawOrder = *ord
	state.RawOrder.Hash = ord.GenerateHash()

	// 之前从未存储过
	if _, _, err := ob.getOrder(state.RawOrder.Hash); err != nil {
		log.Debugf("ipfs new order hash:%s", state.RawOrder.Hash.Hex())
		vd := types.VersionData{}
		vd.RemainedAmountB = state.RawOrder.AmountB
		vd.RemainedAmountS = state.RawOrder.AmountS
		vd.Status = types.ORDER_NEW

		state.States = append(state.States, vd)
		if bs, err := json.Marshal(state); err != nil {
			log.Errorf("ipfs order marshal error:%s", err.Error())
		} else {
			ob.partialTable.Put(state.RawOrder.Hash.Bytes(), bs)
			//ob.whisper.EngineOrderChan <- state
		}
	}
}

// 处理来自eth网络的evt/transaction转换后的orderState
// 如果之前没有存储，那么应该等到eth网络监听到transaction并解析成相应的order再处理
// 如果之前已经存储，那么应该直接处理并发送到miner
func (ob *OrderBook) chainOrderHook(ord *types.OrderState) {
	log.Debugf("accept data from block chain:%s", ord.RawOrder.Protocol.Hex())

	ob.lock.Lock()
	defer ob.lock.Unlock()

	ord, tn, err := ob.getOrder(ord.RawOrder.Hash)

	if err == nil {
		log.Debugf("chain order exist in:%s", tn)

		// 该orderState已经添加了types.versionData

		// todo:判断订单状态

		// todo:根据订单状态从部分完成表转移到完全完成表
		if vd, errvd := ord.LatestVersion(); errvd == nil {
			if vd.Status == types.ORDER_FINISHED || vd.Status == types.ORDER_CANCEL {
				ob.moveOrder(ord)
			}
		}

		// 发送到miner
		ob.whisper.EngineOrderChan <- ord
	}
}

// GetOrder get single order with hash
func (ob *OrderBook) GetOrder(id types.Hash) (*types.OrderState, error) {
	ord, _, err := ob.getOrder(id)
	return ord, err
}

func (ob *OrderBook) getOrder(id types.Hash) (*types.OrderState, string, error) {
	var (
		value []byte
		err   error
		tn    string
		ord   types.OrderState
	)

	if value, err = ob.partialTable.Get(id.Bytes()); err != nil {
		value, err = ob.finishTable.Get(id.Bytes())
		if err != nil {
			return nil, "", errors.New("order do not exist")
		} else {
			tn = FINISH_TABLE_NAME
		}
	} else {
		tn = PARTIAL_TABLE_NAME
	}

	err = json.Unmarshal(value, &ord)
	if err != nil {
		return nil, tn, err
	}

	return &ord, tn, nil
}

// GetOrders get orders from persistence database
func (ob *OrderBook) GetOrders() {

}

// moveOrder move order when partial finished order fully exchanged
func (ob *OrderBook) moveOrder(odw *types.OrderState) error {
	key := odw.RawOrder.Hash.Bytes()
	value, err := json.Marshal(odw)
	if err != nil {
		return err
	}
	ob.partialTable.Delete(key)
	ob.finishTable.Put(key, value)
	return nil
}

// isFinished judge order state
func isFinished(odw *types.OrderState) bool {
	//if odw.RawOrder.
	return true
}
