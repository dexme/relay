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

package txmanager

import (
	"github.com/Loopring/relay/dao"
	"github.com/Loopring/relay/eventemiter"
	"github.com/Loopring/relay/log"
	"github.com/Loopring/relay/market"
	txtyp "github.com/Loopring/relay/txmanager/types"
	"github.com/Loopring/relay/types"
	"github.com/tendermint/go-crypto/keys/tx"
)

type TransactionManager struct {
	db                         dao.RdsService
	accountmanager             *market.AccountManager
	approveEventWatcher        *eventemitter.Watcher
	orderCancelledEventWatcher *eventemitter.Watcher
	cutoffAllEventWatcher      *eventemitter.Watcher
	cutoffPairEventWatcher     *eventemitter.Watcher
	wethDepositEventWatcher    *eventemitter.Watcher
	wethWithdrawalEventWatcher *eventemitter.Watcher
	transferEventWatcher       *eventemitter.Watcher
	ethTransferEventWatcher    *eventemitter.Watcher
	forkDetectedEventWatcher   *eventemitter.Watcher
}

func NewTxManager(db dao.RdsService, accountmanager *market.AccountManager) TransactionManager {
	var tm TransactionManager
	tm.db = db
	tm.accountmanager = accountmanager

	return tm
}

// Start start orderbook as a service
func (tm *TransactionManager) Start() {
	tm.approveEventWatcher = &eventemitter.Watcher{Concurrent: false, Handle: tm.SaveApproveEvent}
	eventemitter.On(eventemitter.Approve, tm.approveEventWatcher)

	tm.orderCancelledEventWatcher = &eventemitter.Watcher{Concurrent: false, Handle: tm.SaveOrderCancelledEvent}
	eventemitter.On(eventemitter.CancelOrder, tm.orderCancelledEventWatcher)

	tm.cutoffAllEventWatcher = &eventemitter.Watcher{Concurrent: false, Handle: tm.SaveCutoffAllEvent}
	eventemitter.On(eventemitter.CutoffAll, tm.cutoffAllEventWatcher)

	tm.cutoffPairEventWatcher = &eventemitter.Watcher{Concurrent: false, Handle: tm.SaveCutoffPairEvent}
	eventemitter.On(eventemitter.CutoffPair, tm.cutoffPairEventWatcher)

	tm.wethDepositEventWatcher = &eventemitter.Watcher{Concurrent: false, Handle: tm.SaveWethDepositEvent}
	eventemitter.On(eventemitter.WethDeposit, tm.wethDepositEventWatcher)

	tm.wethWithdrawalEventWatcher = &eventemitter.Watcher{Concurrent: false, Handle: tm.SaveWethWithdrawalEvent}
	eventemitter.On(eventemitter.WethWithdrawal, tm.wethWithdrawalEventWatcher)

	tm.transferEventWatcher = &eventemitter.Watcher{Concurrent: false, Handle: tm.SaveTransferEvent}
	eventemitter.On(eventemitter.Transfer, tm.transferEventWatcher)

	tm.ethTransferEventWatcher = &eventemitter.Watcher{Concurrent: false, Handle: tm.SaveEthTransferEvent}
	eventemitter.On(eventemitter.EthTransferEvent, tm.ethTransferEventWatcher)

	tm.forkDetectedEventWatcher = &eventemitter.Watcher{Concurrent: false, Handle: tm.ForkProcess}
	eventemitter.On(eventemitter.ChainForkDetected, tm.forkDetectedEventWatcher)
}

func (tm *TransactionManager) Stop() {
	eventemitter.Un(eventemitter.Approve, tm.approveEventWatcher)
	eventemitter.Un(eventemitter.CancelOrder, tm.orderCancelledEventWatcher)
	eventemitter.Un(eventemitter.CutoffAll, tm.cutoffAllEventWatcher)
	eventemitter.Un(eventemitter.CutoffPair, tm.cutoffPairEventWatcher)
	eventemitter.Un(eventemitter.WethDeposit, tm.wethDepositEventWatcher)
	eventemitter.Un(eventemitter.WethWithdrawal, tm.wethWithdrawalEventWatcher)
	eventemitter.Un(eventemitter.Transfer, tm.transferEventWatcher)
	eventemitter.Un(eventemitter.EthTransferEvent, tm.ethTransferEventWatcher)
	eventemitter.Un(eventemitter.ChainForkDetected, tm.forkDetectedEventWatcher)
}

func (tm *TransactionManager) ForkProcess(input eventemitter.EventData) error {
	log.Debugf("txmanager,processing chain fork......")

	tm.Stop()
	forkEvent := input.(*types.ForkedEvent)
	from := forkEvent.ForkBlock.Int64()
	to := forkEvent.DetectedBlock.Int64()
	if err := tm.db.RollBackTransaction(from, to); err != nil {
		log.Fatalf("txmanager,process fork error:%s", err.Error())
	}
	tm.Start()

	return nil
}

func (tm *TransactionManager) SaveApproveEvent(input eventemitter.EventData) error {
	event := input.(*types.ApprovalEvent)

	var (
		entity txtyp.TransactionEntity
		list   []txtyp.TransactionView
	)

	entity.FromApproveEvent(event)
	view, err := txtyp.ApproveView(event)
	if err != nil {
		return err
	}
	list = append(list, view)

	return tm.saveTransaction(&entity, list)
}

func (tm *TransactionManager) SaveOrderCancelledEvent(input eventemitter.EventData) error {
	event := input.(*types.OrderCancelledEvent)

	var (
		entity txtyp.TransactionEntity
		list   []txtyp.TransactionView
	)

	entity.FromCancelEvent(event)
	view := txtyp.CancelView(event)
	list = append(list, view)

	return tm.saveTransaction(&entity, list)
}

func (tm *TransactionManager) SaveCutoffAllEvent(input eventemitter.EventData) error {
	event := input.(*types.CutoffEvent)

	var (
		entity txtyp.TransactionEntity
		list   []txtyp.TransactionView
	)
	entity.FromCutoffEvent(event)
	view := txtyp.CutoffView(event)
	list = append(list, view)

	return tm.saveTransaction(&entity, list)
}

func (tm *TransactionManager) SaveCutoffPairEvent(input eventemitter.EventData) error {
	event := input.(*types.CutoffPairEvent)

	var (
		entity txtyp.TransactionEntity
		list   []txtyp.TransactionView
	)

	entity.FromCutoffPairEvent(event)
	view := txtyp.CutoffPairView(event)
	list = append(list, view)

	return tm.saveTransaction(&entity, list)
}

func (tm *TransactionManager) SaveWethDepositEvent(input eventemitter.EventData) error {
	event := input.(*types.WethDepositEvent)

	var entity txtyp.TransactionEntity
	entity.FromWethDepositEvent(event)
	list := txtyp.WethDepositView(event)

	return tm.saveTransaction(&entity, list)
}

func (tm *TransactionManager) SaveWethWithdrawalEvent(input eventemitter.EventData) error {
	event := input.(*types.WethWithdrawalEvent)

	var entity txtyp.TransactionEntity
	entity.FromWethWithdrawalEvent(event)
	list := txtyp.WethWithdrawalView(event)

	return tm.saveTransaction(&entity, list)
}

func (tm *TransactionManager) SaveTransferEvent(input eventemitter.EventData) error {
	event := input.(*types.TransferEvent)

	var entity txtyp.TransactionEntity
	entity.FromTransferEvent(event)
	list, err := txtyp.TransferView(event)
	if err != nil {
		return err
	}

	return tm.saveTransaction(&entity, list)
}

// 普通的transaction
// 当value大于0时认为是eth转账
// 当value等于0时认为是调用系统不支持的合约,默认使用fromTransferEvent/send type为unsupported_contract
func (tm *TransactionManager) SaveEthTransferEvent(input eventemitter.EventData) error {
	event := input.(*types.TransferEvent)

	var entity txtyp.TransactionEntity
	entity.FromEthTransferEvent(event)
	list := txtyp.EthTransferView(event)

	return tm.saveTransaction(&entity, list)
}

func (tm *TransactionManager) SaveOrderFilledEvent(input eventemitter.EventData) error {
	// todo

	return nil
}

func (tm *TransactionManager) saveTransaction(tx *txtyp.TransactionEntity, list []txtyp.TransactionView) error {
	if err := tm.saveEntity(tx, list); err != nil {
		return err
	}

	if tx.Status == types.TX_STATUS_PENDING {
		if err := tm.savePendingEntity(tx); err != nil {
			return err
		}
	} else {
		if err := tm.saveMinedEntity(tx); err != nil {
			return err
		}
	}

	for _, v := range list {
		if tx.Status == types.TX_STATUS_PENDING {
			if err := tm.savePendingView(&v); err != nil {
				return err
			}
		} else {
			if err := tm.saveMinedView(&v); err != nil {
				return err
			}
		}
	}

	return nil
}

//
func (tm *TransactionManager) saveEntity(tx *txtyp.TransactionEntity, viewList []txtyp.TransactionView) error {
	if !tm.validateEntity(viewList) {
		return nil
	}

	// 如果
	// todo save
}

func (tm *TransactionManager) savePendingEntity(tx *txtyp.TransactionEntity) error {

}

func (tm *TransactionManager) saveMinedEntity(tx *txtyp.TransactionEntity) error {

}

func (tm *TransactionManager) savePendingView(tx *txtyp.TransactionView) error {
	// find transaction which have the same hash, raw_from and nonce
	if _, err := tm.db.GetPendingTransaction(tx.TxHash, tx.RawFrom, tx.Nonce); err == nil {
		return nil
	}

	return tm.addTransaction(tx)
}

func (tm *TransactionManager) saveMinedView(tx *txtyp.TransactionView) error {
	// get transactions by sender and tx.nonce
	list, _ := tm.db.GetTransactionsBySenderNonce(tx.RawFrom, tx.Nonce)

	// insert new data if list is empty
	if len(list) == 0 {
		return tm.addTransaction(tx)
	}

	// judge have any pending tx and the same tx
	hasPendingTx := false
	hasSameTx := false
	for _, v := range list {
		var latest types.Transaction
		v.ConvertUp(&latest)

		if latest.Status == types.TX_STATUS_PENDING {
			hasPendingTx = true
		} else if latest.Compare(tx) {
			hasSameTx = true
		}
	}

	// delete pending tx
	if hasPendingTx {
		tm.db.DeletePendingTransactions(tx.RawFrom, tx.Nonce)
	}
	// check tx if exist
	if !hasSameTx {
		tm.addTransaction(tx)
	}
	return nil
}

func (tm *TransactionManager) addTransaction(tx *types.Transaction) error {
	var (
		latest dao.Transaction
		err    error
	)

	latest.ConvertDown(tx)
	err = tm.db.Add(&latest)

	if err == nil {
		eventemitter.Emit(eventemitter.TransactionEvent, tx)
	}

	return err
}

func (tm *TransactionManager) validateEntity(viewList []txtyp.TransactionView) bool {
	owners := txtyp.RelatedOwners(viewList)

	unlocked := false
	for _, v := range owners {
		if ok, _ := tm.accountmanager.HasUnlocked(v.Hex()); ok {
			unlocked = true
			break
		}
	}

	return unlocked
}

func (tm *TransactionManager) validateView(view *txtyp.TransactionView) bool {
	unlocked, _ := tm.accountmanager.HasUnlocked(view.Owner.Hex())
	return unlocked
}
