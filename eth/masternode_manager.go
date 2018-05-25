// Copyright 2015 The go-ethereum Authors
// Copyright 2018 The go-etherzero Authors
// This file is part of the go-ethereum library.
//
// The go-ethereum library is free software: you can redistribute it and/or modify
// it under the terms of the GNU Lesser General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The go-ethereum library is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE. See the
// GNU Lesser General Public License for more details.
//
// You should have received a copy of the GNU Lesser General Public License
// along with the go-ethereum library. If not, see <http://www.gnu.org/licenses/>.

package eth

import (
	"fmt"
	"math/big"
	"sort"
	"sync"
	"sync/atomic"

	"bytes"
	"encoding/binary"
	"github.com/ethzero/go-ethzero/common"
	"github.com/ethzero/go-ethzero/consensus"
	"github.com/ethzero/go-ethzero/contracts/masternode/contract"
	"github.com/ethzero/go-ethzero/core"
	"github.com/ethzero/go-ethzero/core/types"
	"github.com/ethzero/go-ethzero/eth/downloader"
	"github.com/ethzero/go-ethzero/eth/fetcher"
	"github.com/ethzero/go-ethzero/ethdb"
	"github.com/ethzero/go-ethzero/event"
	"github.com/ethzero/go-ethzero/log"
	"github.com/ethzero/go-ethzero/masternode"
	"github.com/ethzero/go-ethzero/p2p"
	"github.com/ethzero/go-ethzero/params"
	"github.com/pkg/errors"
	"net"
)

const (
	SIGNATURES_TOTAL = 10
)

type MasternodeManager struct {
	networkId uint64

	fastSync  uint32 // Flag whether fast sync is enabled (gets disabled if we already have blocks)
	acceptTxs uint32 // Flag whether we're considered synchronised (enables transaction processing)

	txpool      txPool
	blockchain  *core.BlockChain
	chainconfig *params.ChainConfig
	maxPeers    int

	fetcher *fetcher.Fetcher
	peers   *peerSet

	masternodes *masternode.MasternodeSet

	enableds map[string]*masternode.Masternode //id -> masternode

	is *InstantSend

	winner *MasternodePayments

	active *masternode.ActiveMasternode

	SubProtocols []p2p.Protocol

	eventMux      *event.TypeMux
	txCh          chan core.TxPreEvent
	txSub         event.Subscription
	minedBlockSub *event.TypeMuxSubscription

	// channels for fetcher, syncer, txsyncLoop
	newPeerCh   chan *peer
	txsyncCh    chan *txsync
	quitSync    chan struct{}
	noMorePeers chan struct{}

	// wait group is used for graceful shutdowns during downloading
	// and processing
	wg sync.WaitGroup

	log log.Logger

	contract *contract.Contract
	srvr     *p2p.Server
}

// NewProtocolManager returns a new ethereum sub protocol manager. The Ethereum sub protocol manages peers capable
// with the ethereum network.
func NewMasternodeManager(config *params.ChainConfig, mode downloader.SyncMode, networkId uint64, mux *event.TypeMux, txpool txPool, engine consensus.Engine, blockchain *core.BlockChain, chaindb ethdb.Database) (*MasternodeManager, error) {
	// Create the protocol manager with the base fields
	manager := &MasternodeManager{
		networkId:   networkId,
		eventMux:    mux,
		txpool:      txpool,
		blockchain:  blockchain,
		chainconfig: config,
		peers:       newPeerSet(),
		newPeerCh:   make(chan *peer),
		noMorePeers: make(chan struct{}),
		txsyncCh:    make(chan *txsync),
		quitSync:    make(chan struct{}),
	}

	//if len(manager.SubProtocols) == 0 {
	//	return nil, errIncompatibleConfig
	//}
	validator := func(header *types.Header) error {
		return engine.VerifyHeader(blockchain, header, true)
	}
	heighter := func() uint64 {
		return blockchain.CurrentBlock().NumberU64()
	}

	inserter := func(blocks types.Blocks) (int, error) {
		// If fast sync is running, deny importing weird blocks
		if atomic.LoadUint32(&manager.fastSync) == 1 {
			log.Warn("Discarded bad propagated block", "number", blocks[0].Number(), "hash", blocks[0].Hash())
			return 0, nil
		}
		atomic.StoreUint32(&manager.acceptTxs, 1) // Mark initial sync done on any fetcher import
		return manager.blockchain.InsertChain(blocks)
	}

	vote := func(block *types.Block) bool {
		return manager.winner.ProcessBlock(block)
	}

	manager.fetcher = fetcher.New(blockchain.GetBlockByHash, validator, manager.BroadcastBlock, heighter, inserter, manager.removePeer, vote)

	return manager, nil
}

func (mm *MasternodeManager) removePeer(id string) {
	// Short circuit if the peer was already removed
	peer := mm.peers.Peer(id)
	if peer == nil {
		return
	}
	log.Debug("Removing Etherzero masternode peer", "peer", id)

	if err := mm.peers.Unregister(id); err != nil {
		log.Error("Peer removal failed", "peer", id, "err", err)
	}
	// Hard disconnect at the networking layer
	if peer != nil {
		peer.Peer.Disconnect(p2p.DiscUselessPeer)
	}
}

func (mm *MasternodeManager) Start(srvr *p2p.Server, contract *contract.Contract) {
	mm.contract = contract
	mm.srvr = srvr

	mns, err := masternode.NewMasternodeSet(contract)
	if err != nil {
		log.Error("masternode.NewMasternodeSet", "error", err)
	}
	mm.masternodes = mns

	mm.active = masternode.NewActiveMasternode(srvr)

	go mm.masternodeLoop()
}

func (mm *MasternodeManager) Stop() {

}

func (mm *MasternodeManager) newPeer(pv int, p *p2p.Peer, rw p2p.MsgReadWriter) *peer {
	return newPeer(pv, p, newMeteredMsgWriter(rw))
}

// Deterministically select the oldest/best masternode to pay on the network
// Pass in the hash value of the block that participates in the calculation.
// Dash is the Hash passed to the first 100 blocks.
// If use the current block Hash, there is a risk that the current block will be discarded.
func (mm *MasternodeManager) GetNextMasternodeInQueueForPayment(block common.Hash) (*masternode.Masternode, error) {

	var (
		paids        []int
		tenthNetWork = mm.masternodes.Len() / 10
		countTenth   = 0
		highest      *big.Int
		winner       *masternode.Masternode
		sortMap      map[int]*masternode.Masternode
	)
	if mm.masternodes == nil {
		return nil, errors.New("no masternode detected")
	}
	for _, node := range mm.masternodes.Nodes() {
		i := int(node.Height.Int64())
		paids = append(paids, i)
		sortMap[i] = node
	}

	sort.Ints(paids)

	for _, i := range paids {
		fmt.Printf("%s\t%d\n", i, sortMap[i].CalculateScore(block))
		score := sortMap[i].CalculateScore(block)
		if score.Cmp(highest) > 0 {
			highest = score
			winner = sortMap[i]
		}
		countTenth++
		if countTenth >= tenthNetWork {
			break
		}
	}

	return winner, nil
}

func (mm *MasternodeManager) GetMasternodeRank(id string) (int, bool) {

	var rank int = 0
	mm.syncer()
	block := mm.blockchain.CurrentBlock()

	if block == nil {
		mm.log.Info("ERROR: GetBlockHash() failed at BlockHeight:%d ", block.Number())
		return rank, false
	}
	masternodeScores := mm.GetMasternodeScores(block.Hash(), 1)

	tRank := 0
	for _, masternode := range masternodeScores {
		//info := MasternodeInfo()
		tRank++
		if id == masternode.ID {
			rank = tRank
			break
		}
	}
	return rank, true
}

func (mm *MasternodeManager) GetMasternodeScores(blockHash common.Hash, minProtocol int) map[*big.Int]*masternode.Masternode {

	masternodeScores := make(map[*big.Int]*masternode.Masternode)

	for _, m := range mm.masternodes.Nodes() {
		masternodeScores[m.CalculateScore(blockHash)] = m
	}
	return masternodeScores
}

func (mm *MasternodeManager) ProcessTxLockVotes(votes []*types.TxLockVote) bool {

	rank, ok := mm.GetMasternodeRank(mm.active.ID)
	if !ok {
		log.Info("InstantSend::Vote -- Can't calculate rank for masternode ", mm.active.ID, " rank: ", rank)
		return false
	} else if rank > SIGNATURES_TOTAL {
		log.Info("InstantSend::Vote -- Masternode not in the top ", SIGNATURES_TOTAL, " (", rank, ")")
		return false
	}
	log.Info("InstantSend::Vote -- In the top ", SIGNATURES_TOTAL, " (", rank, ")")
	return mm.is.ProcessTxLockVotes(votes)
}

func (mm *MasternodeManager) ProcessPaymentVotes(vote *MasternodePaymentVote) bool {

	return mm.winner.Vote(vote)
}

func (mn *MasternodeManager) ProcessTxVote(tx *types.Transaction) bool {

	mn.is.ProcessTxLockRequest(tx)
	log.Info("Transaction Lock Request accepted,", "txHash:", tx.Hash().String(), "MasternodeId", mn.active.ID)
	mn.is.Accept(tx)
	mn.is.Vote(tx.Hash())

	return true
}

func (mn *MasternodeManager) updateActiveMasternode() {
	var state int
	
	n := mn.masternodes.Node(mn.active.ID)
	if n == nil {
		state = masternode.ACTIVE_MASTERNODE_NOT_CAPABLE
	} else if int(n.Node.TCP) != mn.active.Addr.Port {
		log.Error("updateActiveMasternode", "Port", n.Node.TCP, "active.Port", mn.active.Addr.Port)
		state = masternode.ACTIVE_MASTERNODE_NOT_CAPABLE
	} else if !n.Node.IP.Equal(mn.active.Addr.IP) {
		log.Error("updateActiveMasternode", "IP", n.Node.IP, "active.IP", mn.active.Addr.IP)
		state = masternode.ACTIVE_MASTERNODE_NOT_CAPABLE
	} else {
		state = masternode.ACTIVE_MASTERNODE_STARTED
	}
	
	mn.active.SetState(state)
}
func (mn *MasternodeManager) masternodeLoop() {
	mn.updateActiveMasternode()
	if mn.active.State() == masternode.ACTIVE_MASTERNODE_STARTED {
		fmt.Println("masternodeCheck true")
	} else if !mn.srvr.MasternodeAddr.IP.Equal(net.IP{}) {
		var misc [32]byte
		misc[0] = 1
		copy(misc[1:17], mn.srvr.Config.MasternodeAddr.IP)
		binary.BigEndian.PutUint16(misc[17:19], uint16(mn.srvr.Config.MasternodeAddr.Port))

		var buf bytes.Buffer
		buf.Write(mn.srvr.Self().ID[:])
		buf.Write(misc[:])
		d := "0x4da274fd" + common.Bytes2Hex(buf.Bytes())
		fmt.Println("Masternode transaction data:", d)
	}

	mn.masternodes.Show()

	joinCh := make(chan *contract.ContractJoin, 32)
	quitCh := make(chan *contract.ContractQuit, 32)
	joinSub, err1 := mn.contract.WatchJoin(nil, joinCh)
	if err1 != nil {
		// TODO: exit
		return
	}
	quitSub, err2 := mn.contract.WatchQuit(nil, quitCh)
	if err2 != nil {
		// TODO: exit
		return
	}

	//pingMsg := &masternode.PingMsg{
	//	ID: self.node.ID,
	//	IP: self.node.IP,
	//	Port: self.node.TCP,
	//}
	//t := time.NewTimer(time.Second * 5)

	for {
		select {
		case join := <-joinCh:
			fmt.Println("join", common.Bytes2Hex(join.Id[:]))
			node, err := mn.masternodes.NodeJoin(join.Id)
			if err == nil {
				if bytes.Equal(join.Id[:], mn.srvr.Self().ID[0:32]) {
					mn.updateActiveMasternode()
				} else {
					mn.srvr.AddPeer(node.Node)
				}
				mn.masternodes.Show()
			}

		case quit := <-quitCh:
			fmt.Println("quit", common.Bytes2Hex(quit.Id[:]))
			mn.masternodes.NodeQuit(quit.Id)
			if bytes.Equal(quit.Id[:], mn.srvr.Self().ID[0:32]) {
				mn.updateActiveMasternode()
			}
			mn.masternodes.Show()

		case err := <-joinSub.Err():
			joinSub.Unsubscribe()
			fmt.Println("eventJoin err", err.Error())
		case err := <-quitSub.Err():
			quitSub.Unsubscribe()
			fmt.Println("eventQuit err", err.Error())

			//case <-t.C:
			//	pingMsg.Update(self.privateKey)
			//	peers := self.peers.peers
			//	for _, peer := range peers {
			//		fmt.Println("peer", peer.ID())
			//		if err := peer.SendMasternodePing(pingMsg); err != nil {
			//			fmt.Println("err:", err)
			//		}
			//	}
			//	t.Reset(time.Second * 100)
		}
	}
}
