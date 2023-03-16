package bcast

import (
	"context"
	"sync"
	"time"

	"github.com/ipfs/go-cid"
	logging "github.com/ipfs/go-log/v2"
	"golang.org/x/xerrors"

	"github.com/filecoin-project/go-state-types/abi"

	"github.com/filecoin-project/lotus/chain/types"
)

var log = logging.Logger("sub-cb")

const (
	// GcSanityCheck determines the number of epochs in the past
	// that will be garbage collected from the current epoch.
	GcSanityCheck = 5
	// GcLookback determines the number of epochs kept in the consistent
	// broadcast cache.
	GcLookback = 1000
)

type blksInfo struct {
	ctx    context.Context
	cancel context.CancelFunc
	blks   []cid.Cid
}

type bcastDict struct {
	// thread-safe map impl for the dictionary
	// sync.Map accepts `any` as keys and values.
	// To make it type safe and only support the right
	// types we use this auxiliary type.
	m *sync.Map
}

func (bd *bcastDict) load(key []byte) (*blksInfo, bool) {
	v, ok := bd.m.Load(string(key))
	if !ok {
		return nil, ok
	}
	return v.(*blksInfo), ok
}

func (bd *bcastDict) store(key []byte, d *blksInfo) {
	bd.m.Store(string(key), d)
}

func (bd *bcastDict) blkLen(key []byte) int {
	v, ok := bd.m.Load(string(key))
	if !ok {
		return 0
	}
	return len(v.(*blksInfo).blks)
}

type ConsistentBCast struct {
	lk    sync.RWMutex
	delay time.Duration
	m     map[abi.ChainEpoch]*bcastDict
}

func newBcastDict() *bcastDict {
	return &bcastDict{new(sync.Map)}
}

func BCastKey(bh *types.BlockHeader) []byte {
	return bh.Ticket.VRFProof
}

func NewConsistentBCast(delay time.Duration) *ConsistentBCast {
	return &ConsistentBCast{
		delay: delay,
		m:     make(map[abi.ChainEpoch]*bcastDict),
	}
}

func cidExists(cids []cid.Cid, c cid.Cid) bool {
	for _, v := range cids {
		if v == c {
			return true
		}
	}
	return false
}

func (bInfo *blksInfo) eqErr() error {
	bInfo.cancel()
	return xerrors.Errorf("different blocks with the same ticket already seen")
}

func (cb *ConsistentBCast) Len() int {
	cb.lk.RLock()
	defer cb.lk.RUnlock()
	return len(cb.m)
}

// RcvBlock is called every time a new block is received through the network.
//
// This function keeps track of all the blocks with a specific VRFProof received
// for the same height. Every time a new block with a VRFProof not seen at certain
// height is received, a new timer is triggered to wait for the delay time determined by
// the consistent broadcast before informing the syncer. During this time, if a new
// block with the same VRFProof for that height is received, it means a miner is
// trying to equivocate, and both blocks are discarded.
//
// The delay time should be set to a value high enough to allow any block sent for
// certain epoch to be propagated to a large amount of miners in the network.
func (cb *ConsistentBCast) RcvBlock(ctx context.Context, blk *types.BlockMsg) {
	cb.lk.Lock()
	bcastDict, ok := cb.m[blk.Header.Height]
	if !ok {
		bcastDict = newBcastDict()
		cb.m[blk.Header.Height] = bcastDict
	}
	cb.lk.Unlock()
	key := BCastKey(blk.Header)
	blkCid := blk.Cid()

	bInfo, ok := bcastDict.load(key)
	if ok {
		if len(bInfo.blks) > 1 {
			log.Errorf("equivocation detected for height %d: %s", blk.Header.Height, bInfo.eqErr())
			return
		}

		if !cidExists(bInfo.blks, blkCid) {
			bcastDict.store(key, &blksInfo{bInfo.ctx, bInfo.cancel, append(bInfo.blks, blkCid)})
			log.Errorf("equivocation detected for height %d: %s", blk.Header.Height, bInfo.eqErr())
			return
		}
		return
	}

	ctx, cancel := context.WithTimeout(ctx, cb.delay)
	bcastDict.store(key, &blksInfo{ctx, cancel, []cid.Cid{blkCid}})
}

// WaitForDelivery is called before informing the syncer about a new block
// to check if the consistent broadcast delay triggered or if the block should
// be held off for a bit more time.
func (cb *ConsistentBCast) WaitForDelivery(bh *types.BlockHeader) error {
	cb.lk.RLock()
	bcastDict := cb.m[bh.Height]
	cb.lk.RUnlock()
	key := BCastKey(bh)
	bInfo, ok := bcastDict.load(key)
	if !ok {
		return xerrors.Errorf("something went wrong, unknown block with Epoch + VRFProof (cid=%s) in consistent broadcast storage", key)
	}
	// Wait for the timeout
	<-bInfo.ctx.Done()
	if bcastDict.blkLen(key) > 1 {
		return xerrors.Errorf("equivocation detected for epoch %d. Two blocks being broadcast with same VRFProof", bh.Height)
	}
	return nil
}

func (cb *ConsistentBCast) GarbageCollect(currEpoch abi.ChainEpoch) {
	cb.lk.Lock()
	defer cb.lk.Unlock()

	// keep currEpoch-2 and delete a few more in the past
	// as a sanity-check
	// Garbage collection is triggered before block delivery,
	// and we use the sanity-check in case there were a few rounds
	// without delivery, and the garbage collection wasn't triggered
	// for a few epochs.
	for i := 0; i < GcSanityCheck; i++ {
		if currEpoch > GcLookback {
			delete(cb.m, currEpoch-abi.ChainEpoch(GcLookback+i))
		}
	}
}
