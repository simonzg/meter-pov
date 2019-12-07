// Copyright (c) 2018 The VeChainThor developers

// Distributed under the GNU Lesser General Public License v3.0 software license, see the accompanying
// file LICENSE or <https://www.gnu.org/licenses/lgpl-3.0.html>

package chain

import (
	"bytes"
	"fmt"
	"sync"

	"github.com/dfinlab/meter/block"
	"github.com/dfinlab/meter/co"
	"github.com/dfinlab/meter/kv"
	"github.com/dfinlab/meter/meter"
	"github.com/dfinlab/meter/tx"
	"github.com/ethereum/go-ethereum/rlp"
	"github.com/pkg/errors"
)

const (
	blockCacheLimit    = 512
	receiptsCacheLimit = 512
)

var errNotFound = errors.New("not found")
var errBlockExist = errors.New("block already exists")
var errParentNotFinalized = errors.New("parent is not finalized")

// Chain describes a persistent block chain.
// It's thread-safe.
type Chain struct {
	kv           kv.GetPutter
	ancestorTrie *ancestorTrie
	genesisBlock *block.Block
	bestBlock    *block.Block
	leafBlock    *block.Block
	bestQC       *block.QuorumCert
	tag          byte
	caches       caches
	rw           sync.RWMutex
	tick         co.Signal

	bestQCCandidate *block.QuorumCert
}

type caches struct {
	rawBlocks *cache
	receipts  *cache
}

// New create an instance of Chain.
func New(kv kv.GetPutter, genesisBlock *block.Block) (*Chain, error) {
	if genesisBlock.Header().Number() != 0 {
		return nil, errors.New("genesis number != 0")
	}
	if len(genesisBlock.Transactions()) != 0 {
		return nil, errors.New("genesis block should not have transactions")
	}
	ancestorTrie := newAncestorTrie(kv)
	var bestBlock, leafBlock *block.Block

	genesisID := genesisBlock.Header().ID()
	if bestBlockID, err := loadBestBlockID(kv); err != nil {
		if !kv.IsNotFound(err) {
			return nil, err
		}
		// no genesis yet
		raw, err := rlp.EncodeToBytes(genesisBlock)
		if err != nil {
			return nil, err
		}

		batch := kv.NewBatch()
		if err := saveBlockRaw(batch, genesisID, raw); err != nil {
			return nil, err
		}

		if err := saveBestBlockID(batch, genesisID); err != nil {
			return nil, err
		}

		if err := ancestorTrie.Update(batch, genesisID, genesisBlock.Header().ParentID()); err != nil {
			return nil, err
		}

		if err := batch.Write(); err != nil {
			return nil, err
		}

		bestBlock = genesisBlock
	} else {
		existGenesisID, err := ancestorTrie.GetAncestor(bestBlockID, 0)
		if err != nil {
			return nil, err
		}
		if existGenesisID != genesisID {
			return nil, errors.New("genesis mismatch")
		}
		raw, err := loadBlockRaw(kv, bestBlockID)
		if err != nil {
			return nil, err
		}
		bestBlock, err = (&rawBlock{raw: raw}).Block()
		if err != nil {
			return nil, err
		}
		if bestBlock.Header().Number() == 0 && bestBlock.QC == nil {
			fmt.Println("QC of best block is empty, set it to genesis QC")
			bestBlock.QC = block.GenesisQC()
		}

		// Load leaf block
		if leafBlockID, err := loadLeafBlockID(kv); err == nil {
			leafBlockRaw, err := loadBlockRaw(kv, leafBlockID)
			if err != nil {
				return nil, err
			}
			leafBlock, err = (&rawBlock{raw: leafBlockRaw}).Block()
			if err != nil {
				return nil, err
			}
		}
	}

	rawBlocksCache := newCache(blockCacheLimit, func(key interface{}) (interface{}, error) {
		raw, err := loadBlockRaw(kv, key.(meter.Bytes32))
		if err != nil {
			return nil, err
		}
		return &rawBlock{raw: raw}, nil
	})

	receiptsCache := newCache(receiptsCacheLimit, func(key interface{}) (interface{}, error) {
		return loadBlockReceipts(kv, key.(meter.Bytes32))
	})

	if leafBlock == nil {
		fmt.Println("Leaf Block is empty, set it to genesis block")
		leafBlock = bestBlock
	} else {
		fmt.Println("Leaf Block", leafBlock.CompactString())
		fmt.Println("*** Start pruning")
		// remove all leaf blocks that are not finalized
		for leafBlock.Header().TotalScore() > bestBlock.Header().TotalScore() {
			parentID, err := ancestorTrie.GetAncestor(leafBlock.Header().ID(), leafBlock.Header().Number()-1)
			if err != nil {
				break
			}
			deletedBlock, err := deleteBlock(kv, leafBlock.Header().ID())
			if err != nil {
				fmt.Println("Error delete block: ", err)
				break
			}
			fmt.Println("Deleted block:", deletedBlock.CompactString())
			parentRaw, err := loadBlockRaw(kv, parentID)
			if err != nil {
				fmt.Println("Error load parent", err)
			}
			parentBlk, err := (&rawBlock{raw: parentRaw}).Block()
			leafBlock = parentBlk
		}

		saveLeafBlockID(kv, leafBlock.Header().ID())
	}

	bestQC, err := loadBestQC(kv)
	if err != nil {
		fmt.Println("Best QC is not in database, set it to use genesis QC, error: ", err)
		bestQC = block.GenesisQC()
	}
	fmt.Println("--------------------------------------------------")
	fmt.Println("                 CHAIN INITIALIZED                ")
	fmt.Println("--------------------------------------------------")
	fmt.Println("LeafBlock Header: ", leafBlock.Header())
	fmt.Println("LeafBlock QC: ", leafBlock.QC)
	fmt.Println("Leaf Block: ", leafBlock.CompactString())
	fmt.Println("Best Block: ", bestBlock.CompactString())
	fmt.Println("Best QC: ", bestQC.String())
	fmt.Println("--------------------------------------------------")
	c := &Chain{
		kv:           kv,
		ancestorTrie: ancestorTrie,
		genesisBlock: genesisBlock,
		bestBlock:    bestBlock,
		leafBlock:    leafBlock,
		bestQC:       bestQC,
		tag:          genesisBlock.Header().ID()[31],
		caches: caches{
			rawBlocks: rawBlocksCache,
			receipts:  receiptsCache,
		},
		bestQCCandidate: nil,
	}

	return c, nil
}

// Tag returns chain tag, which is the last byte of genesis id.
func (c *Chain) Tag() byte {
	return c.tag
}

// GenesisBlock returns genesis block.
func (c *Chain) GenesisBlock() *block.Block {
	return c.genesisBlock
}

// BestBlock returns the newest block on trunk.
func (c *Chain) BestBlock() *block.Block {
	c.rw.RLock()
	defer c.rw.RUnlock()
	return c.bestBlock
}

func (c *Chain) BestQC() *block.QuorumCert {
	c.rw.RLock()
	defer c.rw.RUnlock()
	return c.bestQC
}

func (c *Chain) RemoveBlock(blockID meter.Bytes32) error {
	c.rw.Lock()
	defer c.rw.Unlock()
	_, err := c.getBlockHeader(blockID)
	if err != nil {
		if c.IsNotFound(err) {
			return err
		}
		if block.Number(blockID) <= c.bestBlock.Header().Number() {
			return errors.New("could not remove finalized block")
		}
		return removeBlockRaw(c.kv, blockID)
	}
	return err
}

// AddBlock add a new block into block chain.
// Once reorg happened (len(Trunk) > 0 && len(Branch) >0), Fork.Branch will be the chain transitted from trunk to branch.
// Reorg happens when isTrunk is true.
func (c *Chain) AddBlock(newBlock *block.Block, receipts tx.Receipts, finalize bool) (*Fork, error) {
	c.rw.Lock()
	defer c.rw.Unlock()

	newBlockID := newBlock.Header().ID()

	if header, err := c.getBlockHeader(newBlockID); err != nil {
		if !c.IsNotFound(err) {
			return nil, err
		}
	} else {
		parentFinalized := c.IsBlockFinalized(header.ParentID())

		// block already there
		newHeader := newBlock.Header()
		if header.Number() == newHeader.Number() &&
			header.ParentID() == newHeader.ParentID() &&
			string(header.Signature()) == string(newHeader.Signature()) &&
			header.ReceiptsRoot() == newHeader.ReceiptsRoot() &&
			header.Timestamp() == newHeader.Timestamp() &&
			parentFinalized == true &&
			finalize == true {
			// if the current block is the finalized version of saved block, update it accordingly
			// do nothing
		} else {
			return nil, errBlockExist
		}
	}

	// newBlock.Header().Finalized = finalize
	parent, err := c.getBlockHeader(newBlock.Header().ParentID())
	if err != nil {
		if c.IsNotFound(err) {
			return nil, errors.New("parent missing")
		}
		return nil, err
	}

	// finalized block need to have a finalized parent block
	/** FIXME: comment temporarily
	if finalize == true && parent.Finalized == false {
		return nil, errParentNotFinalized
	}
	**/

	raw := block.BlockEncodeBytes(newBlock)

	batch := c.kv.NewBatch()

	if err := saveBlockRaw(batch, newBlockID, raw); err != nil {
		return nil, err
	}
	if err := saveBlockReceipts(batch, newBlockID, receipts); err != nil {
		return nil, err
	}

	if err := c.ancestorTrie.Update(batch, newBlockID, newBlock.Header().ParentID()); err != nil {
		return nil, err
	}

	for i, tx := range newBlock.Transactions() {
		meta, err := loadTxMeta(c.kv, tx.ID())
		if err != nil {
			if !c.IsNotFound(err) {
				return nil, err
			}
		}
		meta = append(meta, TxMeta{
			BlockID:  newBlockID,
			Index:    uint64(i),
			Reverted: receipts[i].Reverted,
		})
		if err := saveTxMeta(batch, tx.ID(), meta); err != nil {
			return nil, err
		}
	}

	var fork *Fork
	isTrunk := c.isTrunk(newBlock.Header())
	if isTrunk {
		if fork, err = c.buildFork(newBlock.Header(), c.bestBlock.Header()); err != nil {
			return nil, err
		}
		if finalize == true {
			if err := saveBestBlockID(batch, newBlockID); err != nil {
				return nil, err
			}
			c.bestBlock = newBlock
			if newBlock.Header().TotalScore() > c.leafBlock.Header().TotalScore() {
				if err := saveLeafBlockID(batch, newBlockID); err != nil {
					return nil, err
				}

				c.leafBlock = newBlock
			}
			err := c.UpdateBestQC()
			if err != nil {
				fmt.Println("Error during update QC: ", err)
			}
		} else {
			if newBlock.Header().TotalScore() > c.leafBlock.Header().TotalScore() {
				if err := saveLeafBlockID(batch, newBlockID); err != nil {
					return nil, err
				}

				c.leafBlock = newBlock
			}
		}
	} else {
		fork = &Fork{Ancestor: parent, Branch: []*block.Header{newBlock.Header()}}
	}

	if err := batch.Write(); err != nil {
		return nil, err
	}

	c.caches.rawBlocks.Add(newBlockID, newRawBlock(raw, newBlock))
	c.caches.receipts.Add(newBlockID, receipts)

	c.tick.Broadcast()
	return fork, nil
}

func (c *Chain) IsBlockFinalized(id meter.Bytes32) bool {
	if block.Number(id) <= c.bestBlock.Header().Number() {
		return true
	}
	return false
}

// GetBlockHeader get block header by block id.
func (c *Chain) GetBlockHeader(id meter.Bytes32) (*block.Header, error) {
	c.rw.RLock()
	defer c.rw.RUnlock()
	return c.getBlockHeader(id)
}

// GetBlockBody get block body by block id.
func (c *Chain) GetBlockBody(id meter.Bytes32) (*block.Body, error) {
	c.rw.RLock()
	defer c.rw.RUnlock()
	return c.getBlockBody(id)
}

// GetBlock get block by id.
func (c *Chain) GetBlock(id meter.Bytes32) (*block.Block, error) {
	c.rw.RLock()
	defer c.rw.RUnlock()
	return c.getBlock(id)
}

// GetBlockRaw get block rlp encoded bytes for given id.
// Never modify the returned raw block.
func (c *Chain) GetBlockRaw(id meter.Bytes32) (block.Raw, error) {
	c.rw.RLock()
	defer c.rw.RUnlock()
	raw, err := c.getRawBlock(id)
	if err != nil {
		return nil, err
	}
	return raw.raw, nil
}

// GetBlockReceipts get all tx receipts in the block for given block id.
func (c *Chain) GetBlockReceipts(id meter.Bytes32) (tx.Receipts, error) {
	c.rw.RLock()
	defer c.rw.RUnlock()
	return c.getBlockReceipts(id)
}

// GetAncestorBlockID get ancestor block ID of descendant for given ancestor block.
func (c *Chain) GetAncestorBlockID(descendantID meter.Bytes32, ancestorNum uint32) (meter.Bytes32, error) {
	c.rw.RLock()
	defer c.rw.RUnlock()
	return c.ancestorTrie.GetAncestor(descendantID, ancestorNum)
}

// GetTransactionMeta get transaction meta info, on the chain defined by head block ID.
func (c *Chain) GetTransactionMeta(txID meter.Bytes32, headBlockID meter.Bytes32) (*TxMeta, error) {
	c.rw.RLock()
	defer c.rw.RUnlock()
	return c.getTransactionMeta(txID, headBlockID)
}

// GetTransaction get transaction for given block and index.
func (c *Chain) GetTransaction(blockID meter.Bytes32, index uint64) (*tx.Transaction, error) {
	c.rw.RLock()
	defer c.rw.RUnlock()
	return c.getTransaction(blockID, index)
}

// GetTransactionReceipt get tx receipt for given block and index.
func (c *Chain) GetTransactionReceipt(blockID meter.Bytes32, index uint64) (*tx.Receipt, error) {
	c.rw.RLock()
	defer c.rw.RUnlock()
	receipts, err := c.getBlockReceipts(blockID)
	if err != nil {
		return nil, err
	}
	if index >= uint64(len(receipts)) {
		return nil, errors.New("receipt index out of range")
	}
	return receipts[index], nil
}

// GetTrunkBlockID get block id on trunk by given block number.
func (c *Chain) GetTrunkBlockID(num uint32) (meter.Bytes32, error) {
	c.rw.RLock()
	defer c.rw.RUnlock()
	return c.ancestorTrie.GetAncestor(c.bestBlock.Header().ID(), num)
}

// GetTrunkBlockHeader get block header on trunk by given block number.
func (c *Chain) GetTrunkBlockHeader(num uint32) (*block.Header, error) {
	c.rw.RLock()
	defer c.rw.RUnlock()
	id, err := c.ancestorTrie.GetAncestor(c.bestBlock.Header().ID(), num)
	if err != nil {
		return nil, err
	}
	return c.getBlockHeader(id)
}

// GetTrunkBlock get block on trunk by given block number.
func (c *Chain) GetTrunkBlock(num uint32) (*block.Block, error) {
	c.rw.RLock()
	defer c.rw.RUnlock()
	id, err := c.ancestorTrie.GetAncestor(c.bestBlock.Header().ID(), num)
	if err != nil {
		return nil, err
	}
	return c.getBlock(id)
}

// GetTrunkBlockRaw get block raw on trunk by given block number.
func (c *Chain) GetTrunkBlockRaw(num uint32) (block.Raw, error) {
	c.rw.RLock()
	defer c.rw.RUnlock()
	id, err := c.ancestorTrie.GetAncestor(c.bestBlock.Header().ID(), num)
	if err != nil {
		return nil, err
	}
	raw, err := c.getRawBlock(id)
	if err != nil {
		return nil, err
	}
	return raw.raw, nil
}

// GetTrunkTransactionMeta get transaction meta info on trunk by given tx id.
func (c *Chain) GetTrunkTransactionMeta(txID meter.Bytes32) (*TxMeta, error) {
	c.rw.RLock()
	defer c.rw.RUnlock()
	return c.getTransactionMeta(txID, c.bestBlock.Header().ID())
}

// GetTrunkTransaction get transaction on trunk by given tx id.
func (c *Chain) GetTrunkTransaction(txID meter.Bytes32) (*tx.Transaction, *TxMeta, error) {
	c.rw.RLock()
	defer c.rw.RUnlock()
	meta, err := c.getTransactionMeta(txID, c.bestBlock.Header().ID())
	if err != nil {
		return nil, nil, err
	}
	tx, err := c.getTransaction(meta.BlockID, meta.Index)
	if err != nil {
		return nil, nil, err
	}
	return tx, meta, nil
}

// NewSeeker returns a new seeker instance.
func (c *Chain) NewSeeker(headBlockID meter.Bytes32) *Seeker {
	return newSeeker(c, headBlockID)
}

func (c *Chain) isTrunk(header *block.Header) bool {
	bestHeader := c.bestBlock.Header()
	fmt.Println(fmt.Sprintf("IsTrunk: header: %s, bestHeader: %s", header.ID().String(), bestHeader.ID().String()))

	if header.TotalScore() < bestHeader.TotalScore() {
		return false
	}

	if header.TotalScore() > bestHeader.TotalScore() {
		return true
	}

	// total scores are equal
	if bytes.Compare(header.ID().Bytes(), bestHeader.ID().Bytes()) < 0 {
		// smaller ID is preferred, since block with smaller ID usually has larger average score.
		// also, it's a deterministic decision.
		return true
	}
	return false
}

// Think about the example below:
//
//   B1--B2--B3--B4--B5--B6
//             \
//              \
//               b4--b5
//
// When call buildFork(B6, b5), the return values will be:
// ((B3, [B4, B5, B6], [b4, b5]), nil)
func (c *Chain) buildFork(trunkHead *block.Header, branchHead *block.Header) (*Fork, error) {
	var (
		trunk, branch []*block.Header
		err           error
		b1            = trunkHead
		b2            = branchHead
	)

	for {
		if b1.Number() > b2.Number() {
			trunk = append(trunk, b1)
			if b1, err = c.getBlockHeader(b1.ParentID()); err != nil {
				return nil, err
			}
			continue
		}
		if b1.Number() < b2.Number() {
			branch = append(branch, b2)
			if b2, err = c.getBlockHeader(b2.ParentID()); err != nil {
				return nil, err
			}
			continue
		}
		if b1.ID() == b2.ID() {
			// reverse trunk and branch
			for i, j := 0, len(trunk)-1; i < j; i, j = i+1, j-1 {
				trunk[i], trunk[j] = trunk[j], trunk[i]
			}
			for i, j := 0, len(branch)-1; i < j; i, j = i+1, j-1 {
				branch[i], branch[j] = branch[j], branch[i]
			}
			return &Fork{b1, trunk, branch}, nil
		}

		trunk = append(trunk, b1)
		branch = append(branch, b2)

		if b1, err = c.getBlockHeader(b1.ParentID()); err != nil {
			return nil, err
		}

		if b2, err = c.getBlockHeader(b2.ParentID()); err != nil {
			return nil, err
		}
	}
}

func (c *Chain) getRawBlock(id meter.Bytes32) (*rawBlock, error) {
	raw, err := c.caches.rawBlocks.GetOrLoad(id)
	if err != nil {
		return nil, err
	}

	return raw.(*rawBlock), nil
}

func (c *Chain) getBlockHeader(id meter.Bytes32) (*block.Header, error) {
	raw, err := c.getRawBlock(id)
	if err != nil {
		return nil, err
	}
	return raw.Header()
}

func (c *Chain) getBlockBody(id meter.Bytes32) (*block.Body, error) {
	raw, err := c.getRawBlock(id)
	if err != nil {
		return nil, err
	}
	return raw.Body()
}
func (c *Chain) getBlock(id meter.Bytes32) (*block.Block, error) {
	raw, err := c.getRawBlock(id)
	if err != nil {
		return nil, err
	}
	return raw.Block()
}

func (c *Chain) getBlockReceipts(blockID meter.Bytes32) (tx.Receipts, error) {
	receipts, err := c.caches.receipts.GetOrLoad(blockID)
	if err != nil {
		return nil, err
	}
	return receipts.(tx.Receipts), nil
}

func (c *Chain) getTransactionMeta(txID meter.Bytes32, headBlockID meter.Bytes32) (*TxMeta, error) {
	meta, err := loadTxMeta(c.kv, txID)
	if err != nil {
		return nil, err
	}
	for _, m := range meta {
		ancestorID, err := c.ancestorTrie.GetAncestor(headBlockID, block.Number(m.BlockID))
		if err != nil {
			if c.IsNotFound(err) {
				continue
			}
			return nil, err
		}
		if ancestorID == m.BlockID {
			return &m, nil
		}
	}
	return nil, errNotFound
}

func (c *Chain) getTransaction(blockID meter.Bytes32, index uint64) (*tx.Transaction, error) {
	body, err := c.getBlockBody(blockID)
	if err != nil {
		return nil, err
	}
	if index >= uint64(len(body.Txs)) {
		return nil, errors.New("tx index out of range")
	}
	return body.Txs[index], nil
}

// IsNotFound returns if an error means not found.
func (c *Chain) IsNotFound(err error) bool {
	return err == errNotFound || c.kv.IsNotFound(err)
}

// IsBlockExist returns if the error means block was already in the chain.
func (c *Chain) IsBlockExist(err error) bool {
	return err == errBlockExist
}

// NewTicker create a signal Waiter to receive event of head block change.
func (c *Chain) NewTicker() co.Waiter {
	return c.tick.NewWaiter()
}

// Block expanded block.Block to indicate whether it is obsolete
type Block struct {
	*block.Block
	Obsolete bool
}

// BlockReader defines the interface to read Block
type BlockReader interface {
	Read() ([]*Block, error)
}

type readBlock func() ([]*Block, error)

func (r readBlock) Read() ([]*Block, error) {
	return r()
}

// NewBlockReader generate an object that implements the BlockReader interface
func (c *Chain) NewBlockReader(position meter.Bytes32) BlockReader {
	return readBlock(func() ([]*Block, error) {
		c.rw.RLock()
		defer c.rw.RUnlock()

		bestID := c.bestBlock.Header().ID()
		if bestID == position {
			return nil, nil
		}

		var blocks []*Block
		for {
			positionBlock, err := c.getBlock(position)
			if err != nil {
				return nil, err
			}

			if block.Number(position) > block.Number(bestID) {
				blocks = append(blocks, &Block{positionBlock, true})
				position = positionBlock.Header().ParentID()
				continue
			}

			ancestor, err := c.ancestorTrie.GetAncestor(bestID, block.Number(position))
			if err != nil {
				return nil, err
			}

			if position == ancestor {
				next, err := c.nextBlock(bestID, block.Number(position))
				if err != nil {
					return nil, err
				}
				position = next.Header().ID()
				return append(blocks, &Block{next, false}), nil
			}

			blocks = append(blocks, &Block{positionBlock, true})
			position = positionBlock.Header().ParentID()
		}
	})
}

func (c *Chain) nextBlock(descendantID meter.Bytes32, num uint32) (*block.Block, error) {
	next, err := c.ancestorTrie.GetAncestor(descendantID, num+1)
	if err != nil {
		return nil, err
	}

	return c.getBlock(next)
}

func (c *Chain) LeafBlock() *block.Block {
	return c.leafBlock
}

func (c *Chain) UpdateLeafBlock() error {
	if c.leafBlock.Header().Number() < c.bestBlock.Header().Number() {
		c.leafBlock = c.bestBlock
		fmt.Println("!!! Move Leaf Block to: ", c.leafBlock.String())
	}
	return nil
}
func (c *Chain) UpdateBestQC() error {
	fmt.Println("in UpdateBestQC, bestQCCandidate=", c.bestQCCandidate.String(), "bestQC", c.bestQC.String(), "bestBlock.Height=", c.bestBlock.Header().Number())
	if c.leafBlock.Header().ID().String() == c.bestBlock.Header().ID().String() {
		if c.bestQCCandidate != nil && c.bestQCCandidate.QCHeight > c.bestQC.QCHeight && c.bestQCCandidate.QCHeight <= uint64(c.bestBlock.Header().Number()) {
			c.bestQC = c.bestQCCandidate
			c.bestQCCandidate = nil
			fmt.Println("!!! Move BestQC to: ", c.bestQC.String())
		}
		return saveBestQC(c.kv, c.bestQC)
	}
	fmt.Println("UpdateBestQC, bestQCCandidate=", c.bestQCCandidate.String(), ", bestBlock.Height=", c.bestBlock.Header().Number())
	if c.bestQCCandidate != nil && c.bestQCCandidate.QCHeight == uint64(c.bestBlock.Header().Number()) {
		c.bestQC = c.bestQCCandidate
		c.bestQCCandidate = nil
		fmt.Println("!!! Move BestQC to: ", c.bestQC.String())
		return saveBestQC(c.kv, c.bestQC)
	}
	id, err := c.ancestorTrie.GetAncestor(c.leafBlock.Header().ID(), c.bestBlock.Header().Number()+1)
	if err != nil {
		return err
	}
	raw, err := loadBlockRaw(c.kv, id)
	if err != nil {
		return err
	}
	blk, err := raw.DecodeBlockBody()
	if err != nil {
		return err
	}
	if blk.Header().ParentID().String() != c.bestBlock.Header().ID().String() {
		return errors.New("parent mismatch ")
	}
	c.bestQC = blk.QC
	fmt.Println("!!! Move BestQC to: ", c.bestQC.String())
	return saveBestQC(c.kv, c.bestQC)
}

func (c *Chain) SetBestQCCandidate(qc *block.QuorumCert) error {
	if qc.QCHeight < uint64(c.bestBlock.Header().Number()) {
		return errors.New("invalid best qc")
	}
	c.bestQCCandidate = qc
	return nil
}
