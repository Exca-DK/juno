package rpcv8

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/NethermindEth/juno/blockchain"
	"github.com/NethermindEth/juno/core"
	"github.com/NethermindEth/juno/core/felt"
	"github.com/NethermindEth/juno/feed"
	"github.com/NethermindEth/juno/jsonrpc"
	"github.com/NethermindEth/juno/rpc/rpccore"
	"github.com/NethermindEth/juno/sync"
	"github.com/NethermindEth/juno/utils"
	"github.com/sourcegraph/conc"
)

const subscribeEventsChunkSize = 1024

// The function signature of SubscribeTransactionStatus cannot be changed since the jsonrpc package maps the number
// of argument in the function to the parameters in the starknet spec, therefore, the following variables are not passed
// as arguments, and they can be modified in the test to make them run faster.
var (
	subscribeTxStatusTimeout        = 5 * time.Minute
	subscribeTxStatusTickerDuration = 5 * time.Second
)

var (
	_ BlockIdentifier = (*SubscriptionBlockID)(nil)
	_ BlockIdentifier = (*BlockID)(nil)
)

type SubscriptionResponse struct {
	Version string `json:"jsonrpc"`
	Method  string `json:"method"`
	Params  any    `json:"params"`
}

type BlockIdentifier interface {
	IsLatest() bool
	IsPending() bool
	GetHash() *felt.Felt
	GetNumber() uint64
	UnmarshalJSON(data []byte) error
}

// As per the spec, this is the same as BlockID, but without `pending`
type SubscriptionBlockID struct {
	Latest bool
	Hash   *felt.Felt
	Number uint64
}

func (b *SubscriptionBlockID) IsLatest() bool {
	return b.Latest
}

func (b *SubscriptionBlockID) IsPending() bool {
	return false // Subscription blocks can't be pending
}

func (b *SubscriptionBlockID) GetHash() *felt.Felt {
	return b.Hash
}

func (b *SubscriptionBlockID) GetNumber() uint64 {
	return b.Number
}

func (b *SubscriptionBlockID) UnmarshalJSON(data []byte) error {
	if string(data) == `"latest"` {
		b.Latest = true
	} else {
		jsonObject := make(map[string]json.RawMessage)
		if err := json.Unmarshal(data, &jsonObject); err != nil {
			return err
		}
		hash, ok := jsonObject["block_hash"]
		if ok {
			b.Hash = new(felt.Felt)
			return json.Unmarshal(hash, b.Hash)
		}

		number, ok := jsonObject["block_number"]
		if ok {
			return json.Unmarshal(number, &b.Number)
		}

		return errors.New("cannot unmarshal block id")
	}
	return nil
}

// Currently the order of transactions is deterministic, so the transaction always execute on a deterministic state
// Therefore, the emitted events are deterministic and we can use the transaction hash and event index to identify.
type SentEvent struct {
	TransactionHash felt.Felt
	EventIndex      int
}

// SubscribeEvents creates a WebSocket stream which will fire events for new Starknet events with applied filters
func (h *Handler) SubscribeEvents(ctx context.Context, fromAddr *felt.Felt, keys [][]felt.Felt,
	blockID *SubscriptionBlockID,
) (SubscriptionID, *jsonrpc.Error) {
	w, ok := jsonrpc.ConnFromContext(ctx)
	if !ok {
		return 0, jsonrpc.Err(jsonrpc.MethodNotFound, nil)
	}

	lenKeys := len(keys)
	for _, k := range keys {
		lenKeys += len(k)
	}
	if lenKeys > rpccore.MaxEventFilterKeys {
		return 0, rpccore.ErrTooManyKeysInFilter
	}

	requestedHeader, headHeader, rpcErr := h.resolveBlockRange(blockID)
	if rpcErr != nil {
		return 0, rpcErr
	}

	id := h.idgen()
	subscriptionCtx, subscriptionCtxCancel := context.WithCancel(ctx)
	sub := &subscription{
		cancel: subscriptionCtxCancel,
		conn:   w,
	}
	h.subscriptions.Store(id, sub)

	newHeadsSub := h.newHeads.SubscribeKeepLast()
	reorgSub := h.reorgs.SubscribeKeepLast() // as per the spec, reorgs are also sent in the events subscription
	pendingSub := h.pendingBlock.SubscribeKeepLast()
	sub.wg.Go(func() {
		defer func() {
			h.unsubscribe(sub, id)
			newHeadsSub.Unsubscribe()
			reorgSub.Unsubscribe()
			pendingSub.Unsubscribe()
		}()

		// We still need to run this separately outside of the loop to capture the latest block before subscription.
		h.processEvents(subscriptionCtx, w, id, requestedHeader.Number, headHeader.Number, fromAddr, keys, nil)

		nextBlock := headHeader.Number + 1
		eventsPreviouslySent := make(map[SentEvent]struct{})

		for {
			select {
			case <-subscriptionCtx.Done():
				return
			case reorg := <-reorgSub.Recv():
				if err := sendReorg(w, reorg, id); err != nil {
					h.log.Warnw("Error sending reorg", "err", err)
					return
				}
				nextBlock = reorg.StartBlockNum
			case head := <-newHeadsSub.Recv():
				h.processEvents(subscriptionCtx, w, id, nextBlock, head.Number, fromAddr, keys, eventsPreviouslySent)
				nextBlock = head.Number + 1
			case <-pendingSub.Recv():
				h.processEvents(subscriptionCtx, w, id, nextBlock, nextBlock, fromAddr, keys, eventsPreviouslySent)
			}
		}
	})

	return SubscriptionID(id), nil
}

// SubscribeTransactionStatus subscribes to status changes of a transaction. It checks for updates each time a new block is added.
// Later updates are sent only when the transaction status changes.
// The optional block_id parameter is ignored, as status changes are not stored and historical data cannot be sent.
//
//nolint:gocyclo,funlen
func (h *Handler) SubscribeTransactionStatus(ctx context.Context, txHash felt.Felt) (SubscriptionID,
	*jsonrpc.Error,
) {
	w, ok := jsonrpc.ConnFromContext(ctx)
	if !ok {
		return 0, jsonrpc.Err(jsonrpc.MethodNotFound, nil)
	}

	// If the error is transaction not found that means the transaction has not been submitted to the feeder gateway,
	// therefore, we need to wait for a specified time and at regular interval check if the transaction has been found.
	// If the transaction is found during the timout expiry, then we continue to keep track of its status otherwise the
	// websocket connection is closed after the expiry.
	curStatus, rpcErr := h.TransactionStatus(ctx, txHash)
	if rpcErr != nil {
		if rpcErr != rpccore.ErrTxnHashNotFound {
			return 0, rpcErr
		}

		timeout := time.NewTimer(subscribeTxStatusTimeout)
		ticker := time.NewTicker(subscribeTxStatusTickerDuration)

	txNotFoundLoop:
		for {
			select {
			case <-timeout.C:
				ticker.Stop()
				return 0, rpcErr
			case <-ticker.C:
				curStatus, rpcErr = h.TransactionStatus(ctx, txHash)
				if rpcErr != nil {
					if rpcErr != rpccore.ErrTxnHashNotFound {
						return 0, rpcErr
					}
					continue
				}
				timeout.Stop()
				break txNotFoundLoop
			}
		}
	}

	id := h.idgen()
	subscriptionCtx, subscriptionCtxCancel := context.WithCancel(ctx)
	sub := &subscription{
		cancel: subscriptionCtxCancel,
		conn:   w,
	}
	h.subscriptions.Store(id, sub)

	pendingSub := h.pendingBlock.Subscribe()
	l1HeadSub := h.l1Heads.Subscribe()
	reorgSub := h.reorgs.Subscribe()

	sub.wg.Go(func() {
		defer func() {
			h.unsubscribe(sub, id)
			pendingSub.Unsubscribe()
			l1HeadSub.Unsubscribe()
			reorgSub.Unsubscribe()
		}()

		var wg conc.WaitGroup

		err := sendTxnStatus(w, SubscriptionTransactionStatus{&txHash, *curStatus}, id)
		if err != nil {
			h.log.Errorw("Error while sending Txn status", "txHash", txHash, "err", err)
			return
		}

		// Check if the requested transaction is already final.
		// A transaction is considered to be final if it has been rejected or accepted on l1
		if curStatus.Finality == TxnStatusRejected || curStatus.Finality == TxnStatusAcceptedOnL1 {
			return
		}

		// At this point, the transaction has not reached finality.
		wg.Go(func() {
			for {
				select {
				case <-subscriptionCtx.Done():
					return
				case <-pendingSub.Recv():
					// Pending block has been updated, hence, check if transaction has reached l2 finality, if not,
					// check feeder.
					// TransactionStatus calls TransactionReceiptByHash which checks the pending block if it contains
					// a transaction and if it does, then the appropriate transaction status is returned.
					// Therefore, we don't need to explicitly find the transaction in the pending block received from
					// the pendingSub.
					if curStatus.Finality < TxnStatusAcceptedOnL2 {
						prevStatus := curStatus
						curStatus, rpcErr = h.TransactionStatus(subscriptionCtx, txHash)

						if rpcErr != nil {
							h.log.Errorw("Error while getting Txn status", "txHash", txHash, "err", rpcErr)
							return
						}

						if curStatus.Finality > prevStatus.Finality {
							err := sendTxnStatus(w, SubscriptionTransactionStatus{&txHash, *curStatus}, id)
							if err != nil {
								h.log.Errorw("Error while sending Txn status", "txHash", txHash, "err", err)
								return
							}
							if curStatus.Finality == TxnStatusRejected || curStatus.Finality == TxnStatusAcceptedOnL1 {
								return
							}
						}
					}
				case <-l1HeadSub.Recv():
					receipt, rpcErr := h.TransactionReceiptByHash(txHash)
					if rpcErr != nil {
						h.log.Errorw("Error while getting Receipt", "txHash", txHash, "err", rpcErr)
						return
					}

					if receipt.FinalityStatus == TxnAcceptedOnL1 {
						s := &TransactionStatus{
							Finality:      TxnStatus(receipt.FinalityStatus),
							Execution:     receipt.ExecutionStatus,
							FailureReason: receipt.RevertReason,
						}

						err := sendTxnStatus(w, SubscriptionTransactionStatus{&txHash, *s}, id)
						if err != nil {
							h.log.Errorw("Error while sending Txn status", "txHash", txHash, "err", err)
						}
						return
					}
				}
			}
		})

		wg.Go(func() {
			h.processReorgs(subscriptionCtx, reorgSub, w, id)
		})

		wg.Wait()
	})

	return SubscriptionID(id), nil
}

func (h *Handler) processEvents(ctx context.Context, w jsonrpc.Conn, id, from, to uint64, fromAddr *felt.Felt,
	keys [][]felt.Felt, eventsPreviouslySent map[SentEvent]struct{},
) {
	filter, err := h.bcReader.EventFilter(fromAddr, keys)
	if err != nil {
		h.log.Warnw("Error creating event filter", "err", err)
		return
	}

	defer h.callAndLogErr(filter.Close, "Error closing event filter in events subscription")

	if err = setEventFilterRange(filter, &BlockID{Number: from}, &BlockID{Number: to}, to); err != nil {
		h.log.Warnw("Error setting event filter range", "err", err)
		return
	}

	filteredEvents, cToken, err := filter.Events(nil, subscribeEventsChunkSize)
	if err != nil {
		h.log.Warnw("Error filtering events", "err", err)
		return
	}

	err = sendEvents(ctx, w, filteredEvents, eventsPreviouslySent, id)
	if err != nil {
		h.log.Warnw("Error sending events", "err", err)
		return
	}

	for cToken != nil {
		filteredEvents, cToken, err = filter.Events(cToken, subscribeEventsChunkSize)
		if err != nil {
			h.log.Warnw("Error filtering events", "err", err)
			return
		}

		err = sendEvents(ctx, w, filteredEvents, eventsPreviouslySent, id)
		if err != nil {
			h.log.Warnw("Error sending events", "err", err)
			return
		}
	}
}

func sendEvents(ctx context.Context, w jsonrpc.Conn, events []*blockchain.FilteredEvent,
	eventsPreviouslySent map[SentEvent]struct{}, id uint64,
) error {
	for _, event := range events {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			if eventsPreviouslySent != nil {
				sentEvent := SentEvent{
					TransactionHash: *event.TransactionHash,
					EventIndex:      event.EventIndex,
				}
				if _, ok := eventsPreviouslySent[sentEvent]; ok {
					continue
				}
				// This describe the lifecycle of SentEvent.
				// It's added when the event is received from a pending block.
				// It's deleted when the event is received from a head block.
				if isPending := event.BlockHash == nil; isPending {
					eventsPreviouslySent[sentEvent] = struct{}{}
				} else {
					delete(eventsPreviouslySent, sentEvent)
				}
			}

			emittedEvent := &EmittedEvent{
				BlockNumber:     event.BlockNumber, // This always be filled as subscribeEvents cannot be called on pending block
				BlockHash:       event.BlockHash,
				TransactionHash: event.TransactionHash,
				Event: &Event{
					From: event.From,
					Keys: event.Keys,
					Data: event.Data,
				},
			}

			if err := sendResponse("starknet_subscriptionEvents", w, id, emittedEvent); err != nil {
				return err
			}
		}
	}
	return nil
}

// SubscribeNewHeads creates a WebSocket stream which will fire events when a new block header is added.
func (h *Handler) SubscribeNewHeads(ctx context.Context, blockID *SubscriptionBlockID) (SubscriptionID, *jsonrpc.Error) {
	w, ok := jsonrpc.ConnFromContext(ctx)
	if !ok {
		return 0, jsonrpc.Err(jsonrpc.MethodNotFound, nil)
	}

	startHeader, latestHeader, rpcErr := h.resolveBlockRange(blockID)
	if rpcErr != nil {
		return 0, rpcErr
	}

	id := h.idgen()
	subscriptionCtx, subscriptionCtxCancel := context.WithCancel(ctx)
	sub := &subscription{
		cancel: subscriptionCtxCancel,
		conn:   w,
	}
	h.subscriptions.Store(id, sub)

	newHeadsSub := h.newHeads.Subscribe()
	reorgSub := h.reorgs.Subscribe() // as per the spec, reorgs are also sent in the new heads subscription
	sub.wg.Go(func() {
		defer func() {
			h.unsubscribe(sub, id)
			newHeadsSub.Unsubscribe()
			reorgSub.Unsubscribe()
		}()

		var wg conc.WaitGroup

		wg.Go(func() {
			if err := h.sendHistoricalHeaders(subscriptionCtx, startHeader, latestHeader, w, id); err != nil {
				h.log.Errorw("Error sending old headers", "err", err)
				return
			}
		})

		wg.Go(func() {
			h.processReorgs(subscriptionCtx, reorgSub, w, id)
		})

		wg.Go(func() {
			h.processNewHeaders(subscriptionCtx, newHeadsSub, w, id)
		})

		wg.Wait()
	})

	return SubscriptionID(id), nil
}

// SubscribePendingTxs creates a WebSocket stream which will fire events when a new pending transaction is added.
// The getDetails flag controls if the response will contain the transaction details or just the transaction hashes.
// The senderAddr flag is used to filter the transactions by sender address.
func (h *Handler) SubscribePendingTxs(ctx context.Context, getDetails *bool, senderAddr []felt.Felt) (SubscriptionID, *jsonrpc.Error) {
	w, ok := jsonrpc.ConnFromContext(ctx)
	if !ok {
		return 0, jsonrpc.Err(jsonrpc.MethodNotFound, nil)
	}

	if len(senderAddr) > rpccore.MaxEventFilterKeys {
		return 0, rpccore.ErrTooManyAddressesInFilter
	}

	id := h.idgen()
	subscriptionCtx, subscriptionCtxCancel := context.WithCancel(ctx)
	sub := &subscription{
		cancel: subscriptionCtxCancel,
		conn:   w,
	}
	h.subscriptions.Store(id, sub)

	pendingSub := h.pendingBlock.Subscribe()
	sub.wg.Go(func() {
		defer func() {
			h.unsubscribe(sub, id)
			pendingSub.Unsubscribe()
		}()

		h.processPendingTxs(subscriptionCtx, getDetails != nil && *getDetails, senderAddr, pendingSub, w, id)
	})

	return SubscriptionID(id), nil
}

func (h *Handler) processPendingTxs(ctx context.Context, getDetails bool, senderAddr []felt.Felt,
	pendingSub *feed.Subscription[*core.Block], w jsonrpc.Conn, id uint64,
) {
	for {
		select {
		case <-ctx.Done():
			return
		case pendingBlock := <-pendingSub.Recv():
			filteredTxs := h.filterTxs(pendingBlock.Transactions, getDetails, senderAddr)
			for _, filteredTxn := range filteredTxs {
				if err := sendPendingTxs(w, filteredTxn, id); err != nil {
					h.log.Warnw("Error sending pending transactions", "err", err)
					return
				}
			}
		}
	}
}

// filterTxs filters the transactions based on the getDetails flag.
// If getDetails is true, response will contain the transaction details.
// If getDetails is false, response will only contain the transaction hashes.
func (h *Handler) filterTxs(pendingTxs []core.Transaction, getDetails bool, senderAddr []felt.Felt) []any {
	if getDetails {
		return h.filterTxDetails(pendingTxs, senderAddr)
	}
	return h.filterTxHashes(pendingTxs, senderAddr)
}

func (h *Handler) filterTxDetails(pendingTxs []core.Transaction, senderAddr []felt.Felt) []any {
	filteredTxs := make([]any, 0, len(pendingTxs))
	for _, txn := range pendingTxs {
		if h.filterTxBySender(txn, senderAddr) {
			filteredTxs = append(filteredTxs, AdaptTransaction(txn))
		}
	}
	return filteredTxs
}

func (h *Handler) filterTxHashes(pendingTxs []core.Transaction, senderAddr []felt.Felt) []any {
	filteredTxHashes := make([]any, 0, len(pendingTxs))
	for _, txn := range pendingTxs {
		if h.filterTxBySender(txn, senderAddr) {
			filteredTxHashes = append(filteredTxHashes, txn.Hash())
		}
	}
	return filteredTxHashes
}

// filterTxBySender checks if the transaction is included in the sender address list.
// If the sender address list is empty, it will return true by default.
// If the sender address list is not empty, it will check if the transaction is an Invoke or Declare transaction
// and if the sender address is in the list. For other transaction types, it will by default return false.
func (h *Handler) filterTxBySender(txn core.Transaction, senderAddr []felt.Felt) bool {
	if len(senderAddr) == 0 {
		return true
	}

	switch t := txn.(type) {
	case *core.InvokeTransaction:
		for _, addr := range senderAddr {
			if t.SenderAddress.Equal(&addr) {
				return true
			}
		}
	case *core.DeclareTransaction:
		for _, addr := range senderAddr {
			if t.SenderAddress.Equal(&addr) {
				return true
			}
		}
	}

	return false
}

func sendPendingTxs(w jsonrpc.Conn, result any, id uint64) error {
	return sendResponse("starknet_subscriptionPendingTransactions", w, id, result)
}

// resolveBlockRange returns the start and latest headers based on the blockID.
// It will also do some sanity checks and return errors if the blockID is invalid.
func (h *Handler) resolveBlockRange(id BlockIdentifier) (*core.Header, *core.Header, *jsonrpc.Error) {
	latestHeader, err := h.bcReader.HeadsHeader()
	if err != nil {
		return nil, nil, rpccore.ErrInternal.CloneWithData(err.Error())
	}

	if utils.IsNil(id) {
		return latestHeader, latestHeader, nil
	}

	if id.IsLatest() {
		return latestHeader, latestHeader, nil
	}

	startHeader, rpcErr := h.blockHeaderByID(id)
	if rpcErr != nil {
		return nil, nil, rpcErr
	}

	if latestHeader.Number >= rpccore.MaxBlocksBack && startHeader.Number <= latestHeader.Number-rpccore.MaxBlocksBack {
		return nil, nil, rpccore.ErrTooManyBlocksBack
	}

	return startHeader, latestHeader, nil
}

// sendHistoricalHeaders sends a range of headers from the start header until the latest header
func (h *Handler) sendHistoricalHeaders(
	ctx context.Context,
	startHeader, latestHeader *core.Header,
	w jsonrpc.Conn,
	id uint64,
) error {
	var (
		err       error
		curHeader = startHeader
	)

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			if err := sendHeader(w, curHeader, id); err != nil {
				return err
			}

			if curHeader.Number == latestHeader.Number {
				return nil
			}

			curHeader, err = h.bcReader.BlockHeaderByNumber(curHeader.Number + 1)
			if err != nil {
				return err
			}
		}
	}
}

func (h *Handler) processNewHeaders(ctx context.Context, newHeadsSub *feed.Subscription[*core.Block], w jsonrpc.Conn, id uint64) {
	for {
		select {
		case <-ctx.Done():
			return
		case head := <-newHeadsSub.Recv():
			if err := sendHeader(w, head.Header, id); err != nil {
				h.log.Warnw("Error sending header", "err", err)
				return
			}
		}
	}
}

// sendHeader creates a request and sends it to the client
func sendHeader(w jsonrpc.Conn, header *core.Header, id uint64) error {
	return sendResponse("starknet_subscriptionNewHeads", w, id, adaptBlockHeader(header))
}

func (h *Handler) processReorgs(ctx context.Context, reorgSub *feed.Subscription[*sync.ReorgBlockRange], w jsonrpc.Conn, id uint64) {
	for {
		select {
		case <-ctx.Done():
			return
		case reorg := <-reorgSub.Recv():
			if err := sendReorg(w, reorg, id); err != nil {
				h.log.Warnw("Error sending reorg", "err", err)
				return
			}
		}
	}
}

type ReorgEvent struct {
	StartBlockHash *felt.Felt `json:"starting_block_hash"`
	StartBlockNum  uint64     `json:"starting_block_number"`
	EndBlockHash   *felt.Felt `json:"ending_block_hash"`
	EndBlockNum    uint64     `json:"ending_block_number"`
}

func sendReorg(w jsonrpc.Conn, reorg *sync.ReorgBlockRange, id uint64) error {
	return sendResponse("starknet_subscriptionReorg", w, id, &ReorgEvent{
		StartBlockHash: reorg.StartBlockHash,
		StartBlockNum:  reorg.StartBlockNum,
		EndBlockHash:   reorg.EndBlockHash,
		EndBlockNum:    reorg.EndBlockNum,
	})
}

func (h *Handler) Unsubscribe(ctx context.Context, id uint64) (bool, *jsonrpc.Error) {
	w, ok := jsonrpc.ConnFromContext(ctx)
	if !ok {
		return false, jsonrpc.Err(jsonrpc.MethodNotFound, nil)
	}
	sub, ok := h.subscriptions.Load(id)
	if !ok {
		return false, rpccore.ErrInvalidSubscriptionID
	}

	subs := sub.(*subscription)
	if !subs.conn.Equal(w) {
		return false, rpccore.ErrInvalidSubscriptionID
	}

	subs.cancel()
	subs.wg.Wait() // Let the subscription finish before responding.
	h.subscriptions.Delete(id)
	return true, nil
}

type SubscriptionTransactionStatus struct {
	TransactionHash *felt.Felt        `json:"transaction_hash"`
	Status          TransactionStatus `json:"status"`
}

// sendTxnStatus creates a response and sends it to the client
func sendTxnStatus(w jsonrpc.Conn, status SubscriptionTransactionStatus, id uint64) error {
	return sendResponse("starknet_subscriptionTransactionsStatus", w, id, status)
}

func sendResponse(method string, w jsonrpc.Conn, id uint64, result any) error {
	resp, err := json.Marshal(SubscriptionResponse{
		Version: "2.0",
		Method:  method,
		Params: map[string]any{
			"subscription_id": id,
			"result":          result,
		},
	})
	if err != nil {
		return err
	}
	_, err = w.Write(resp)
	return err
}
