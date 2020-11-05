package p2pstore

import (
	"context"
	"encoding/hex"
	"sync"
	"time"

	"github.com/33cn/chain33/common/log/log15"
	"github.com/33cn/chain33/system/p2p/dht/protocol"
	types2 "github.com/33cn/chain33/system/p2p/dht/types"
	"github.com/33cn/chain33/types"
	"github.com/libp2p/go-libp2p-core/discovery"
	"github.com/libp2p/go-libp2p-core/peer"
	dht "github.com/libp2p/go-libp2p-kad-dht"
	kb "github.com/libp2p/go-libp2p-kbucket"
)

const (
	fetchShardPeer = "/chain33/fetch-shard-peer/1.0.0"
	fetchChunk     = "/chain33/fetch-chunk/1.0.0"
	storeChunk     = "/chain33/store-chunk/1.0.0"
	getHeader      = "/chain33/headers/1.0.0"
	getChunkRecord = "/chain33/chunk-record/1.0.0"
	fullNode       = "/chain33/full-node/1.0.0"
	// Deprecated: old version, use getHeader instead
	getHeaderOld = "/chain33/headerinfoReq/1.0.0"
)

const maxConcurrency = 10

var log = log15.New("module", "protocol.p2pstore")

//Protocol ...
type Protocol struct {
	*protocol.P2PEnv //协议共享接口变量

	//cache the notify message when other peers notify this node to store chunk.
	notifying sync.Map
	//send the message in <notifying> to this queue if chunk does not exist.
	notifyingQueue chan *types.ChunkInfoMsg
	//chunks that full node can provide without checking.
	chunkWhiteList sync.Map

	//a child table of healthy routing table without full nodes
	ShardHealthyRoutingTable *kb.RoutingTable

	//本节点保存的chunk的索引表，会随着网络拓扑结构的变化而变化
	localChunkInfo      map[string]LocalChunkInfo
	localChunkInfoMutex sync.RWMutex

	concurrency int64
}

func init() {
	protocol.RegisterProtocolInitializer(InitProtocol)
}

//InitProtocol initials the protocol.
func InitProtocol(env *protocol.P2PEnv) {
	p := &Protocol{
		P2PEnv:                   env,
		ShardHealthyRoutingTable: kb.NewRoutingTable(dht.KValue, kb.ConvertPeerID(env.Host.ID()), time.Minute, env.Host.Peerstore()),
		notifyingQueue:           make(chan *types.ChunkInfoMsg, 1024),
	}
	go p.updateShardHealthyRoutingTableRountine()
	p.initLocalChunkInfoMap()

	//注册p2p通信协议，用于处理节点之间请求
	protocol.RegisterStreamHandler(p.Host, fetchShardPeer, protocol.HandlerWithRW(p.handleStreamFetchShardPeers))
	protocol.RegisterStreamHandler(p.Host, getHeaderOld, p.handleStreamGetHeaderOld)
	protocol.RegisterStreamHandler(p.Host, fullNode, protocol.HandlerWithWrite(p.handleStreamIsFullNode))
	protocol.RegisterStreamHandler(p.Host, fetchChunk, p.handleStreamFetchChunk) //数据较大，采用特殊写入方式
	protocol.RegisterStreamHandler(p.Host, storeChunk, protocol.HandlerWithAuth(p.handleStreamStoreChunks))
	protocol.RegisterStreamHandler(p.Host, getHeader, protocol.HandlerWithAuthAndSign(p.handleStreamGetHeader))
	protocol.RegisterStreamHandler(p.Host, getChunkRecord, protocol.HandlerWithAuthAndSign(p.handleStreamGetChunkRecord))
	//同时注册eventHandler，用于处理blockchain模块发来的请求
	protocol.RegisterEventHandler(types.EventNotifyStoreChunk, p.handleEventNotifyStoreChunk)
	protocol.RegisterEventHandler(types.EventGetChunkBlock, p.handleEventGetChunkBlock)
	protocol.RegisterEventHandler(types.EventGetChunkBlockBody, p.handleEventGetChunkBlockBody)
	protocol.RegisterEventHandler(types.EventGetChunkRecord, p.handleEventGetChunkRecord)
	protocol.RegisterEventHandler(types.EventFetchBlockHeaders, p.handleEventGetHeaders)

	go p.syncRoutine()
	go func() {
		ticker1 := time.NewTicker(time.Minute)
		ticker2 := time.NewTicker(types2.RefreshInterval)
		ticker4 := time.NewTicker(time.Minute * 5)

		for {
			select {
			case <-p.Ctx.Done():
				return
			case <-ticker1.C:
				p.updateChunkWhiteList()
				p.advertiseFullNode()
			case <-ticker2.C:
				p.republish()
			case <-ticker4.C:
				//debug info
				p.localChunkInfoMutex.Lock()
				log.Info("debugLocalChunk", "local chunk hash len", len(p.localChunkInfo))
				p.localChunkInfoMutex.Unlock()
				p.debugFullNode()
				log.Info("debug rt peers", "======== amount", p.RoutingTable.Size())
				log.Info("debug healthy peers", "======== amount", p.HealthyRoutingTable.Size())
				log.Info("debug shard healthy peers", "======== amount", p.ShardHealthyRoutingTable.Size())
				for _, pid := range p.ShardHealthyRoutingTable.ListPeers() {
					log.Info("LatencyEWMA", "pid", pid, "maddr", p.Host.Peerstore().Addrs(pid), "latency", p.Host.Peerstore().LatencyEWMA(pid))
				}
				log.Info("debug length", "notifying msg len", len(p.notifyingQueue))
			}
		}
	}()
}

func (p *Protocol) syncRoutine() {
	for {
		select {
		case <-p.Ctx.Done():
			return
		case info := <-p.notifyingQueue:
			p.syncChunk(info)
			p.notifying.Delete(hex.EncodeToString(info.ChunkHash))
		}
	}
}

func (p *Protocol) syncChunk(info *types.ChunkInfoMsg) {
	//检查本地 p2pStore，如果已存在数据则直接更新
	if err := p.updateChunk(info); err == nil {
		return
	}

	var bodys *types.BlockBodys
	bodys, _ = p.getChunkFromBlockchain(info)
	if bodys == nil {
		//blockchain模块没有数据，从网络中搜索数据
		bodys, _, _ = p.mustFetchChunk(p.Ctx, info, true)
	}
	if bodys == nil {
		log.Error("syncChunk error", "chunkhash", hex.EncodeToString(info.ChunkHash), "start", info.Start)
		return
	}
	if err := p.addChunkBlock(info, bodys); err != nil {
		log.Error("syncChunk", "addChunkBlock error", err)
		return
	}
	log.Info("syncChunk", "chunkhash", hex.EncodeToString(info.ChunkHash), "start", info.Start)
}

func (p *Protocol) updateChunkWhiteList() {
	p.chunkWhiteList.Range(func(k, v interface{}) bool {
		t := v.(time.Time)
		if time.Since(t) > time.Minute*10 {
			p.chunkWhiteList.Delete(k)
		}
		return true
	})
}

func (p *Protocol) advertiseFullNode(opts ...discovery.Option) {
	if !p.SubConfig.IsFullNode {
		return
	}
	_, err := p.Advertise(p.Ctx, fullNode, opts...)
	if err != nil {
		log.Error("advertiseFullNode", "error", err)
	}
}

//TODO
//debug info, to delete soon
func (p *Protocol) debugFullNode() {
	ctx, cancel := context.WithTimeout(p.Ctx, time.Second*3)
	defer cancel()
	peerInfos, err := p.FindPeers(ctx, fullNode)
	if err != nil {
		log.Error("debugFullNode", "FindPeers error", err)
		return
	}
	var count int
	for peerInfo := range peerInfos {
		count++
		log.Info("debugFullNode", "ID", peerInfo.ID, "maddrs", peerInfo.Addrs)
	}
	log.Info("debugFullNode", "total count", count)
}

func (p *Protocol) updateShardHealthyRoutingTableRountine() {
	// HealthyRoutingTable更新时同时更新ShardHealthyRoutingTable
	p.HealthyRoutingTable.PeerRemoved = func(id peer.ID) {
		p.ShardHealthyRoutingTable.Remove(id)
	}
	for p.HealthyRoutingTable.Size() == 0 {
		time.Sleep(time.Second / 2)
	}
	for {
		for _, pid := range p.HealthyRoutingTable.ListPeers() {
			ok, err := p.queryFull(pid)
			if err != nil {
				continue
			}
			if !ok {
				_, _ = p.ShardHealthyRoutingTable.Update(pid)
			}
		}
		time.Sleep(time.Minute * 5)
	}
}

func (p *Protocol) queryFull(pid peer.ID) (bool, error) {
	stream, err := p.Host.NewStream(p.Ctx, pid, fullNode)
	if err != nil {
		return false, err
	}
	defer protocol.CloseStream(stream)
	var resp types.P2PResponse
	err = protocol.ReadStream(&resp, stream)
	if err != nil {
		return false, err
	}
	if reply, ok := resp.Response.(*types.P2PResponse_Reply); ok {
		return reply.Reply.IsOk, nil
	}

	return false, types2.ErrInvalidResponse
}
