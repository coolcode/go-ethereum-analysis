// Copyright 2015 The github.com/go-ethereum-analysis Authors
// This file is part of the github.com/go-ethereum-analysis library.
//
// The github.com/go-ethereum-analysis library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The github.com/go-ethereum-analysis library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the github.com/go-ethereum-analysis library. If not, see <http://www.gnu.org/licenses/>.

// Package downloader contains the manual full chain synchronisation.
package downloader

import (
	"errors"
	"fmt"
	"math/big"
	"sync"
	"sync/atomic"
	"time"

	ethereum "github.com/go-ethereum-analysis"
	"github.com/go-ethereum-analysis/common"
	"github.com/go-ethereum-analysis/core/rawdb"
	"github.com/go-ethereum-analysis/core/types"
	"github.com/go-ethereum-analysis/ethdb"
	"github.com/go-ethereum-analysis/event"
	"github.com/go-ethereum-analysis/log"
	"github.com/go-ethereum-analysis/metrics"
	"github.com/go-ethereum-analysis/params"
)

var (
	MaxHashFetch    = 512 // Amount of hashes to be fetched per retrieval request
	MaxBlockFetch   = 128 // Amount of blocks to be fetched per retrieval request
	MaxHeaderFetch  = 192 // Amount of block headers to be fetched per retrieval request
	MaxSkeletonSize = 128 // Number of header fetches to need for a skeleton assembly
	MaxBodyFetch    = 128 // Amount of block bodies to be fetched per retrieval request
	MaxReceiptFetch = 256 // Amount of transaction receipts to allow fetching per request
	MaxStateFetch   = 384 // Amount of node state values to allow fetching per request

	MaxForkAncestry  = 3 * params.EpochDuration // Maximum chain reorganisation
	rttMinEstimate   = 2 * time.Second          // Minimum round-trip time to target for download requests
	rttMaxEstimate   = 20 * time.Second         // Maximum round-trip time to target for download requests
	rttMinConfidence = 0.1                      // Worse confidence factor in our estimated RTT value
	// RTT的恒定比例因子-> TTL转换
	ttlScaling       = 3                        // Constant scaling factor for RTT -> TTL conversion
	ttlLimit         = time.Minute              // Maximum TTL allowance to prevent reaching crazy timeouts

	qosTuningPeers   = 5    // Number of peers to tune based on (best peers)
	qosConfidenceCap = 10   // Number of peers above which not to modify RTT confidence
	qosTuningImpact  = 0.25 // Impact that a new tuning target has on the previous value

	maxQueuedHeaders  = 32 * 1024 // [eth/62] Maximum number of headers to queue for import (DOS protection)
	// 一次导入链中的header下载结果数
	maxHeadersProcess = 2048      // Number of header download results to import at once into the chain
	// 一次导入到链中的内容(txs, receipts)下载结果数
	maxResultsProcess = 2048      // Number of content download results to import at once into the chain

	// fast同步期间下载headers的验证频率
	fsHeaderCheckFrequency = 100             // Verification frequency of the downloaded headers during fast sync
	// 检测到链冲突时要丢弃的header数目限制
	fsHeaderSafetyNet      = 2048            // Number of headers to discard in case a chain violation is detected
	fsHeaderForceVerify    = 24              // Number of headers to verify before and after the pivot to accept it
	fsHeaderContCheck      = 3 * time.Second // Time interval to check for header continuations during state download
	// 即使在 fast 同步下也能完全拉取的块数
	// fast其实不是从当前对端节点的最高块开始,而是从最高块往前64个块开始同步
	// 这里是以太坊故意留64个块,在fast之后赶紧做full以校验数据的准确性
	fsMinFullBlocks        = 64              // Number of blocks to retrieve fully even in fast sync
)

var (
	errBusy                    = errors.New("busy")
	errUnknownPeer             = errors.New("peer is unknown or unhealthy")
	errBadPeer                 = errors.New("action from bad peer ignored")
	errStallingPeer            = errors.New("peer is stalling")
	errNoPeers                 = errors.New("no peers to keep download active")
	errTimeout                 = errors.New("timeout")
	errEmptyHeaderSet          = errors.New("empty header set by peer")
	errPeersUnavailable        = errors.New("no peers available or all tried for download")
	errInvalidAncestor         = errors.New("retrieved ancestor is invalid")
	errInvalidChain            = errors.New("retrieved hash chain is invalid")
	errInvalidBlock            = errors.New("retrieved block is invalid")
	errInvalidBody             = errors.New("retrieved block body is invalid")
	errInvalidReceipt          = errors.New("retrieved receipt is invalid")
	errCancelBlockFetch        = errors.New("block download canceled (requested)")
	errCancelHeaderFetch       = errors.New("block header download canceled (requested)")
	errCancelBodyFetch         = errors.New("block body download canceled (requested)")
	errCancelReceiptFetch      = errors.New("receipt download canceled (requested)")
	errCancelStateFetch        = errors.New("state data download canceled (requested)")
	errCancelHeaderProcessing  = errors.New("header processing canceled (requested)")
	errCancelContentProcessing = errors.New("content processing canceled (requested)")
	errNoSyncActive            = errors.New("no sync active")
	errTooOld                  = errors.New("peer doesn't speak recent enough protocol version (need version >= 62)")
)

type Downloader struct {
	mode SyncMode       // Synchronisation mode defining the strategy used (per sync cycle)
	mux  *event.TypeMux // Event multiplexer to announce sync operation events

	// 调度器队列，用于选择要下载的hashes
	queue   *queue   // Scheduler for selecting the hashes to download
	peers   *peerSet // Set of active peers from which download can proceed
	stateDB ethdb.Database  // 直接操作 levelDB 的中的State数据的db引用

	// Round trip time to target for download requests
	// 以下载请求为目标的往返时间
	rttEstimate   uint64

	// Confidence in the estimated RTT (unit: millionths to allow atomic ops)
	// 对估算的RTT的置信度（单位：允许原子操作的百万分之一）
	rttConfidence uint64

	// Statistics
	syncStatsChainOrigin uint64 // Origin block number where syncing started at
	syncStatsChainHeight uint64 // Highest block number known when syncing started
	syncStatsState       stateSyncStats
	syncStatsLock        sync.RWMutex // Lock protecting the sync stats fields

	lightchain LightChain
	blockchain BlockChain

	// Callbacks
	dropPeer peerDropFn // Drops a peer for misbehaving

	// Status
	synchroniseMock func(id string, hash common.Hash) error // Replacement for synchronise during testing
	synchronising   int32
	notified        int32
	committed       int32

	// Channels
	//
	// 接收入站 headers的通道
	headerCh      chan dataPack        // [eth/62] Channel receiving inbound block headers
	// 接收入站 bodies的通道
	bodyCh        chan dataPack        // [eth/62] Channel receiving inbound block bodies
	// 接收入站 receipts的通道
	receiptCh     chan dataPack        // [eth/63] Channel receiving inbound receipts

	// 向新tasks的block body获取程序(fetcher)发送信号的chan
	bodyWakeCh    chan bool            // [eth/62] Channel to signal the block body fetcher of new tasks
	// 向新tasks的receipt获取者(fetcher)发送信号的chan
	receiptWakeCh chan bool            // [eth/63] Channel to signal the receipt fetcher of new tasks

	// 向header处理器提供新tasks的chan
	headerProcCh  chan []*types.Header // [eth/62] Channel to feed the header processor new tasks

	// for stateFetcher
	stateSyncStart chan *stateSync   // TODO 超级重要的, 发起同步state的请求信号
	trackStateReq  chan *stateReq    // 用来 跟踪req 的处理
	stateCh        chan dataPack // [eth/63] Channel receiving inbound node state data

	// Cancellation and termination
	cancelPeer string         // Identifier of the peer currently being used as the master (cancel on drop)
	cancelCh   chan struct{}  // Channel to cancel mid-flight syncs
	cancelLock sync.RWMutex   // Lock to protect the cancel channel and peer in delivers
	cancelWg   sync.WaitGroup // Make sure all fetcher goroutines have exited.

	quitCh   chan struct{} // Quit channel to signal termination
	quitLock sync.RWMutex  // Lock to prevent double closes

	// Testing hooks
	syncInitHook     func(uint64, uint64)  // Method to call upon initiating a new sync run
	bodyFetchHook    func([]*types.Header) // Method to call upon starting a block body fetch
	receiptFetchHook func([]*types.Header) // Method to call upon starting a receipt fetch
	chainInsertHook  func([]*fetchResult)  // Method to call upon inserting a chain of blocks (possibly in multiple invocations)
}

// LightChain encapsulates functions required to synchronise a light chain.
type LightChain interface {
	// HasHeader verifies a header's presence in the local chain.
	HasHeader(common.Hash, uint64) bool

	// GetHeaderByHash retrieves a header from the local chain.
	GetHeaderByHash(common.Hash) *types.Header

	// CurrentHeader retrieves the head header from the local chain.
	CurrentHeader() *types.Header

	// GetTd returns the total difficulty of a local block.
	GetTd(common.Hash, uint64) *big.Int

	// InsertHeaderChain inserts a batch of headers into the local chain.
	InsertHeaderChain([]*types.Header, int) (int, error)

	// Rollback removes a few recently added elements from the local chain.
	Rollback([]common.Hash)
}

// BlockChain encapsulates functions required to sync a (full or fast) blockchain.
type BlockChain interface {
	LightChain

	// HasBlock verifies a block's presence in the local chain.
	HasBlock(common.Hash, uint64) bool

	// GetBlockByHash retrieves a block from the local chain.
	GetBlockByHash(common.Hash) *types.Block

	// CurrentBlock retrieves the head block from the local chain.
	CurrentBlock() *types.Block

	// CurrentFastBlock retrieves the head fast block from the local chain.
	CurrentFastBlock() *types.Block

	// FastSyncCommitHead directly commits the head block to a certain entity.
	FastSyncCommitHead(common.Hash) error

	// InsertChain inserts a batch of blocks into the local chain.
	InsertChain(types.Blocks) (int, error)

	// InsertReceiptChain inserts a batch of receipts into the local chain.
	InsertReceiptChain(types.Blocks, []types.Receipts) (int, error)
}

// New creates a new downloader to fetch hashes and blocks from remote peers.
// 创建 Downloader 对象
// 到时候同步是根据 mode 来同步的
func New(mode SyncMode, stateDb ethdb.Database, mux *event.TypeMux, chain BlockChain, lightchain LightChain, dropPeer peerDropFn) *Downloader {
	if lightchain == nil {
		lightchain = chain
	}

	dl := &Downloader{
		mode:           mode, // mode: full/fast/light 三种模式
		stateDB:        stateDb,
		mux:            mux,
		queue:          newQueue(),
		peers:          newPeerSet(),
		rttEstimate:    uint64(rttMaxEstimate),
		rttConfidence:  uint64(1000000),
		blockchain:     chain,
		lightchain:     lightchain,
		dropPeer:       dropPeer,
		headerCh:       make(chan dataPack, 1),
		bodyCh:         make(chan dataPack, 1),
		receiptCh:      make(chan dataPack, 1),
		bodyWakeCh:     make(chan bool, 1),
		receiptWakeCh:  make(chan bool, 1),
		headerProcCh:   make(chan []*types.Header, 1),
		quitCh:         make(chan struct{}),
		stateCh:        make(chan dataPack),
		stateSyncStart: make(chan *stateSync),
		syncStatsState: stateSyncStats{
			processed: rawdb.ReadFastTrieProgress(stateDb),
		},
		trackStateReq: make(chan *stateReq),
	}

	// 运行Qos调音器!?
	// 	调节
	//	d.rttEstimate: 下载请求为目标的往返时间
	// 	d.rttConfidence: 对估算的RTT的置信度（单位：允许原子操作的百万分之一）
	go dl.qosTuner()
	go dl.stateFetcher()
	return dl
}

// Progress retrieves the synchronisation boundaries, specifically the origin
// block where synchronisation started at (may have failed/suspended); the block
// or header sync is currently at; and the latest known block which the sync targets.
//
// In addition, during the state download phase of fast synchronisation the number
// of processed and the total number of known states are also returned. Otherwise
// these are zero.
func (d *Downloader) Progress() ethereum.SyncProgress {
	// Lock the current stats and return the progress
	d.syncStatsLock.RLock()
	defer d.syncStatsLock.RUnlock()

	current := uint64(0)
	switch d.mode {
	case FullSync:
		current = d.blockchain.CurrentBlock().NumberU64()
	case FastSync:
		current = d.blockchain.CurrentFastBlock().NumberU64()
	case LightSync:
		current = d.lightchain.CurrentHeader().Number.Uint64()
	}
	return ethereum.SyncProgress{
		StartingBlock: d.syncStatsChainOrigin,
		CurrentBlock:  current,
		HighestBlock:  d.syncStatsChainHeight,
		PulledStates:  d.syncStatsState.processed,
		KnownStates:   d.syncStatsState.processed + d.syncStatsState.pending,
	}
}

// Synchronising returns whether the downloader is currently retrieving blocks.
func (d *Downloader) Synchronising() bool {
	return atomic.LoadInt32(&d.synchronising) > 0
}

// RegisterPeer injects a new download peer into the set of block source to be
// used for fetching hashes and blocks from.
//
// RegisterPeer: 将新的下载 peer 注入到块源集中，以用于从中获取哈希值和块
func (d *Downloader) RegisterPeer(id string, version int, peer Peer) error {
	logger := log.New("peer", id)
	logger.Trace("Registering sync peer")
	/** todo 这里的 peer 是light peer 的实现 */
	if err := d.peers.Register(newPeerConnection(id, version, peer, logger)); err != nil {
		logger.Error("Failed to register sync peer", "err", err)
		return err
	}
	d.qosReduceConfidence()

	return nil
}

// RegisterLightPeer injects a light client peer, wrapping it so it appears as a regular peer.
func (d *Downloader) RegisterLightPeer(id string, version int, peer LightPeer) error {

	/** todo 注册 light peer 实例, downloader 同步时,需要 */
	return d.RegisterPeer(id, version, &lightPeerWrapper{peer})
}

// UnregisterPeer remove a peer from the known list, preventing any action from
// the specified peer. An effort is also made to return any pending fetches into
// the queue.
func (d *Downloader) UnregisterPeer(id string) error {
	// Unregister the peer from the active peer set and revoke any fetch tasks
	logger := log.New("peer", id)
	logger.Trace("Unregistering sync peer")
	if err := d.peers.Unregister(id); err != nil {
		logger.Error("Failed to unregister sync peer", "err", err)
		return err
	}
	d.queue.Revoke(id)

	// If this peer was the master peer, abort sync immediately
	d.cancelLock.RLock()
	master := id == d.cancelPeer
	d.cancelLock.RUnlock()

	if master {
		d.cancel()
	}
	return nil
}

// Synchronise tries to sync up our local block chain with a remote peer, both
// adding various sanity checks as well as wrapping it with various log entries.
func (d *Downloader) Synchronise(id string, head common.Hash, td *big.Int, mode SyncMode) error {
	err := d.synchronise(id, head, td, mode)
	switch err {
	case nil:
	case errBusy:

	case errTimeout, errBadPeer, errStallingPeer,
		errEmptyHeaderSet, errPeersUnavailable, errTooOld,
		errInvalidAncestor, errInvalidChain:
		log.Warn("Synchronisation failed, dropping peer", "peer", id, "err", err)
		if d.dropPeer == nil {
			// The dropPeer method is nil when `--copydb` is used for a local copy.
			// Timeouts can occur if e.g. compaction hits at the wrong time, and can be ignored
			log.Warn("Downloader wants to drop peer, but peerdrop-function is not set", "peer", id)
		} else {
			d.dropPeer(id)
		}
	default:
		log.Warn("Synchronisation failed, retrying", "err", err)
	}
	return err
}

// synchronise will select the peer and use it for synchronising. If an empty string is given
// it will use the best peer possible and synchronize if its TD is higher than our own. If any of the
// checks fail an error will be returned. This method is synchronous
func (d *Downloader) synchronise(id string, hash common.Hash, td *big.Int, mode SyncMode) error {
	// Mock out the synchronisation if testing
	if d.synchroniseMock != nil {
		return d.synchroniseMock(id, hash)
	}
	// Make sure only one goroutine is ever allowed past this point at once
	if !atomic.CompareAndSwapInt32(&d.synchronising, 0, 1) {
		return errBusy
	}
	defer atomic.StoreInt32(&d.synchronising, 0)

	// Post a user notification of the sync (only once per session)
	if atomic.CompareAndSwapInt32(&d.notified, 0, 1) {
		log.Info("Block synchronisation started")
	}
	// Reset the queue, peer set and wake channels to clean any internal leftover state
	d.queue.Reset()
	d.peers.Reset()

	for _, ch := range []chan bool{d.bodyWakeCh, d.receiptWakeCh} {
		select {
		case <-ch:
		default:
		}
	}
	for _, ch := range []chan dataPack{d.headerCh, d.bodyCh, d.receiptCh} {
		for empty := false; !empty; {
			select {
			case <-ch:
			default:
				empty = true
			}
		}
	}
	for empty := false; !empty; {
		select {
		case <-d.headerProcCh:
		default:
			empty = true
		}
	}
	// Create cancel channel for aborting mid-flight and mark the master peer
	d.cancelLock.Lock()
	d.cancelCh = make(chan struct{})
	d.cancelPeer = id
	d.cancelLock.Unlock()

	defer d.Cancel() // No matter what, we can't leave the cancel channel open

	// Set the requested sync mode, unless it's forbidden
	d.mode = mode

	// Retrieve the origin peer and initiate the downloading process
	p := d.peers.Peer(id)
	if p == nil {
		return errUnknownPeer
	}
	return d.syncWithPeer(p, hash, td)
}

// syncWithPeer starts a block synchronization based on the hash chain from the
// specified peer and head hash.
func (d *Downloader) syncWithPeer(p *peerConnection, hash common.Hash, td *big.Int) (err error) {
	d.mux.Post(StartEvent{})
	defer func() {
		// reset on error
		if err != nil {
			d.mux.Post(FailedEvent{err})
		} else {
			d.mux.Post(DoneEvent{})
		}
	}()
	if p.version < 62 {
		return errTooOld
	}

	log.Debug("Synchronising with the network", "peer", p.id, "eth", p.version, "head", hash, "td", td, "mode", d.mode)
	defer func(start time.Time) {
		log.Debug("Synchronisation terminated", "elapsed", time.Since(start))
	}(time.Now())

	// Look up the sync boundaries: the common ancestor and the target block
	//
	// todo 查找同步边界：`公共祖先` 和 `目标块`
	latest, err := d.fetchHeight(p)
	if err != nil {
		return err
	}
	height := latest.Number.Uint64()

	origin, err := d.findAncestor(p, height)
	if err != nil {
		return err
	}
	d.syncStatsLock.Lock()
	if d.syncStatsChainHeight <= origin || d.syncStatsChainOrigin > origin {
		d.syncStatsChainOrigin = origin
	}
	d.syncStatsChainHeight = height
	d.syncStatsLock.Unlock()

	// Ensure our origin point is below any fast sync pivot point
	pivot := uint64(0)
	if d.mode == FastSync {
		if height <= uint64(fsMinFullBlocks) {
			origin = 0
		} else {
			pivot = height - uint64(fsMinFullBlocks)
			if pivot <= origin {
				origin = pivot - 1
			}
		}
	}

	d.committed = 1
	if d.mode == FastSync && pivot != 0 {
		d.committed = 0
	}
	// Initiate the sync using a concurrent header and content retrieval algorithm
	d.queue.Prepare(origin+1, d.mode)
	if d.syncInitHook != nil {
		d.syncInitHook(origin, height)
	}


	/**
	TODO 同步模块最重要的几部分

	抓取 headers
	抓取 bodies
	抓取 receipts

	将抓取到的 header 进行处理 <校验,刷盘> full fast light 都使用的函数 `processHeaders`
	 */
	fetchers := []func() error{
		// headers 总是被拉取的 (根据 skeleton 骨架的方式去拉取) fast full light 都用到
		func() error { return d.fetchHeaders(p, origin+1, pivot) }, // Headers are always retrieved
		// block bodies 只在 full(normal) 和 fast 模式下被拉取
		func() error { return d.fetchBodies(origin + 1) },          // Bodies are retrieved during normal and fast sync
		// receipts 只有在 fast 模式下被拉取
		func() error { return d.fetchReceipts(origin + 1) },        // Receipts are retrieved during fast sync

		// todo: 处理已被 拉取回来的headers
		// todo: 将 headers 校验且落链,且决定是否基于这些headers 拉取更多相关信息 (bodies, receipts等等)
		// todo: full fast light 三种模式都会用到的方法, 处理拉取回来的headers
		func() error { return d.processHeaders(origin+1, pivot, td) },
	}

	/**
	TODO 超级超级重要
	todo 这里只有  full  和fast 模式才会执行到同步 state trie node
	todo  processFastSyncContent 这个方法中就会最终调到这里
	*/
	if d.mode == FastSync {
		// todo 如果是 fast 模式,处理 txs receipts states 的func
		fetchers = append(fetchers, func() error { return d.processFastSyncContent(latest) })
	} else if d.mode == FullSync {
		// 否则只是处理 bodies 之类
		// 逐个将拉取回来的 `fetchResult` 刷盘, 比较简单的实现
		fetchers = append(fetchers, d.processFullSyncContent)
	}

	/** todo go, 上吧,皮卡丘~ */
	return d.spawnSync(fetchers)
}

// spawnSync runs d.process and all given fetcher functions to completion in
// separate goroutines, returning the first error that appears.
//
// spawnSync:
// 在单独的goroutine中运行 `d.process` 和所有给定的fetcher函数，以返回出现的第一个错误
//
// 入参为多个函数的集合,
// 必须的函数:
//     	fetchHeaders : headers的拉取 (根据 skeleton 骨架的方式去拉取) fast full light 都用到
//     	fetchBodies :  bodies的拉取 只在 full(normal) 和 fast 模式下被拉取
//     	fetchReceipts : receipts的拉取 只有在 fast 模式下被拉取
//     	processHeaders : 处理已被 拉取回来的headers  fast full light 都用到
// fast模式需要的函数:
// 		processFastSyncContent : fast 模式拉取的结果处理 处理 txs receipts states 的func
// full模式需要的函数:
// 		processFullSyncContent : full 模式拉取的结果处理 处理 txs 的func
//
//
func (d *Downloader) spawnSync(fetchers []func() error) error {
	errc := make(chan error, len(fetchers))
	d.cancelWg.Add(len(fetchers))

	// 按照顺序,逐个 调用 fn
	for _, fn := range fetchers {
		fn := fn
		go func() { defer d.cancelWg.Done(); errc <- fn() }()
	}
	// Wait for the first error, then terminate the others.
	//
	// 等待第一个错误，然后终止其他错误
	var err error
	for i := 0; i < len(fetchers); i++ {
		if i == len(fetchers)-1 {
			// Close the queue when all fetchers have exited.
			// This will cause the block processor to end when
			// it has processed the queue.
			d.queue.Close()
		}
		if err = <-errc; err != nil {
			break
		}
	}
	d.queue.Close()
	d.Cancel()
	return err
}

// cancel aborts all of the operations and resets the queue. However, cancel does
// not wait for the running download goroutines to finish. This method should be
// used when cancelling the downloads from inside the downloader.
func (d *Downloader) cancel() {
	// Close the current cancel channel
	d.cancelLock.Lock()
	if d.cancelCh != nil {
		select {
		case <-d.cancelCh:
			// Channel was already closed
		default:
			close(d.cancelCh)
		}
	}
	d.cancelLock.Unlock()
}

// Cancel aborts all of the operations and waits for all download goroutines to
// finish before returning.
func (d *Downloader) Cancel() {
	d.cancel()
	d.cancelWg.Wait()
}

// Terminate interrupts the downloader, canceling all pending operations.
// The downloader cannot be reused after calling Terminate.
//
/**
Terminate:
中断下载程序，取消所有挂起的操作.
调用终止后无法重用 downloader.
 */
func (d *Downloader) Terminate() {
	// Close the termination channel (make sure double close is allowed)
	d.quitLock.Lock()
	select {
	case <-d.quitCh:
	default:
		close(d.quitCh)
	}
	d.quitLock.Unlock()

	// Cancel any pending download requests
	d.Cancel()
}

// fetchHeight retrieves the head header of the remote peer to aid in estimating
// the total time a pending synchronisation would take.
//
/**
fetchHeight:
拉取远程peer的头部(最高的)header，以帮助估计待处理的同步将花费的总时间
 */
func (d *Downloader) fetchHeight(p *peerConnection) (*types.Header, error) {
	p.log.Debug("Retrieving remote chain height")

	// Request the advertised remote head block and wait for the response
	//
	// 请求播发的远程头(最高)块并等待响应
	head, _ := p.peer.Head()

	// todo 往对端 peer 发起拉取 block header 的请求 (根据hash)
	go p.peer.RequestHeadersByHash(head, 1, 0, false)

	// 获取 downloader 允许的 请求存活时间
	ttl := d.requestTTL()
	timeout := time.After(ttl)
	for {
		select {
		case <-d.cancelCh:
			return nil, errCancelBlockFetch

		// 处理一个 header 的数据包
		// 从远端抓取的
		// Downloader.DeliverHeaders() 中压入 chan
		case packet := <-d.headerCh:
			// Discard anything not from the origin peer
			//
			// 丢弃不来自原始 peer 的任何内容
			// 即: 丢掉所有pid 不匹配的数据包
			if packet.PeerId() != p.id {
				log.Debug("Received headers from incorrect peer", "peer", packet.PeerId())
				break
			}
			// Make sure the peer actually gave something valid
			// 转化成 headers
			headers := packet.(*headerPack).headers

			// 如果 拿到的包中的header 不是1个的话,报异常~
			// 因为 当前只根据一个Hash 拉取对端peer上的最高块的header啊
			if len(headers) != 1 {
				p.log.Debug("Multiple headers for single request", "headers", len(headers))
				return nil, errBadPeer
			}
			head := headers[0]
			p.log.Debug("Remote head header identified", "number", head.Number, "hash", head.Hash())
			return head, nil

		// 超时处理, 依据是上边的 TTL
		case <-timeout:
			p.log.Debug("Waiting for head header timed out", "elapsed", ttl)
			return nil, errTimeout

		// 忽略 body 和receipt
		// todo note: 理论上都不应该走这一步吧
		case <-d.bodyCh:
		case <-d.receiptCh:
			// Out of bounds delivery, ignore
			// 超出范围交付，忽略
		}
	}
}

// findAncestor tries to locate the common ancestor link of the local chain and
// a remote peers blockchain. In the general case when our node was in sync and
// on the correct chain, checking the top N links should already get us a match.
// In the rare scenario when we ended up on a long reorganisation (i.e. none of
// the head links match), we do a binary search to find the common ancestor.
func (d *Downloader) findAncestor(p *peerConnection, height uint64) (uint64, error) {
	// Figure out the valid ancestor range to prevent rewrite attacks
	floor, ceil := int64(-1), d.lightchain.CurrentHeader().Number.Uint64()

	if d.mode == FullSync {
		ceil = d.blockchain.CurrentBlock().NumberU64()
	} else if d.mode == FastSync {
		ceil = d.blockchain.CurrentFastBlock().NumberU64()
	}
	if ceil >= MaxForkAncestry {
		floor = int64(ceil - MaxForkAncestry)
	}
	p.log.Debug("Looking for common ancestor", "local", ceil, "remote", height)

	// Request the topmost blocks to short circuit binary ancestor lookup
	head := ceil
	if head > height {
		head = height
	}
	from := int64(head) - int64(MaxHeaderFetch)
	if from < 0 {
		from = 0
	}
	// Span out with 15 block gaps into the future to catch bad head reports
	limit := 2 * MaxHeaderFetch / 16
	count := 1 + int((int64(ceil)-from)/16)
	if count > limit {
		count = limit
	}
	go p.peer.RequestHeadersByNumber(uint64(from), count, 15, false)

	// Wait for the remote response to the head fetch
	number, hash := uint64(0), common.Hash{}

	ttl := d.requestTTL()
	timeout := time.After(ttl)

	for finished := false; !finished; {
		select {
		case <-d.cancelCh:
			return 0, errCancelHeaderFetch

		case packet := <-d.headerCh:
			// Discard anything not from the origin peer
			if packet.PeerId() != p.id {
				log.Debug("Received headers from incorrect peer", "peer", packet.PeerId())
				break
			}
			// Make sure the peer actually gave something valid
			headers := packet.(*headerPack).headers
			if len(headers) == 0 {
				p.log.Warn("Empty head header set")
				return 0, errEmptyHeaderSet
			}
			// Make sure the peer's reply conforms to the request
			for i := 0; i < len(headers); i++ {
				if number := headers[i].Number.Int64(); number != from+int64(i)*16 {
					p.log.Warn("Head headers broke chain ordering", "index", i, "requested", from+int64(i)*16, "received", number)
					return 0, errInvalidChain
				}
			}
			// Check if a common ancestor was found
			finished = true
			for i := len(headers) - 1; i >= 0; i-- {
				// Skip any headers that underflow/overflow our requested set
				if headers[i].Number.Int64() < from || headers[i].Number.Uint64() > ceil {
					continue
				}
				// Otherwise check if we already know the header or not
				if (d.mode == FullSync && d.blockchain.HasBlock(headers[i].Hash(), headers[i].Number.Uint64())) || (d.mode != FullSync && d.lightchain.HasHeader(headers[i].Hash(), headers[i].Number.Uint64())) {
					number, hash = headers[i].Number.Uint64(), headers[i].Hash()

					// If every header is known, even future ones, the peer straight out lied about its head
					if number > height && i == limit-1 {
						p.log.Warn("Lied about chain head", "reported", height, "found", number)
						return 0, errStallingPeer
					}
					break
				}
			}

		case <-timeout:
			p.log.Debug("Waiting for head header timed out", "elapsed", ttl)
			return 0, errTimeout

		case <-d.bodyCh:
		case <-d.receiptCh:
			// Out of bounds delivery, ignore
		}
	}
	// If the head fetch already found an ancestor, return
	if hash != (common.Hash{}) {
		if int64(number) <= floor {
			p.log.Warn("Ancestor below allowance", "number", number, "hash", hash, "allowance", floor)
			return 0, errInvalidAncestor
		}
		p.log.Debug("Found common ancestor", "number", number, "hash", hash)
		return number, nil
	}
	// Ancestor not found, we need to binary search over our chain
	start, end := uint64(0), head
	if floor > 0 {
		start = uint64(floor)
	}
	for start+1 < end {
		// Split our chain interval in two, and request the hash to cross check
		check := (start + end) / 2

		ttl := d.requestTTL()
		timeout := time.After(ttl)

		go p.peer.RequestHeadersByNumber(check, 1, 0, false)

		// Wait until a reply arrives to this request
		for arrived := false; !arrived; {
			select {
			case <-d.cancelCh:
				return 0, errCancelHeaderFetch

			case packer := <-d.headerCh:
				// Discard anything not from the origin peer
				if packer.PeerId() != p.id {
					log.Debug("Received headers from incorrect peer", "peer", packer.PeerId())
					break
				}
				// Make sure the peer actually gave something valid
				headers := packer.(*headerPack).headers
				if len(headers) != 1 {
					p.log.Debug("Multiple headers for single request", "headers", len(headers))
					return 0, errBadPeer
				}
				arrived = true

				// Modify the search interval based on the response
				if (d.mode == FullSync && !d.blockchain.HasBlock(headers[0].Hash(), headers[0].Number.Uint64())) || (d.mode != FullSync && !d.lightchain.HasHeader(headers[0].Hash(), headers[0].Number.Uint64())) {
					end = check
					break
				}
				header := d.lightchain.GetHeaderByHash(headers[0].Hash()) // Independent of sync mode, header surely exists
				if header.Number.Uint64() != check {
					p.log.Debug("Received non requested header", "number", header.Number, "hash", header.Hash(), "request", check)
					return 0, errBadPeer
				}
				start = check

			case <-timeout:
				p.log.Debug("Waiting for search header timed out", "elapsed", ttl)
				return 0, errTimeout

			case <-d.bodyCh:
			case <-d.receiptCh:
				// Out of bounds delivery, ignore
			}
		}
	}
	// Ensure valid ancestry and return
	if int64(start) <= floor {
		p.log.Warn("Ancestor below allowance", "number", start, "hash", hash, "allowance", floor)
		return 0, errInvalidAncestor
	}
	p.log.Debug("Found common ancestor", "number", start, "hash", hash)
	return start, nil
}

// fetchHeaders keeps retrieving headers concurrently from the number
// requested, until no more are returned, potentially throttling on the way. To
// facilitate concurrency but still protect against malicious nodes sending bad
// headers, we construct a header chain skeleton using the "origin" peer we are
// syncing with, and fill in the missing headers using anyone else. Headers from
// other peers are only accepted if they map cleanly to the skeleton. If no one
// can fill in the skeleton - not even the origin peer - it's assumed invalid and
// the origin is dropped.
func (d *Downloader) fetchHeaders(p *peerConnection, from uint64, pivot uint64) error {
	p.log.Debug("Directing header downloads", "origin", from)
	defer p.log.Debug("Header download terminated")

	// Create a timeout timer, and the associated header fetcher
	// 创建一个超时计时器，以及相关的 header提取程序


	// 表示: 骨架组装阶段或完成阶段 的标识位
	// true: 表示可以拉取 骨架
	// false: 表示不可以或者已完成拉取骨架
	skeleton := true            // Skeleton assembly phase or finishing up
	// 最后骨架获取请求的时间
	request := time.Now()       // time of the last skeleton fetch request
	// 计时器以 记录 dump 无响应的活动 peer
	timeout := time.NewTimer(0) // timer to dump a non-responsive active peer

	// 超时通道最初应为空
	<-timeout.C                 // timeout channel should be initially empty
	defer timeout.Stop()

	var ttl time.Duration

	/**
	封装 获取头的函数
	 */
	getHeaders := func(from uint64) {
		request = time.Now()

		// 获取出计算得到的 TTL 值
		ttl = d.requestTTL()
		// 使用该值设置给 timer
		timeout.Reset(ttl)

		if skeleton {
			// 如果是用骨架同步的话 (一般,同步开始的时候先用 骨架同步方式, 同步回几个关键的 block 作为骨架点)
			p.log.Trace("Fetching skeleton headers", "count", MaxHeaderFetch, "from", from)
			go p.peer.RequestHeadersByNumber(from+uint64(MaxHeaderFetch)-1, MaxSkeletonSize, MaxHeaderFetch-1, false)
		} else {
			// 使用 正常同步方式, 一般是骨架同步关键 block 之后
			p.log.Trace("Fetching full headers", "count", MaxHeaderFetch, "from", from)
			go p.peer.RequestHeadersByNumber(from, MaxHeaderFetch, 0, false)
		}
	}
	// Start pulling the header chain skeleton until all is done
	// 开始拉取回 headeR chain 骨架，直到完成所有操作
	getHeaders(from) // 先拉 骨架

	for {
		select {

		// 接收到取消信号
		case <-d.cancelCh:
			return errCancelHeaderFetch

		// 接收到 block header 的数据包
		case packet := <-d.headerCh:
			// Make sure the active peer is giving us the skeleton headers
			// 确保活动的 peer 正在给我们骨架点上的 headers
			//
			// 如果不是对应的对端peer发回来的消息包
			if packet.PeerId() != p.id {
				log.Debug("Received skeleton from incorrect peer", "peer", packet.PeerId())
				break
			}

			// 统计相关
			headerReqTimer.UpdateSince(request)
			// 数据包已经拿回,则将超时计时器关闭,以免时间到了,发送没必要的超市信号
			timeout.Stop()

			// If the skeleton's finished, pull any remaining head headers directly from the origin
			//
			// 如果骨架完成，直接从原点拉出任何剩余的header
			if packet.Items() == 0 && skeleton {

				// 现将骨架标识位置为: false
				skeleton = false
				// 从 起始点拉取剩余部分 headers
				getHeaders(from)
				continue
			}

			// If no more headers are inbound, notify the content fetchers and return
			//
			// 如果没有更多的headers 入站，则通知所有内容提取器(fetchers)并返回
			if packet.Items() == 0 {
				// Don't abort header fetches while the pivot is downloading
				//
				// 当基准点 pivot被下载时,不要终止其他headers 的拉取
				if atomic.LoadInt32(&d.committed) == 0 && pivot <= from {
					p.log.Debug("No headers, waiting for pivot commit")
					select {
					case <-time.After(fsHeaderContCheck):
						getHeaders(from)
						continue
					case <-d.cancelCh:
						return errCancelHeaderFetch
					}
				}
				// Pivot done (or not in fast sync) and no more headers, terminate the process
				p.log.Debug("No more headers available")
				select {
				case d.headerProcCh <- nil:
					return nil
				case <-d.cancelCh:
					return errCancelHeaderFetch
				}
			}

			// 将数据包做断言转化
			// 沃日,这里不做ok判断的?
			headers := packet.(*headerPack).headers

			// If we received a skeleton batch, resolve internals concurrently
			//
			// 如果我们收到 骨架 batch，请同时解决
			// 即:
			if skeleton {

				// todo 一个重中之重的入口
				// 以起始点为基准,开始填充 拉取回来的,骨架之外的 其他headers
				/**
				返回:
				结果缓存累积完成的 header
				和 已被处理的 header 数量

				并重置 downloader的queue上的这两个字段的的值
				*/
				filled, proced, err := d.fillHeaderSkeleton(from, headers)
				if err != nil {
					p.log.Debug("Skeleton chain invalid", "err", err)
					return errInvalidChain
				}

				// 取出 已经拉回来,但是还没有被处理的headers
				headers = filled[proced:]

				// 将起始点向最后一个被处理的header的高度移动
				from += uint64(proced)
			}


			// Insert all the new headers and fetch the next batch
			//
			// 插入所有新的headers并获取下一批
			if len(headers) > 0 {
				p.log.Trace("Scheduling new headers", "count", len(headers), "from", from)
				select {
				case d.headerProcCh <- headers:
				case <-d.cancelCh:
					return errCancelHeaderFetch
				}
				from += uint64(len(headers))
			}
			getHeaders(from)

		case <-timeout.C:
			if d.dropPeer == nil {
				// The dropPeer method is nil when `--copydb` is used for a local copy.
				// Timeouts can occur if e.g. compaction hits at the wrong time, and can be ignored
				p.log.Warn("Downloader wants to drop peer, but peerdrop-function is not set", "peer", p.id)
				break
			}
			// Header retrieval timed out, consider the peer bad and drop
			p.log.Debug("Header request timed out", "elapsed", ttl)
			headerTimeoutMeter.Mark(1)
			d.dropPeer(p.id)

			// Finish the sync gracefully instead of dumping the gathered data though
			for _, ch := range []chan bool{d.bodyWakeCh, d.receiptWakeCh} {
				select {
				case ch <- false:
				case <-d.cancelCh:
				}
			}
			select {
			case d.headerProcCh <- nil:
			case <-d.cancelCh:
			}
			return errBadPeer
		}
	}
}

// fillHeaderSkeleton concurrently retrieves headers from all our available peers
// and maps them to the provided skeleton header chain.
//
// Any partial results from the beginning of the skeleton is (if possible) forwarded
// immediately to the header processor to keep the rest of the pipeline full even
// in the case of header stalls.
//
// The method returns the entire filled skeleton and also the number of headers
// already forwarded for processing.
//
/**
fillHeaderSkeleton:
同时从所有可用 peer 拉取 header，并将它们映射到提供的 骨架header chain 上面

骨架开头的任何部分结果（如果可能）都将立即转发到 headers 处理器，即使在 某个骨架点的header 停顿的情况下也可以保持其余的管道满载

该方法返回整个已填充的骨架以及已被转发进行处理的 headers数目
 */
func (d *Downloader) fillHeaderSkeleton(from uint64, skeleton []*types.Header) ([]*types.Header, int, error) {
	log.Debug("Filling up skeleton", "from", from)

	// 开始填充 骨架上剩余的被拉取回来的block 相关的header,body,receipt等等之类
	d.queue.ScheduleSkeleton(from, skeleton)

	var (

		/**
		req拉取回来的数据 交付函数
		 */
		deliver = func(packet dataPack) (int, error) {
			pack := packet.(*headerPack)
			return d.queue.DeliverHeaders(pack.peerID, pack.headers, d.headerProcCh)
		}

		/**
		处理 存活时间 相关函数
		 */
		expire   = func() map[string]int { return d.queue.ExpireHeaders(d.requestTTL()) }

		/**
		喉咙? 油门?
		 */
		throttle = func() bool { return false }

		/**
		保留给定 peer 的一组 headers，跳过任何先前失败的 batch
		 */
		reserve  = func(p *peerConnection, count int) (*fetchRequest, bool, error) {
			return d.queue.ReserveHeaders(p, count), false, nil
		}

		/**
		发送header 拉取 req到远程 peer
		TODO 在这里头,真正的请求 RequestHeadersByNumber 函数了
		 */
		fetch    = func(p *peerConnection, req *fetchRequest) error { return p.FetchHeaders(req.From, MaxHeaderFetch) }

		/**
		根据其先前发现的吞吐量拉取peer 的header 下载配额
		 */
		capacity = func(p *peerConnection) int { return p.HeaderCapacity(d.requestRTT()) }

		/**
		将peer设置为空闲，从而允许其执行新的header拉取req
		 */
		setIdle  = func(p *peerConnection, accepted int) { p.SetHeadersIdle(accepted) }
	)

	/* 将上述 func 加到这里头,进行调度 */
	// todo 这个大头函数,你读懂,我吃屎
	err := d.fetchParts(errCancelHeaderFetch, d.headerCh, deliver, d.queue.headerContCh, expire,
		d.queue.PendingHeaders, d.queue.InFlightHeaders, throttle, reserve,
		nil, fetch, d.queue.CancelHeaders, capacity, d.peers.HeaderIdlePeers, setIdle, "headers")

	log.Debug("Skeleton fill terminated", "err", err)

	// 根据计划的骨架拉取header进行组装
	// todo 说是这么说,但其实这个方法并没有做任何事.
	/**
	返回:
	结果缓存累积完成的 header
	和 已被处理的 header 数量
	*/
	filled, proced := d.queue.RetrieveHeaders()
	return filled, proced, err
}

// fetchBodies iteratively downloads the scheduled block bodies, taking any
// available peers, reserving a chunk of blocks for each, waiting for delivery
// and also periodically checking for timeouts.
func (d *Downloader) fetchBodies(from uint64) error {
	log.Debug("Downloading block bodies", "origin", from)

	var (
		deliver = func(packet dataPack) (int, error) {
			pack := packet.(*bodyPack)
			return d.queue.DeliverBodies(pack.peerID, pack.transactions, pack.uncles)
		}
		expire   = func() map[string]int { return d.queue.ExpireBodies(d.requestTTL()) }
		fetch    = func(p *peerConnection, req *fetchRequest) error { return p.FetchBodies(req) }
		capacity = func(p *peerConnection) int { return p.BlockCapacity(d.requestRTT()) }
		setIdle  = func(p *peerConnection, accepted int) { p.SetBodiesIdle(accepted) }
	)
	err := d.fetchParts(errCancelBodyFetch, d.bodyCh, deliver, d.bodyWakeCh, expire,
		d.queue.PendingBlocks, d.queue.InFlightBlocks, d.queue.ShouldThrottleBlocks, d.queue.ReserveBodies,
		d.bodyFetchHook, fetch, d.queue.CancelBodies, capacity, d.peers.BodyIdlePeers, setIdle, "bodies")

	log.Debug("Block body download terminated", "err", err)
	return err
}

// fetchReceipts iteratively downloads the scheduled block receipts, taking any
// available peers, reserving a chunk of receipts for each, waiting for delivery
// and also periodically checking for timeouts.
func (d *Downloader) fetchReceipts(from uint64) error {
	log.Debug("Downloading transaction receipts", "origin", from)

	var (
		deliver = func(packet dataPack) (int, error) {
			pack := packet.(*receiptPack)
			return d.queue.DeliverReceipts(pack.peerID, pack.receipts)
		}
		expire   = func() map[string]int { return d.queue.ExpireReceipts(d.requestTTL()) }
		fetch    = func(p *peerConnection, req *fetchRequest) error { return p.FetchReceipts(req) }
		capacity = func(p *peerConnection) int { return p.ReceiptCapacity(d.requestRTT()) }
		setIdle  = func(p *peerConnection, accepted int) { p.SetReceiptsIdle(accepted) }
	)
	err := d.fetchParts(errCancelReceiptFetch, d.receiptCh, deliver, d.receiptWakeCh, expire,
		d.queue.PendingReceipts, d.queue.InFlightReceipts, d.queue.ShouldThrottleReceipts, d.queue.ReserveReceipts,
		d.receiptFetchHook, fetch, d.queue.CancelReceipts, capacity, d.peers.ReceiptIdlePeers, setIdle, "receipts")

	log.Debug("Transaction receipt download terminated", "err", err)
	return err
}

/**
todo 这个大头函数,你读懂,我吃屎
 */
// fetchParts iteratively downloads scheduled block parts, taking any available
// peers, reserving a chunk of fetch requests for each, waiting for delivery and
// also periodically checking for timeouts.
//
// As the scheduling/timeout logic mostly is the same for all downloaded data
// types, this method is used by each for data gathering and is instrumented with
// various callbacks to handle the slight differences between processing them.
//
// The instrumentation parameters:
//  - errCancel:   error type to return if the fetch operation is cancelled (mostly makes logging nicer)
//  - deliveryCh:  channel from which to retrieve downloaded data packets (merged from all concurrent peers)
//  - deliver:     processing callback to deliver data packets into type specific download queues (usually within `queue`)
//  - wakeCh:      notification channel for waking the fetcher when new tasks are available (or sync completed)
//  - expire:      task callback method to abort requests that took too long and return the faulty peers (traffic shaping)
//  - pending:     task callback for the number of requests still needing download (detect completion/non-completability)
//  - inFlight:    task callback for the number of in-progress requests (wait for all active downloads to finish)
//  - throttle:    task callback to check if the processing queue is full and activate throttling (bound memory use)
//  - reserve:     task callback to reserve new download tasks to a particular peer (also signals partial completions)
//  - fetchHook:   tester callback to notify of new tasks being initiated (allows testing the scheduling logic)
//  - fetch:       network callback to actually send a particular download request to a physical remote peer
//  - cancel:      task callback to abort an in-flight download request and allow rescheduling it (in case of lost peer)
//  - capacity:    network callback to retrieve the estimated type-specific bandwidth capacity of a peer (traffic shaping)
//  - idle:        network callback to retrieve the currently (type specific) idle peers that can be assigned tasks
//  - setIdle:     network callback to set a peer back to idle and update its estimated capacity (traffic shaping)
//  - kind:        textual label of the type being downloaded to display in log mesages
func (d *Downloader) fetchParts(errCancel error, deliveryCh chan dataPack, deliver func(dataPack) (int, error), wakeCh chan bool,
	expire func() map[string]int, pending func() int, inFlight func() bool, throttle func() bool, reserve func(*peerConnection, int) (*fetchRequest, bool, error),
	fetchHook func([]*types.Header), fetch func(*peerConnection, *fetchRequest) error, cancel func(*fetchRequest), capacity func(*peerConnection) int,
	idle func() ([]*peerConnection, int), setIdle func(*peerConnection, int), kind string) error {

	// Create a ticker to detect expired retrieval tasks
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	update := make(chan struct{}, 1)

	// Prepare the queue and fetch block parts until the block header fetcher's done
	finished := false
	for {
		select {
		case <-d.cancelCh:
			return errCancel

		case packet := <-deliveryCh:
			// If the peer was previously banned and failed to deliver its pack
			// in a reasonable time frame, ignore its message.
			if peer := d.peers.Peer(packet.PeerId()); peer != nil {
				// Deliver the received chunk of data and check chain validity
				accepted, err := deliver(packet)
				if err == errInvalidChain {
					return err
				}
				// Unless a peer delivered something completely else than requested (usually
				// caused by a timed out request which came through in the end), set it to
				// idle. If the delivery's stale, the peer should have already been idled.
				if err != errStaleDelivery {
					setIdle(peer, accepted)
				}
				// Issue a log to the user to see what's going on
				switch {
				case err == nil && packet.Items() == 0:
					peer.log.Trace("Requested data not delivered", "type", kind)
				case err == nil:
					peer.log.Trace("Delivered new batch of data", "type", kind, "count", packet.Stats())
				default:
					peer.log.Trace("Failed to deliver retrieved data", "type", kind, "err", err)
				}
			}
			// Blocks assembled, try to update the progress
			select {
			case update <- struct{}{}:
			default:
			}

		case cont := <-wakeCh:
			// The header fetcher sent a continuation flag, check if it's done
			if !cont {
				finished = true
			}
			// Headers arrive, try to update the progress
			select {
			case update <- struct{}{}:
			default:
			}

		case <-ticker.C:
			// Sanity check update the progress
			select {
			case update <- struct{}{}:
			default:
			}

		case <-update:
			// Short circuit if we lost all our peers
			if d.peers.Len() == 0 {
				return errNoPeers
			}
			// Check for fetch request timeouts and demote the responsible peers
			for pid, fails := range expire() {
				if peer := d.peers.Peer(pid); peer != nil {
					// If a lot of retrieval elements expired, we might have overestimated the remote peer or perhaps
					// ourselves. Only reset to minimal throughput but don't drop just yet. If even the minimal times
					// out that sync wise we need to get rid of the peer.
					//
					// The reason the minimum threshold is 2 is because the downloader tries to estimate the bandwidth
					// and latency of a peer separately, which requires pushing the measures capacity a bit and seeing
					// how response times reacts, to it always requests one more than the minimum (i.e. min 2).
					if fails > 2 {
						peer.log.Trace("Data delivery timed out", "type", kind)
						setIdle(peer, 0)
					} else {
						peer.log.Debug("Stalling delivery, dropping", "type", kind)
						if d.dropPeer == nil {
							// The dropPeer method is nil when `--copydb` is used for a local copy.
							// Timeouts can occur if e.g. compaction hits at the wrong time, and can be ignored
							peer.log.Warn("Downloader wants to drop peer, but peerdrop-function is not set", "peer", pid)
						} else {
							d.dropPeer(pid)
						}
					}
				}
			}
			// If there's nothing more to fetch, wait or terminate
			if pending() == 0 {
				if !inFlight() && finished {
					log.Debug("Data fetching completed", "type", kind)
					return nil
				}
				break
			}
			// Send a download request to all idle peers, until throttled
			progressed, throttled, running := false, false, inFlight()
			idles, total := idle()

			for _, peer := range idles {
				// Short circuit if throttling activated
				if throttle() {
					throttled = true
					break
				}
				// Short circuit if there is no more available task.
				if pending() == 0 {
					break
				}
				// Reserve a chunk of fetches for a peer. A nil can mean either that
				// no more headers are available, or that the peer is known not to
				// have them.
				request, progress, err := reserve(peer, capacity(peer))
				if err != nil {
					return err
				}
				if progress {
					progressed = true
				}
				if request == nil {
					continue
				}
				if request.From > 0 {
					peer.log.Trace("Requesting new batch of data", "type", kind, "from", request.From)
				} else {
					peer.log.Trace("Requesting new batch of data", "type", kind, "count", len(request.Headers), "from", request.Headers[0].Number)
				}
				// Fetch the chunk and make sure any errors return the hashes to the queue
				if fetchHook != nil {
					fetchHook(request.Headers)
				}
				if err := fetch(peer, request); err != nil {
					// Although we could try and make an attempt to fix this, this error really
					// means that we've double allocated a fetch task to a peer. If that is the
					// case, the internal state of the downloader and the queue is very wrong so
					// better hard crash and note the error instead of silently accumulating into
					// a much bigger issue.
					panic(fmt.Sprintf("%v: %s fetch assignment failed", peer, kind))
				}
				running = true
			}
			// Make sure that we have peers available for fetching. If all peers have been tried
			// and all failed throw an error
			if !progressed && !throttled && !running && len(idles) == total && pending() > 0 {
				return errPeersUnavailable
			}
		}
	}
}

// processHeaders takes batches of retrieved headers from an input channel and
// keeps processing and scheduling them into the header chain and downloader's
// queue until the stream ends or a failure occurs.
//
/**
processHeaders:
从输入通道中提取批次的headers，并继续进行处理并将其安排在 headerchain 和 download 的 queue中，直到流结束或发生故障
todo 处理已被 拉取回来的headers

todo 说白了就是将 headers 校验且落链,且决定是否基于这些headers 拉取更多相关信息 (bodies, receipts等等)
 */
func (d *Downloader) processHeaders(origin uint64, pivot uint64, td *big.Int) error {
	// Keep a count of uncertain headers to roll back
	//
	// 保留 不确定的 header 数量 用以回滚
	rollback := []*types.Header{}
	defer func() {

		// 当具备 rollback 时,
		if len(rollback) > 0 {
			// Flatten the headers and roll them back
			// 压平 这些需要rollback 的headers 并将其回滚
			hashes := make([]common.Hash, len(rollback))
			for i, header := range rollback {
				hashes[i] = header.Hash()
			}
			lastHeader, lastFastBlock, lastBlock := d.lightchain.CurrentHeader().Number, common.Big0, common.Big0
			if d.mode != LightSync {
				lastFastBlock = d.blockchain.CurrentFastBlock().Number()
				lastBlock = d.blockchain.CurrentBlock().Number()
			}

			/** todo 根据hashes 回滚 headerchain?  */
			d.lightchain.Rollback(hashes)
			curFastBlock, curBlock := common.Big0, common.Big0
			if d.mode != LightSync {
				curFastBlock = d.blockchain.CurrentFastBlock().Number()
				curBlock = d.blockchain.CurrentBlock().Number()
			}
			log.Warn("Rolled back headers", "count", len(hashes),
				"header", fmt.Sprintf("%d->%d", lastHeader, d.lightchain.CurrentHeader().Number),
				"fast", fmt.Sprintf("%d->%d", lastFastBlock, curFastBlock),
				"block", fmt.Sprintf("%d->%d", lastBlock, curBlock))
		}
	}()

	// Wait for batches of headers to process
	//
	// 等待一批 headers处理
	gotHeaders := false

	for {
		select {
		case <-d.cancelCh:
			return errCancelHeaderProcessing


		/**
		接收到 新的 已被下载回来的 headers
		进行处理这些headers
		 */
		case headers := <-d.headerProcCh:
			// Terminate header processing if we synced up
			//
			/**

			todo 如果我们已经同步完了，则终止头处理
			 */
			if len(headers) == 0 {
				// Notify everyone that headers are fully processed
				//
				// 通知所有人headers已完全处理, 需要去获取新的 block body 和 receipt了
				for _, ch := range []chan bool{d.bodyWakeCh, d.receiptWakeCh} {
					select {
					case ch <- false:
					case <-d.cancelCh:
					}
				}
				// If no headers were retrieved at all, the peer violated its TD promise that it had a
				// better chain compared to ours. The only exception is if its promised blocks were
				// already imported by other means (e.g. fecher):
				//
				// R <remote peer>, L <local node>: Both at block 10
				// R: Mine block 11, and propagate it to L
				// L: Queue block 11 for import
				// L: Notice that R's head and TD increased compared to ours, start sync
				// L: Import of block 11 finishes
				// L: Sync begins, and finds common ancestor at 11
				// L: Request new headers up from 11 (R's TD was higher, it must have something)
				// R: Nothing to give
				//
				/**
				如果根本没有拉取到header，则对端peer违反了其TD承诺，即与我们相比，
				它具有更好的(它自己认为更好的)链(即: 对端和我们 分叉了)。
				唯一的例外是其承诺的block是否已通过其他方式
				（例如fecher <这里的fetcher 指的是 和 downloader 平起平坐的那个 fetcher >）导入：

				R <远程对等点>，L <本地节点>：两者都在块10
				R：挖到第11块，并将其传播到L
				L：要导入的队列块11
				L：请注意，与我们相比，R的头部和TD有所增加，开始同步
				L：导入第11块加工完成
				L：同步开始，并在11找到共同祖先
				L：从11开始请求新的标头（R的TD更高，必须有值）
				R：没什么可给的
				 */
				if d.mode != LightSync { // TODO 这里是  light 模式处理 header

					// 先拿自己本地chain的最高块
					head := d.blockchain.CurrentBlock()

					// 如果 gotHeader == false 且 当前chain的td比入参的td小的话就报错
					if !gotHeaders && td.Cmp(d.blockchain.GetTd(head.Hash(), head.NumberU64())) > 0 {
						return errStallingPeer
					}
				}
				// If fast or light syncing, ensure promised headers are indeed delivered. This is
				// needed to detect scenarios where an attacker feeds a bad pivot and then bails out
				// of delivering the post-pivot blocks that would flag the invalid content.
				//
				// This check cannot be executed "as is" for full imports, since blocks may still be
				// queued for processing when the header download completes. However, as long as the
				// peer gave us something useful, we're already happy/progressed (above check).
				//
				/**
				如果进行fast同步或 light同步，请确保确实交付了(即: 处理了)承诺的header。
				这是必要的，以检测攻击者提供了错误的枢轴块(pivot),然后放弃提供会标记无效内容的后枢轴块 (post-pivot)

				对于full同步，此`if`检查无法“按原样”执行，因为在header下载完成后，block仍可能排队等待处理。
				但是，只要对端peer给我们一些有用的东西，我们就已经很高兴/进步了（经过检查）
				 */
				if d.mode == FastSync || d.mode == LightSync {
					// 还是先拿链上最高块的header
					head := d.lightchain.CurrentHeader()
					if td.Cmp(d.lightchain.GetTd(head.Hash(), head.Number.Uint64())) > 0 {
						return errStallingPeer
					}
				}
				// Disable any rollback and return
				//
				// 禁用任何回滚和返回
				rollback = nil
				return nil
			}
			// Otherwise split the chunk of headers into batches and process them
			//
			// 否则，将 headers 拆分为多个批次并进行处理
			gotHeaders = true

			/**
			todo 否则, 我们将处理这些 headers
			 */
			for len(headers) > 0 {
				// Terminate if something failed in between processing chunks
				//
				// 如果在处理 block 之间出现故障，则终止
				select {
				case <-d.cancelCh:
					return errCancelHeaderProcessing
				default:
				}
				// Select the next chunk of headers to import
				//
				// 选择下一组要导入的 headers
				limit := maxHeadersProcess

				// 默认, 不超过 2048 个headers
				if limit > len(headers) {
					limit = len(headers)
				}

				// 每最大 2048个headers 为一组,拿去处理
				chunk := headers[:limit]

				// In case of header only syncing, validate the chunk immediately
				//
				// 如果仅 header同步，则立即验证该组headers
				// 这种情况 只会发生在 fast 模式和 light 模式
				if d.mode == FastSync || d.mode == LightSync {
					// Collect the yet unknown headers to mark them as uncertain
					//
					// 收集未知的headers以将其标记为不确定
					unknown := make([]*types.Header, 0, len(headers))

					// 过滤出当前组中headers中 不在当前 chain 上的 header
					for _, header := range chunk {
						if !d.lightchain.HasHeader(header.Hash(), header.Number.Uint64()) {
							unknown = append(unknown, header)
						}
					}


					// If we're importing pure headers, verify based on their recentness
					//
					// 如果我们要导入纯headers，根据他们的近况进行验证
					frequency := fsHeaderCheckFrequency // fast同步期间下载headers的验证频率

					// 如果组中最后一个header的高度-频率 >
					if chunk[len(chunk)-1].Number.Uint64()+uint64(fsHeaderForceVerify) > pivot {
						frequency = 1
					}

					// 将headers组加入 chain中
					if n, err := d.lightchain.InsertHeaderChain(chunk, frequency); err != nil {
						// If some headers were inserted, add them too to the rollback list
						if n > 0 {
							rollback = append(rollback, chunk[:n]...)
						}
						log.Debug("Invalid header encountered", "number", chunk[n].Number, "hash", chunk[n].Hash(), "err", err)
						return errInvalidChain
					}
					// All verifications passed, store newly found uncertain headers
					//
					// 通过所有验证，存储新发现的不确定 headers
					//
					// todo 不明白为什么将 unknown 的headers也一起追加以备 rollback ?
					rollback = append(rollback, unknown...)

					// 如果需要 rollback 的header 数目  大于  检测到链冲突时要丢弃的header数目限制
					if len(rollback) > fsHeaderSafetyNet {
						rollback = append(rollback[:0], rollback[len(rollback)-fsHeaderSafetyNet:]...)
					}
				}
				// Unless we're doing light chains, schedule the headers for associated content retrieval
				// 除非我们做的是 light链，否则请安排headers以进行关联的内容拉取
				// 如: full 和 fast 都需要继续拉取 bodies
				if d.mode == FullSync || d.mode == FastSync {
					// If we've reached the allowed number of pending headers, stall a bit
					//
					// 如果我们达到了待处理的headers的允许数量，请稍等一下
					for d.queue.PendingBlocks() >= maxQueuedHeaders || d.queue.PendingReceipts() >= maxQueuedHeaders {
						select {
						case <-d.cancelCh:
							return errCancelHeaderProcessing

							// 等个一秒钟
						case <-time.After(time.Second):
						}
					}
					// Otherwise insert the headers for content retrieval
					//
					// 否则插入headers以进行关联内容(bodies,receipts等等)的拉取
					inserts := d.queue.Schedule(chunk, origin)
					if len(inserts) != len(chunk) {
						log.Debug("Stale headers")
						return errBadPeer
					}
				}

				// 继续处理剩下的 header
				headers = headers[limit:]
				// 记录已经处理的headers数目
				origin += uint64(limit)
			}

			// Update the highest block number we know if a higher one is found.
			d.syncStatsLock.Lock()
			if d.syncStatsChainHeight < origin {
				d.syncStatsChainHeight = origin - 1
			}
			d.syncStatsLock.Unlock()

			// Signal the content downloaders of the availablility of new tasks
			//
			// 通知 继续下载新的内容的 tasks
			for _, ch := range []chan bool{d.bodyWakeCh, d.receiptWakeCh} {
				select {
				case ch <- true:
				default:
				}
			}
		}
	}
}

// processFullSyncContent takes fetch results from the queue and imports them into the chain.
func (d *Downloader) processFullSyncContent() error {
	// full 没有 fast那么复杂,就直接拿数据直接组个刷盘
	// 没有什么根据pivot点操作
	for {
		results := d.queue.Results(true)
		if len(results) == 0 {
			return nil
		}
		if d.chainInsertHook != nil {
			d.chainInsertHook(results)
		}
		if err := d.importBlockResults(results); err != nil {
			return err
		}
	}
}

func (d *Downloader) importBlockResults(results []*fetchResult) error {
	// Check for any early termination requests
	if len(results) == 0 {
		return nil
	}
	select {
	case <-d.quitCh:
		return errCancelContentProcessing
	default:
	}
	// Retrieve the a batch of results to import
	first, last := results[0].Header, results[len(results)-1].Header
	log.Debug("Inserting downloaded chain", "items", len(results),
		"firstnum", first.Number, "firsthash", first.Hash(),
		"lastnum", last.Number, "lasthash", last.Hash(),
	)
	blocks := make([]*types.Block, len(results))
	for i, result := range results {
		blocks[i] = types.NewBlockWithHeader(result.Header).WithBody(result.Transactions, result.Uncles)
	}
	if index, err := d.blockchain.InsertChain(blocks); err != nil {
		log.Debug("Downloaded item processing failed", "number", results[index].Header.Number, "hash", results[index].Header.Hash(), "err", err)
		return errInvalidChain
	}
	return nil
}

// processFastSyncContent takes fetch results from the queue and writes them to the
// database. It also controls the synchronisation of state nodes of the pivot block.
//
/**
processFastSyncContent:
从队列中获取结果并将其写入leveldb。 它还控制枢纽块(pivot block) state trie node 的同步
 */
func (d *Downloader) processFastSyncContent(latest *types.Header) error {
	// Start syncing state of the reported head block. This should get us most of
	// the state of the pivot block.
	//
	//
	// 开始同步报告的头块的state。 这应该使我们获得枢轴块的大部分state
	// (什么 枢纽块, state 同步不是只有 lastest块的同步么)
	//
	// todo 这里就是发起state 同步的开始
	stateSync := d.syncState(latest.Root)
	defer stateSync.Cancel()
	go func() {
		// 阻塞,等待接收 结束信号
		if err := stateSync.Wait(); err != nil && err != errCancelStateFetch {
			d.queue.Close() // wake up WaitResults
		}
	}()
	// Figure out the ideal pivot block. Note, that this goalpost may move if the
	// sync takes long enough for the chain head to move significantly.
	//
	// (判断)找出理想的枢轴块。 请注意，如果同步花费足够长的时间使链头明显移动
	// <因为, 同步时间过长,对端节点上的peer可能已经增长了>，则该球门柱(goalpost: 门柱, 即 枢纽块)可能会移动
	pivot := uint64(0)
	// 这里先算出 枢纽块
	if height := latest.Number.Uint64(); height > uint64(fsMinFullBlocks) {
		// TODO fast其实不是从当前对端节点的最高块开始,而是从最高块往前64个块开始同步
		pivot = height - uint64(fsMinFullBlocks)
	}
	// To cater for moving pivot points, track the pivot block and subsequently
	// accumulated download results separately.
	//
	// 为了适应枢轴点的移动，请分别跟踪枢轴块和随后累积的下载结果
	var (
		// 锁定在枢轴块中，最终可能会更改
		// 记录历史的pivot fetchResult
		oldPivot *fetchResult   // Locked in pivot block, might change eventually
		// 枢轴之后的下载fetchResult
		// 即 对应pivot fetchResult 后面的去下载的fetchResult
		oldTail  []*fetchResult // Downloaded content after the pivot
	)

	for {
		// Wait for the next batch of downloaded data to be available, and if the pivot
		// block became stale, move the goalpost
		//
		// 等待下一批下载的数据可用，并且如果枢轴块变得陈旧，请移动球门柱。
		// 说白了就是动态的去调整 pivot
		// 从 queue.resultCache 中拿出一部分 `fetchResult` 进行处理
		results := d.queue.Results(oldPivot == nil) // Block if we're not monitoring pivot staleness 如果我们不监视轴心陈旧，则阻止
		if len(results) == 0 {
			// If pivot sync is done, stop
			// 如果完成数据 枢纽同步，请停止
			if oldPivot == nil {
				return stateSync.Cancel()
			}
			// If sync failed, stop
			// 如果同步失败，请停止
			select {
			case <-d.cancelCh:
				return stateSync.Cancel()
			default:
			}
		}

		// 回调
		if d.chainInsertHook != nil {
			d.chainInsertHook(results)
		}

		// 收集各个 pivot fetchResult
		// 和 pivot fetchResult 之后的 fetchResult
		// 和 从 queue.resultCache 中拿出一部分 `fetchResult`
		if oldPivot != nil {
			results = append(append([]*fetchResult{oldPivot}, oldTail...), results...)
		}
		// Split around the pivot block and process the two sides via fast/full sync
		//
		// 围绕枢轴块拆分并通过 fast/full 同步处理两侧 (blocks?)
		if atomic.LoadInt32(&d.committed) == 0 {
			latest = results[len(results)-1].Header
			if height := latest.Number.Uint64(); height > pivot+2*uint64(fsMinFullBlocks) {
				log.Warn("Pivot became stale, moving", "old", pivot, "new", height-uint64(fsMinFullBlocks))
				pivot = height - uint64(fsMinFullBlocks)
			}
		}

		//	P: pivot点的 `fetchResult`
		// 	beforeP: pivot点之前的所有 `fetchResult`
		//  afterP: pivot点之后的所有 `fetchResult`
		P, beforeP, afterP := splitAroundPivot(pivot, results)

		// 将beforeP的 blocks 和 receipt 刷入磁盘
		// todo 注意: state 数据不在这里刷盘哦
		if err := d.commitFastSyncData(beforeP, stateSync); err != nil {
			return err
		}


		// 当,存在pivot点的 `fetchResult`时
		if P != nil {
			// If new pivot block found, cancel old state retrieval and restart
			//
			// 如果找到新的枢轴块，请取消旧状态检索并重新启动
			if oldPivot != P {
				stateSync.Cancel()

				// todo 由于找到了新的 pivot, 则重新启动同步 state 数据
				stateSync = d.syncState(P.Header.Root)
				defer stateSync.Cancel()
				go func() {
					if err := stateSync.Wait(); err != nil && err != errCancelStateFetch {
						d.queue.Close() // wake up WaitResults
					}
				}()
				oldPivot = P
			}
			// Wait for completion, occasionally checking for pivot staleness
			//
			// 等待完成，偶尔检查枢纽是否陈旧
			select {

			// 接收到同步结束信号
			case <-stateSync.done:
				if stateSync.err != nil {
					return stateSync.err
				}

				// todo 刷入 pivot 点的`fetchResult`
				if err := d.commitPivotBlock(P); err != nil {
					return err
				}
				oldPivot = nil

			case <-time.After(time.Second):
				oldTail = afterP
				continue
			}
		}
		// Fast sync done, pivot commit done, full import
		//
		// 快速同步完成，数据 `枢纽` 提交完成，数据完全(同步)导入
		// 将afterP 的数据刷盘
		if err := d.importBlockResults(afterP); err != nil {
			return err
		}
	}
}

func splitAroundPivot(pivot uint64, results []*fetchResult) (p *fetchResult, before, after []*fetchResult) {
	for _, result := range results {
		num := result.Header.Number.Uint64()
		switch {
		case num < pivot:
			before = append(before, result)
		case num == pivot:
			p = result
		default:
			after = append(after, result)
		}
	}
	return p, before, after
}


// 将 blocks 和 receipt 刷入磁盘
// todo 注意: state 数据不在这里刷盘哦
func (d *Downloader) commitFastSyncData(results []*fetchResult, stateSync *stateSync) error {
	// Check for any early termination requests
	if len(results) == 0 {
		return nil
	}
	select {
	case <-d.quitCh:
		return errCancelContentProcessing
	case <-stateSync.done:
		if err := stateSync.Wait(); err != nil {
			return err
		}
	default:
	}
	// Retrieve the a batch of results to import
	first, last := results[0].Header, results[len(results)-1].Header
	log.Debug("Inserting fast-sync blocks", "items", len(results),
		"firstnum", first.Number, "firsthash", first.Hash(),
		"lastnumn", last.Number, "lasthash", last.Hash(),
	)
	blocks := make([]*types.Block, len(results))
	receipts := make([]types.Receipts, len(results))
	for i, result := range results {
		blocks[i] = types.NewBlockWithHeader(result.Header).WithBody(result.Transactions, result.Uncles)
		receipts[i] = result.Receipts
	}

	// todo 将 blocks 和 receipt 刷入磁盘
	if index, err := d.blockchain.InsertReceiptChain(blocks, receipts); err != nil {
		log.Debug("Downloaded item processing failed", "number", results[index].Header.Number, "hash", results[index].Header.Hash(), "err", err)
		return errInvalidChain
	}
	return nil
}

// 刷入 pivot 点的`fetchResult`
func (d *Downloader) commitPivotBlock(result *fetchResult) error {
	block := types.NewBlockWithHeader(result.Header).WithBody(result.Transactions, result.Uncles)
	log.Debug("Committing fast sync pivot as new head", "number", block.Number(), "hash", block.Hash())
	if _, err := d.blockchain.InsertReceiptChain([]*types.Block{block}, []types.Receipts{result.Receipts}); err != nil {
		return err
	}
	if err := d.blockchain.FastSyncCommitHead(block.Hash()); err != nil {
		return err
	}
	atomic.StoreInt32(&d.committed, 1)
	return nil
}

// DeliverHeaders injects a new batch of block headers received from a remote
// node into the download schedule.
func (d *Downloader) DeliverHeaders(id string, headers []*types.Header) (err error) {
	return d.deliver(id, d.headerCh, &headerPack{id, headers}, headerInMeter, headerDropMeter)
}

// DeliverBodies injects a new batch of block bodies received from a remote node.
func (d *Downloader) DeliverBodies(id string, transactions [][]*types.Transaction, uncles [][]*types.Header) (err error) {
	return d.deliver(id, d.bodyCh, &bodyPack{id, transactions, uncles}, bodyInMeter, bodyDropMeter)
}

// DeliverReceipts injects a new batch of receipts received from a remote node.
func (d *Downloader) DeliverReceipts(id string, receipts [][]*types.Receipt) (err error) {
	return d.deliver(id, d.receiptCh, &receiptPack{id, receipts}, receiptInMeter, receiptDropMeter)
}

// DeliverNodeData injects a new batch of node state data received from a remote node.
func (d *Downloader) DeliverNodeData(id string, data [][]byte) (err error) {
	return d.deliver(id, d.stateCh, &statePack{id, data}, stateInMeter, stateDropMeter)
}

// deliver injects a new batch of data received from a remote node.
func (d *Downloader) deliver(id string, destCh chan dataPack, packet dataPack, inMeter, dropMeter metrics.Meter) (err error) {
	// Update the delivery metrics for both good and failed deliveries
	inMeter.Mark(int64(packet.Items()))
	defer func() {
		if err != nil {
			dropMeter.Mark(int64(packet.Items()))
		}
	}()
	// Deliver or abort if the sync is canceled while queuing
	d.cancelLock.RLock()
	cancel := d.cancelCh
	d.cancelLock.RUnlock()
	if cancel == nil {
		return errNoSyncActive
	}
	select {

	// todo 将数据包 丢给 通道
	case destCh <- packet:
		return nil
	case <-cancel:
		return errNoSyncActive
	}
}

// qosTuner is the quality of service tuning loop that occasionally gathers the
// peer latency statistics and updates the estimated request round trip time.
//
// qosTuner是服务质量调整循环，它偶尔会收集 对端peer 延迟统计信息并更新估计的请求往返时间。
//
//
// 知识点:
//
// RTT(Round Trip Time)：一个连接的往返时间，即数据发送时刻到接收到确认的时刻的差值；
// RTO(Retransmission Time Out)：重传超时时间，即从数据发送时刻算起，超过这个时间便执行重传。
// RTT和RTO 的关系是：由于网络波动的不确定性，每个RTT都是动态变化的，所以RTO也应随着RTT动态变化。
//
//
//
func (d *Downloader) qosTuner() {

	// 	调节
	//	d.rttEstimate: 下载请求为目标的往返时间
	// 	d.rttConfidence: 对估算的RTT的置信度（单位：允许原子操作的百万分之一）

	for {
		// Retrieve the current median RTT and integrate into the previoust target RTT
		// 检索当前的中位RTT并整合到先前的目标RTT中
		rtt := time.Duration((1-qosTuningImpact)*float64(atomic.LoadUint64(&d.rttEstimate)) + qosTuningImpact*float64(d.peers.medianRTT()))
		atomic.StoreUint64(&d.rttEstimate, uint64(rtt))

		// A new RTT cycle passed, increase our confidence in the estimated RTT
		// 新的RTT周期过去了，提高了我们对估算的RTT的信心
		conf := atomic.LoadUint64(&d.rttConfidence)
		conf = conf + (1000000-conf)/2
		atomic.StoreUint64(&d.rttConfidence, conf)

		// Log the new QoS values and sleep until the next RTT
		// 记录新的QoS值并休眠直到下一个RTT
		//
		// QoS（Quality of Service，服务质量）
		log.Debug("Recalculated downloader QoS values", "rtt", rtt, "confidence", float64(conf)/1000000.0, "ttl", d.requestTTL())
		select {
		case <-d.quitCh:
			return
		case <-time.After(rtt):
		}
	}
}

// qosReduceConfidence is meant to be called when a new peer joins the downloader's
// peer set, needing to reduce the confidence we have in out QoS estimates.
func (d *Downloader) qosReduceConfidence() {
	// If we have a single peer, confidence is always 1
	peers := uint64(d.peers.Len())
	if peers == 0 {
		// Ensure peer connectivity races don't catch us off guard
		return
	}
	if peers == 1 {
		atomic.StoreUint64(&d.rttConfidence, 1000000)
		return
	}
	// If we have a ton of peers, don't drop confidence)
	if peers >= uint64(qosConfidenceCap) {
		return
	}
	// Otherwise drop the confidence factor
	conf := atomic.LoadUint64(&d.rttConfidence) * (peers - 1) / peers
	if float64(conf)/1000000 < rttMinConfidence {
		conf = uint64(rttMinConfidence * 1000000)
	}
	atomic.StoreUint64(&d.rttConfidence, conf)

	rtt := time.Duration(atomic.LoadUint64(&d.rttEstimate))
	log.Debug("Relaxed downloader QoS values", "rtt", rtt, "confidence", float64(conf)/1000000.0, "ttl", d.requestTTL())
}

// requestRTT returns the current target round trip time for a download request
// to complete in.
//
// Note, the returned RTT is .9 of the actually estimated RTT. The reason is that
// the downloader tries to adapt queries to the RTT, so multiple RTT values can
// be adapted to, but smaller ones are preferred (stabler download stream).
func (d *Downloader) requestRTT() time.Duration {
	return time.Duration(atomic.LoadUint64(&d.rttEstimate)) * 9 / 10
}

// requestTTL returns the current timeout allowance for a single download request
// to finish under.
//
// requestTTL: 返回当前的超时限额，以完成单个下载请求
// TTL: Time To Live 生存时间
func (d *Downloader) requestTTL() time.Duration {
	var (
		// 获取, 以下载请求为目标的往返时间
		rtt  = time.Duration(atomic.LoadUint64(&d.rttEstimate))
		// 对估算的RTT的置信度（单位：允许原子操作的百万分之一）
		conf = float64(atomic.LoadUint64(&d.rttConfidence)) / 1000000.0
	)

	// 根据几个值,计算出 TTL
	ttl := time.Duration(ttlScaling) * time.Duration(float64(rtt)/conf)
	if ttl > ttlLimit {
		ttl = ttlLimit
	}
	return ttl
}
