/*
 * Copyright 2018 The ThunderDB Authors.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package sqlchain

import (
	"encoding/binary"
	"fmt"
	"sync"
	"time"

	bolt "github.com/coreos/bbolt"
	"gitlab.com/thunderdb/ThunderDB/crypto/hash"
	"gitlab.com/thunderdb/ThunderDB/crypto/kms"
	"gitlab.com/thunderdb/ThunderDB/kayak"
	"gitlab.com/thunderdb/ThunderDB/proto"
	"gitlab.com/thunderdb/ThunderDB/rpc"
	ct "gitlab.com/thunderdb/ThunderDB/sqlchain/types"
	"gitlab.com/thunderdb/ThunderDB/utils/log"
	wt "gitlab.com/thunderdb/ThunderDB/worker/types"
)

var (
	metaBucket              = [4]byte{0x0, 0x0, 0x0, 0x0}
	metaStateKey            = []byte("thunderdb-state")
	metaBlockIndexBucket    = []byte("thunderdb-block-index-bucket")
	metaHeightIndexBucket   = []byte("thunderdb-query-height-index-bucket")
	metaRequestIndexBucket  = []byte("thunderdb-query-reqeust-index-bucket")
	metaResponseIndexBucket = []byte("thunderdb-query-response-index-bucket")
	metaAckIndexBucket      = []byte("thunderdb-query-ack-index-bucket")
)

// heightToKey converts a height in int32 to a key in bytes.
func heightToKey(h int32) (key []byte) {
	key = make([]byte, 4)
	binary.BigEndian.PutUint32(key, uint32(h))
	return
}

// keyToHeight converts a height back from a key in bytes.
func keyToHeight(k []byte) int32 {
	return int32(binary.BigEndian.Uint32(k))
}

// Chain represents a sql-chain.
type Chain struct {
	db *bolt.DB
	bi *blockIndex
	qi *queryIndex
	cl *rpc.Caller
	rt *runtime

	stopCh    chan struct{}
	blocks    chan *ct.Block
	responses chan *wt.ResponseHeader
	acks      chan *wt.AckHeader
}

// NewChain creates a new sql-chain struct.
func NewChain(c *Config) (chain *Chain, err error) {
	err = c.Genesis.VerifyAsGenesis()

	if err != nil {
		return
	}

	// Open DB file
	db, err := bolt.Open(c.DataFile, 0600, nil)

	if err != nil {
		return
	}

	// Create buckets for chain meta
	if err = db.Update(func(tx *bolt.Tx) (err error) {
		bucket, err := tx.CreateBucketIfNotExists(metaBucket[:])

		if err != nil {
			return
		}

		if _, err = bucket.CreateBucketIfNotExists(metaBlockIndexBucket); err != nil {
			return
		}

		_, err = bucket.CreateBucketIfNotExists(metaHeightIndexBucket)
		return
	}); err != nil {
		return
	}

	// Create chain state
	chain = &Chain{
		db:        db,
		bi:        newBlockIndex(c),
		qi:        newQueryIndex(),
		cl:        rpc.NewCaller(),
		rt:        newRunTime(c),
		stopCh:    make(chan struct{}),
		blocks:    make(chan *ct.Block),
		responses: make(chan *wt.ResponseHeader),
		acks:      make(chan *wt.AckHeader),
	}

	if err = chain.pushBlock(c.Genesis); err != nil {
		return nil, err
	}

	return
}

// LoadChain loads the chain state from the specified database and rebuilds a memory index.
func LoadChain(c *Config) (chain *Chain, err error) {
	// Open DB file
	db, err := bolt.Open(c.DataFile, 0600, nil)

	if err != nil {
		return
	}

	// Create chain state
	chain = &Chain{
		db:        db,
		bi:        newBlockIndex(c),
		qi:        newQueryIndex(),
		cl:        rpc.NewCaller(),
		rt:        newRunTime(c),
		stopCh:    make(chan struct{}),
		blocks:    make(chan *ct.Block),
		responses: make(chan *wt.ResponseHeader),
		acks:      make(chan *wt.AckHeader),
	}

	err = chain.db.View(func(tx *bolt.Tx) (err error) {
		// Read state struct
		meta := tx.Bucket(metaBucket[:])
		st := &state{}
		err = st.UnmarshalBinary(meta.Get(metaStateKey))

		if err != nil {
			return err
		}

		chain.rt.setHead(st)

		// Read blocks and rebuild memory index
		var last *blockNode
		var index int32
		blocks := meta.Bucket(metaBlockIndexBucket)
		nodes := make([]blockNode, blocks.Stats().KeyN)

		if err = blocks.ForEach(func(k, v []byte) (err error) {
			block := &ct.Block{}

			if err = block.UnmarshalBinary(v); err != nil {
				return
			}

			log.WithFields(log.Fields{
				"peer":  chain.rt.getPeerInfoString(),
				"block": block,
			}).Debug("Loading block from database")
			parent := (*blockNode)(nil)

			if last == nil {
				if err = block.SignedHeader.VerifyAsGenesis(); err != nil {
					return
				}

				// Set constant fields from genesis block
				chain.rt.setGenesis(block)
			} else if block.SignedHeader.ParentHash.IsEqual(&last.hash) {
				if err = block.SignedHeader.Verify(); err != nil {
					return
				}

				parent = last
			} else {
				parent = chain.bi.lookupNode(&block.SignedHeader.BlockHash)

				if parent == nil {
					return ErrParentNotFound
				}
			}

			height := chain.rt.getHeightFromTime(block.SignedHeader.Timestamp)
			nodes[index].initBlockNode(height, block, parent)
			last = &nodes[index]
			index++
			return
		}); err != nil {
			return
		}

		// Read queries and rebuild memory index
		heights := meta.Bucket(metaHeightIndexBucket)
		resp := &wt.SignedResponseHeader{}
		ack := &wt.SignedAckHeader{}

		if err = heights.ForEach(func(k, v []byte) (err error) {
			h := keyToHeight(k)

			if resps := heights.Bucket(k).Bucket(
				metaResponseIndexBucket); resps != nil {
				if err = resps.ForEach(func(k []byte, v []byte) (err error) {
					if err = resp.UnmarshalBinary(v); err != nil {
						return
					}

					return chain.qi.addResponse(h, resp)
				}); err != nil {
					return
				}
			}

			if acks := heights.Bucket(k).Bucket(metaAckIndexBucket); acks != nil {
				if err = acks.ForEach(func(k []byte, v []byte) (err error) {
					if err = ack.UnmarshalBinary(v); err != nil {
						return
					}

					return chain.qi.addAck(h, ack)
				}); err != nil {
					return
				}
			}

			return
		}); err != nil {
			return
		}

		return
	})

	return
}

// pushBlock pushes the signed block header to extend the current main chain.
func (c *Chain) pushBlock(b *ct.Block) (err error) {
	// Prepare and encode
	h := c.rt.getHeightFromTime(b.SignedHeader.Timestamp)
	node := newBlockNode(h, b, c.rt.getHead().node)
	st := &state{
		node:   node,
		Head:   node.hash,
		Height: node.height,
	}
	var encBlock, encState []byte

	if encBlock, err = b.MarshalBinary(); err != nil {
		return
	}

	if encState, err = st.MarshalBinary(); err != nil {
		return
	}

	// Update in transaction
	return c.db.Update(func(tx *bolt.Tx) (err error) {
		if err = tx.Bucket(metaBucket[:]).Put(metaStateKey, encState); err != nil {
			return
		}

		if err = tx.Bucket(metaBucket[:]).Bucket(metaBlockIndexBucket).Put(
			node.indexKey(), encBlock); err != nil {
			return
		}

		c.rt.setHead(st)
		c.bi.addBlock(node)
		c.qi.setSignedBlock(h, b)
		log.WithFields(log.Fields{
			"peer":        c.rt.getPeerInfoString(),
			"block":       b.SignedHeader.BlockHash.String(),
			"producer":    b.SignedHeader.Producer,
			"blocktime":   b.SignedHeader.Timestamp.Format(time.RFC3339Nano),
			"blockheight": c.rt.getHeightFromTime(b.SignedHeader.Timestamp),
			"headblock": fmt.Sprintf("%s <- %s",
				func() string {
					if st.node.parent != nil {
						return st.node.parent.hash.String()
					}
					return "|"
				}(), st.Head.String()),
			"headheight": c.rt.getHead().Height,
		}).Debug("Pushed new block")
		return
	})
}

func ensureHeight(tx *bolt.Tx, k []byte) (hb *bolt.Bucket, err error) {
	b := tx.Bucket(metaBucket[:]).Bucket(metaHeightIndexBucket)

	if hb = b.Bucket(k); hb == nil {
		// Create and initialize bucket in new height
		if hb, err = b.CreateBucket(k); err != nil {
			return
		}

		if _, err = hb.CreateBucket(metaRequestIndexBucket); err != nil {
			return
		}

		if _, err = hb.CreateBucket(metaResponseIndexBucket); err != nil {
			return
		}

		if _, err = hb.CreateBucket(metaAckIndexBucket); err != nil {
			return
		}
	}

	return
}

// pushResponedQuery pushes a responsed, signed and verified query into the chain.
func (c *Chain) pushResponedQuery(resp *wt.SignedResponseHeader) (err error) {
	h := c.rt.getHeightFromTime(resp.Request.Timestamp)
	k := heightToKey(h)
	var enc []byte

	if enc, err = resp.MarshalBinary(); err != nil {
		return
	}

	return c.db.Update(func(tx *bolt.Tx) (err error) {
		heightBucket, err := ensureHeight(tx, k)

		if err != nil {
			return
		}

		if err = heightBucket.Bucket(metaResponseIndexBucket).Put(
			resp.HeaderHash[:], enc); err != nil {
			return
		}

		// Always put memory changes which will not be affected by rollback after DB operations
		return c.qi.addResponse(h, resp)
	})
}

// pushAckedQuery pushes a acknowledged, signed and verified query into the chain.
func (c *Chain) pushAckedQuery(ack *wt.SignedAckHeader) (err error) {
	h := c.rt.getHeightFromTime(ack.SignedResponseHeader().Timestamp)
	k := heightToKey(h)
	var enc []byte

	if enc, err = ack.MarshalBinary(); err != nil {
		return
	}

	return c.db.Update(func(tx *bolt.Tx) (err error) {
		b, err := ensureHeight(tx, k)

		if err != nil {
			return
		}

		// TODO(leventeliu): this doesn't seem right to use an error to detect key existence.
		if err = b.Bucket(metaAckIndexBucket).Put(
			ack.HeaderHash[:], enc,
		); err != nil && err != bolt.ErrIncompatibleValue {
			return
		}

		// Always put memory changes which will not be affected by rollback after DB operations
		if err = c.qi.addAck(h, ack); err != nil {
			return
		}

		return
	})
}

// produceBlock prepares, signs and advises the pending block to the orther peers.
func (c *Chain) produceBlock(now time.Time) (err error) {
	// Retrieve local key pair
	priv, err := kms.GetLocalPrivateKey()

	if err != nil {
		return
	}

	// Pack and sign block
	block := &ct.Block{
		SignedHeader: ct.SignedHeader{
			Header: ct.Header{
				Version:     0x01000000,
				Producer:    c.rt.getServer().ID,
				GenesisHash: c.rt.genesisHash,
				ParentHash:  c.rt.getHead().Head,
				// MerkleRoot: will be set by Block.PackAndSignBlock(PrivateKey)
				Timestamp: now,
			},
			// BlockHash/Signee/Signature: will be set by Block.PackAndSignBlock(PrivateKey)
		},
		Queries: c.qi.markAndCollectUnsignedAcks(c.rt.getNextTurn()),
	}

	if err = block.PackAndSignBlock(priv); err != nil {
		return
	}

	// Send to pending list
	c.blocks <- block
	log.WithFields(log.Fields{
		"peer":       c.rt.getPeerInfoString(),
		"curr_turn":  c.rt.getNextTurn(),
		"now_time":   now.Format(time.RFC3339Nano),
		"block_hash": block.SignedHeader.BlockHash.String(),
	}).Debug("Produced new block")

	// Advise new block to the other peers
	req := &MuxAdviseNewBlockReq{
		Envelope: proto.Envelope{
			// TODO(leventeliu): Add fields.
		},
		DatabaseID: c.rt.databaseID,
		AdviseNewBlockReq: AdviseNewBlockReq{
			Block: block,
		},
	}
	method := fmt.Sprintf("%s.%s", c.rt.muxService.ServiceName, "AdviseNewBlock")
	peers := c.rt.getPeers()
	wg := &sync.WaitGroup{}

	for _, s := range peers.Servers {
		if s.ID != c.rt.getServer().ID {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp := &MuxAdviseAckedQueryResp{}
				if err = c.cl.CallNode(s.ID, method, req, resp); err != nil {
					log.WithFields(log.Fields{
						"peer":       c.rt.getPeerInfoString(),
						"curr_turn":  c.rt.getNextTurn(),
						"now_time":   now.Format(time.RFC3339Nano),
						"block_hash": block.SignedHeader.BlockHash.String(),
					}).WithError(err).Error(
						"Failed to advise new block")
				}
			}()
		}
	}

	wg.Wait()
	return
}

func (c *Chain) syncBlock() {
	// Try to fetch if the the block of the current turn is not advised yet
	if h := c.rt.getNextTurn() - 1; c.rt.getHead().Height < h {
		var err error
		req := &MuxFetchBlockReq{
			Envelope: proto.Envelope{
				// TODO(leventeliu): Add fields.
			},
			DatabaseID: c.rt.databaseID,
			FetchBlockReq: FetchBlockReq{
				Height: h,
			},
		}
		resp := &MuxFetchBlockResp{}
		method := fmt.Sprintf("%s.%s", c.rt.muxService.ServiceName, "FetchBlock")
		peers := c.rt.getPeers()
		succ := false

		for i, s := range peers.Servers {
			if s.ID != c.rt.getServer().ID {
				if err = c.cl.CallNode(s.ID, method, req, resp); err != nil || resp.Block == nil {
					log.WithFields(log.Fields{
						"peer":        c.rt.getPeerInfoString(),
						"remote":      fmt.Sprintf("[%d/%d] %s", i, len(peers.Servers), s.ID),
						"curr_turn":   c.rt.getNextTurn(),
						"head_height": c.rt.getHead().Height,
						"head_block":  c.rt.getHead().Head.String(),
					}).WithError(err).Error(
						"Failed to fetch block from peer")
				} else {
					c.blocks <- resp.Block
					log.WithFields(log.Fields{
						"peer":        c.rt.getPeerInfoString(),
						"remote":      fmt.Sprintf("[%d/%d] %s", i, len(peers.Servers), s.ID),
						"curr_turn":   c.rt.getNextTurn(),
						"head_height": c.rt.getHead().Height,
						"head_block":  c.rt.getHead().Head.String(),
					}).Debug(
						"Fetch block from remote peer successfully")
					succ = true
					break
				}
			}
		}

		if !succ {
			log.WithFields(log.Fields{
				"peer":        c.rt.getPeerInfoString(),
				"curr_turn":   c.rt.getNextTurn(),
				"head_height": c.rt.getHead().Height,
				"head_block":  c.rt.getHead().Head.String(),
			}).Error(
				"Cannot get block from any peer")
		}
	}
}

// runCurrentTurn does the check and runs block producing if its my turn.
func (c *Chain) runCurrentTurn(now time.Time) {
	defer func() {
		c.rt.setNextTurn()
		c.qi.advanceBarrier(c.rt.getMinValidHeight())
	}()

	log.WithFields(log.Fields{
		"peer":        c.rt.getPeerInfoString(),
		"curr_turn":   c.rt.getNextTurn(),
		"head_height": c.rt.getHead().Height,
		"head_block":  c.rt.getHead().Head.String(),
		"now_time":    now.Format(time.RFC3339Nano),
	}).Debug("Run current turn")

	if !c.rt.isMyTurn() {
		return
	}

	if err := c.produceBlock(now); err != nil {
		log.WithFields(log.Fields{
			"peer":      c.rt.getPeerInfoString(),
			"curr_turn": c.rt.getNextTurn(),
			"now":       now.Format(time.RFC3339Nano),
		}).WithError(err).Error(
			"Failed to produce block")
	}
}

// mainCycle runs main cycle of the sql-chain.
func (c *Chain) mainCycle() {
	defer func() {
		c.rt.wg.Done()
		// Signal worker goroutines to stop
		close(c.stopCh)
	}()

	for {
		select {
		case <-c.rt.stopCh:
			return
		default:
			c.syncBlock()

			if t, d := c.rt.nextTick(); d > 0 {
				log.WithFields(log.Fields{
					"peer":        c.rt.getPeerInfoString(),
					"next_turn":   c.rt.getNextTurn(),
					"head_height": c.rt.head.Height,
					"head_block":  c.rt.head.Head.String(),
					"now_time":    t.Format(time.RFC3339Nano),
					"duration":    d,
				}).Debug("Main cycle")
				time.Sleep(d)
			} else {
				c.runCurrentTurn(t)
			}
		}
	}
}

// sync synchronizes blocks and queries from the other peers.
func (c *Chain) sync() (err error) {
	log.WithFields(log.Fields{
		"peer": c.rt.getPeerInfoString(),
	}).Debug("Synchronizing chain state")

	for {
		now := c.rt.now()
		height := c.rt.getHeightFromTime(now)

		if c.rt.getNextTurn() >= height {
			break
		}

		for c.rt.getNextTurn() <= height {
			// TODO(leventeliu): fetch blocks and queries.
			c.rt.setNextTurn()
		}
	}

	return
}

func (c *Chain) processBlocks() {
	rsCh := make(chan struct{})
	rsWG := &sync.WaitGroup{}
	returnStash := func(stash []*ct.Block) {
		defer rsWG.Done()
		for _, block := range stash {
			select {
			case c.blocks <- block:
			case <-rsCh:
				return
			}
		}
	}

	defer func() {
		close(rsCh)
		rsWG.Wait()
		c.rt.wg.Done()
	}()

	var stash []*ct.Block
	for {
		select {
		case block := <-c.blocks:
			if h := c.rt.getHeightFromTime(block.Timestamp()); h > c.rt.getNextTurn()-1 {
				// Stash newer blocks for later check
				if stash == nil {
					stash = make([]*ct.Block, 0)
				}
				stash = append(stash, block)
			} else {
				// Process block
				if h < c.rt.getNextTurn()-1 {
					// TODO(leventeliu): check and add to fork list.
				} else {
					if block.SignedHeader.Producer == c.rt.getServer().ID {
						if err := c.pushBlock(block); err != nil {

						}
					} else {
						if err := c.CheckAndPushNewBlock(block); err != nil {

						}
					}
				}

				// Return all stashed blocks to pending channel
				if stash != nil {
					rsWG.Add(1)
					go returnStash(stash)
					stash = nil
				}
			}
		case <-c.stopCh:
			return
		}
	}
}

func (c *Chain) processResponses() {
	defer c.rt.wg.Done()
	for {
		select {
		case <-c.stopCh:
			return
		}
	}
}

func (c *Chain) processAcks() {
	defer c.rt.wg.Done()
	for {
		select {
		case <-c.stopCh:
			return
		}
	}
}

// Start starts the main process of the sql-chain.
func (c *Chain) Start() (err error) {
	if err = c.sync(); err != nil {
		return
	}

	c.rt.wg.Add(1)
	go c.processBlocks()
	c.rt.wg.Add(1)
	go c.processResponses()
	c.rt.wg.Add(1)
	go c.processAcks()
	c.rt.wg.Add(1)
	go c.mainCycle()
	c.rt.startService(c)
	return
}

// Stop stops the main process of the sql-chain.
func (c *Chain) Stop() (err error) {
	// Stop main process
	log.WithFields(log.Fields{"peer": c.rt.getPeerInfoString()}).Debug("Stopping chain")
	c.rt.stop()
	log.WithFields(log.Fields{"peer": c.rt.getPeerInfoString()}).Debug("Chain service stopped")
	// Close database file
	err = c.db.Close()
	log.WithFields(log.Fields{"peer": c.rt.getPeerInfoString()}).Debug("Chain database closed")
	return
}

// FetchBlock fetches the block at specified height from local cache.
func (c *Chain) FetchBlock(height int32) (b *ct.Block, err error) {
	if n := c.rt.getHead().node.ancestor(height); n != nil {
		k := n.indexKey()
		err = c.db.View(func(tx *bolt.Tx) (err error) {
			if v := tx.Bucket(metaBucket[:]).Bucket(metaBlockIndexBucket).Get(k); v != nil {
				b = &ct.Block{}
				err = b.UnmarshalBinary(v)
			}

			return
		})
	}

	return
}

// FetchAckedQuery fetches the acknowledged query from local cache.
func (c *Chain) FetchAckedQuery(height int32, header *hash.Hash) (
	ack *wt.SignedAckHeader, err error,
) {
	if ack, err = c.qi.getAck(height, header); err != nil {
		err = c.db.View(func(tx *bolt.Tx) (err error) {
			for i := height - c.rt.queryTTL; i <= height; i++ {
				if b := tx.Bucket(metaBucket[:]).Bucket(metaHeightIndexBucket).Bucket(
					heightToKey(height)); b != nil {
					if v := b.Bucket(metaAckIndexBucket).Get(header[:]); v != nil {
						dec := &wt.SignedAckHeader{}

						if err = dec.UnmarshalBinary(v); err != nil {
							ack = dec
							break
						}
					}
				}
			}

			return
		})
	}

	return
}

func (c *Chain) syncAckedQuery(height int32, ack *hash.Hash, id proto.NodeID) (err error) {
	req := &MuxFetchAckedQueryReq{
		Envelope: proto.Envelope{
			// TODO(leventeliu): Add fields.
		},
		DatabaseID: c.rt.databaseID,
		FetchAckedQueryReq: FetchAckedQueryReq{
			Height:                height,
			SignedAckedHeaderHash: ack,
		},
	}
	resp := &MuxFetchAckedQueryResp{}
	method := fmt.Sprintf("%s.%s", c.rt.muxService.ServiceName, "FetchAckedQuery")

	if err = c.cl.CallNode(id, method, req, resp); err != nil {
		log.WithFields(log.Fields{
			"peer": c.rt.getPeerInfoString(),
		}).WithError(err).Error(
			"Failed to fetch acked query")
		return
	}

	return c.VerifyAndPushAckedQuery(resp.Ack)
}

// CheckAndPushNewBlock implements ChainRPCServer.CheckAndPushNewBlock.
func (c *Chain) CheckAndPushNewBlock(block *ct.Block) (err error) {
	height := c.rt.getHeightFromTime(block.SignedHeader.Timestamp)
	head := c.rt.getHead()
	peers := c.rt.getPeers()
	total := int32(len(peers.Servers))
	next := func() int32 {
		if total > 0 {
			return (head.Height + 1) % total
		}
		return -1
	}()
	log.WithFields(log.Fields{
		"peer":        c.rt.getPeerInfoString(),
		"block":       block.SignedHeader.BlockHash.String(),
		"producer":    block.SignedHeader.Producer,
		"blocktime":   block.SignedHeader.Timestamp.Format(time.RFC3339Nano),
		"blockheight": height,
		"blockparent": block.SignedHeader.ParentHash.String(),
		"headblock":   head.Head.String(),
		"headheight":  head.Height,
	}).Debug("Checking new block from other peer")

	if head.Height == height && head.Head.IsEqual(&block.SignedHeader.BlockHash) {
		// Maybe already set by FetchBlock
		return nil
	} else if !block.SignedHeader.ParentHash.IsEqual(&head.Head) {
		// Pushed block must extend the best chain
		log.WithFields(log.Fields{
			"peer":        c.rt.getPeerInfoString(),
			"block":       block.SignedHeader.BlockHash.String(),
			"producer":    block.SignedHeader.Producer,
			"blocktime":   block.SignedHeader.Timestamp.Format(time.RFC3339Nano),
			"blockheight": height,
			"blockparent": block.SignedHeader.ParentHash.String(),
			"headblock":   head.Head.String(),
			"headheight":  height,
		}).WithError(ErrInvalidBlock).Error(
			"Failed to check new block")
		return ErrInvalidBlock
	}

	// Check block producer
	index, found := peers.Find(block.SignedHeader.Producer)

	if !found {
		return ErrUnknownProducer
	}

	if index != next {
		log.WithFields(log.Fields{
			"peer":     c.rt.getPeerInfoString(),
			"expected": next,
			"actual":   index,
		}).WithError(err).Error(
			"Failed to check new block")
		return ErrInvalidProducer
	}

	// TODO(leventeliu): check if too many periods are skipped or store block for future use.
	// if height-c.rt.getHead().Height > X {
	// 	...
	// }

	// Check queries
	for _, q := range block.Queries {
		var ok bool

		if ok, err = c.qi.checkAckFromBlock(height, &block.SignedHeader.BlockHash, q); err != nil {
			return
		}

		if !ok {
			if err = c.syncAckedQuery(height, q, block.SignedHeader.Producer); err != nil {
				return
			}

			if _, err = c.qi.checkAckFromBlock(height, &block.SignedHeader.BlockHash, q); err != nil {
				return
			}
		}
	}

	// Verify block signatures
	if err = block.Verify(); err != nil {
		return
	}

	return c.pushBlock(block)
}

// VerifyAndPushResponsedQuery verifies a responsed and signed query, and pushed it if valid.
func (c *Chain) VerifyAndPushResponsedQuery(resp *wt.SignedResponseHeader) (err error) {
	// TODO(leventeliu): check resp.
	if c.rt.queryTimeIsExpired(resp.Timestamp) {
		return ErrQueryExpired
	}

	if err = resp.Verify(); err != nil {
		return
	}

	return c.pushResponedQuery(resp)
}

// VerifyAndPushAckedQuery verifies a acknowledged and signed query, and pushed it if valid.
func (c *Chain) VerifyAndPushAckedQuery(ack *wt.SignedAckHeader) (err error) {
	// TODO(leventeliu): check ack.
	if c.rt.queryTimeIsExpired(ack.SignedResponseHeader().Timestamp) {
		return ErrQueryExpired
	}

	if err = ack.Verify(); err != nil {
		return
	}

	return c.pushAckedQuery(ack)
}

// UpdatePeers updates peer list of the sql-chain.
func (c *Chain) UpdatePeers(peers *kayak.Peers) error {
	return c.rt.updatePeers(peers)
}
