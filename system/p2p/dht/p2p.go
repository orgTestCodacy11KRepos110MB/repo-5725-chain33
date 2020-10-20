// Copyright Fuzamei Corp. 2018 All Rights Reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package dht 基于libp2p实现p2p 插件
package dht

import (
	"context"
	"encoding/hex"
	"fmt"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	circuit "github.com/libp2p/go-libp2p-circuit"

	pubsub "github.com/libp2p/go-libp2p-pubsub"

	"github.com/33cn/chain33/client"
	logger "github.com/33cn/chain33/common/log/log15"
	"github.com/33cn/chain33/p2p"
	"github.com/33cn/chain33/queue"
	"github.com/33cn/chain33/system/p2p/dht/manage"
	"github.com/33cn/chain33/system/p2p/dht/net"
	"github.com/33cn/chain33/system/p2p/dht/protocol"
	prototypes "github.com/33cn/chain33/system/p2p/dht/protocol/types"
	"github.com/33cn/chain33/system/p2p/dht/store"
	p2pty "github.com/33cn/chain33/system/p2p/dht/types"
	"github.com/33cn/chain33/types"
	ds "github.com/ipfs/go-datastore"
	"github.com/libp2p/go-libp2p"
	connmgr "github.com/libp2p/go-libp2p-connmgr"
	core "github.com/libp2p/go-libp2p-core"
	p2pcrypto "github.com/libp2p/go-libp2p-core/crypto"
	"github.com/libp2p/go-libp2p-core/metrics"
	"github.com/multiformats/go-multiaddr"
)

var log = logger.New("module", p2pty.DHTTypeName)

func init() {
	p2p.RegisterP2PCreate(p2pty.DHTTypeName, New)
}

// P2P p2p struct
type P2P struct {
	chainCfg      *types.Chain33Config
	host          core.Host
	discovery     *net.Discovery
	connManag     *manage.ConnManager
	peerInfoManag *manage.PeerInfoManager
	api           client.QueueProtocolAPI
	client        queue.Client
	addrbook      *AddrBook
	taskGroup     *sync.WaitGroup

	pubsub     *net.PubSub
	restart    int32
	p2pCfg     *types.P2P
	subCfg     *p2pty.P2PSubConfig
	mgr        *p2p.Manager
	subChan    chan interface{}
	ctx        context.Context
	cancel     context.CancelFunc
	blackCache *manage.TimeCache
	db         ds.Datastore
	//env *protocol.P2PEnv
	env *prototypes.P2PEnv
}

// New new dht p2p network
func New(mgr *p2p.Manager, subCfg []byte) p2p.IP2P {

	chainCfg := mgr.ChainCfg
	p2pCfg := chainCfg.GetModuleConfig().P2P
	mcfg := &p2pty.P2PSubConfig{}
	types.MustDecode(subCfg, mcfg)
	if mcfg.Port == 0 {
		mcfg.Port = p2pty.DefaultP2PPort
	}
	client := mgr.Client
	p := &P2P{
		client:   client,
		chainCfg: chainCfg,
		subCfg:   mcfg,
		p2pCfg:   p2pCfg,
		mgr:      mgr,
		api:      mgr.SysAPI,
		addrbook: NewAddrBook(p2pCfg),
		subChan:  mgr.PubSub.Sub(p2pty.DHTTypeName),
	}

	return initP2P(p)

}

func initP2P(p *P2P) *P2P {
	//other init work
	p.ctx, p.cancel = context.WithCancel(context.Background())
	priv := p.addrbook.GetPrivkey()
	if priv == nil { //addrbook存储的peer key 为空
		if p.p2pCfg.WaitPid { //p2p阻塞,直到创建钱包之后
			p.genAirDropKey()
		} else { //创建随机公私钥对,生成临时pid，待创建钱包之后，提换钱包生成的pid
			p.addrbook.Randkey()
			go p.genAirDropKey() //非阻塞模式
		}

	} else { //非阻塞模式
		go p.genAirDropKey()
	}

	p.blackCache = manage.NewTimeCache(p.ctx, time.Minute*5)
	maddr, err := multiaddr.NewMultiaddr(fmt.Sprintf("/ip4/0.0.0.0/tcp/%d", p.subCfg.Port))
	if err != nil {
		panic(err)
	}
	log.Info("NewMulti", "addr", maddr.String())

	bandwidthTracker := metrics.NewBandwidthCounter()
	options := p.buildHostOptions(p.addrbook.GetPrivkey(), bandwidthTracker, maddr)
	host, err := libp2p.New(p.ctx, options...)
	if err != nil {
		panic(err)
	}

	p.host = host
	psOpts := make([]pubsub.Option, 0)
	// pubsub消息默认会基于节点私钥进行签名和验签，支持关闭
	if p.subCfg.DisablePubSubMsgSign {
		psOpts = append(psOpts, pubsub.WithMessageSigning(false), pubsub.WithStrictSignatureVerification(false))
	}
	ps, err := net.NewPubSub(p.ctx, p.host, psOpts...)
	if err != nil {
		return nil
	}
	p.pubsub = ps
	p.discovery = net.InitDhtDiscovery(p.ctx, p.host, p.addrbook.AddrsInfo(), p.chainCfg, p.subCfg)
	p.connManag = manage.NewConnManager(p.host, p.discovery, bandwidthTracker, p.subCfg)

	p.peerInfoManag = manage.NewPeerInfoManager(p.host, p.client, p.blackCache, p.pruePeers)
	p.taskGroup = &sync.WaitGroup{}
	p.db = store.NewDataStore(p.subCfg)

	return p
}

// StartP2P start p2p
func (p *P2P) StartP2P() {
	if atomic.LoadInt32(&p.restart) == 1 {
		log.Info("RestartP2P...")
		initP2P(p) //重新创建host
	}
	atomic.StoreInt32(&p.restart, 0)
	p.addrbook.StoreHostID(p.host.ID(), p.p2pCfg.DbPath)
	log.Info("NewP2p", "peerId", p.host.ID(), "addrs", p.host.Addrs())
	p.discovery.Start()

	env := &prototypes.P2PEnv{
		ChainCfg:         p.chainCfg,
		QueueClient:      p.client,
		Host:             p.host,
		ConnManager:      p.connManag,
		PeerInfoManager:  p.peerInfoManag,
		P2PManager:       p.mgr,
		SubConfig:        p.subCfg,
		Discovery:        p.discovery,
		Pubsub:           p.pubsub,
		Ctx:              p.ctx,
		DB:               p.db,
		RoutingTable:     p.discovery.RoutingTable(),
		RoutingDiscovery: p.discovery.RoutingDiscovery,
	}
	p.env = env
	protocol.Init(env)
	//debug new
	env2 := &protocol.P2PEnv{
		Ctx:              p.ctx,
		ChainCfg:         p.chainCfg,
		QueueClient:      p.client,
		Host:             p.host,
		P2PManager:       p.mgr,
		SubConfig:        p.subCfg,
		DB:               p.db,
		RoutingDiscovery: p.discovery.RoutingDiscovery,
		RoutingTable:     p.discovery.RoutingTable(),
	}
	protocol.InitAllProtocol(env2)
	go p.peerInfoManag.Start()
	go p.managePeers()
	go p.handleP2PEvent()
	go p.findLANPeers()
}

// CloseP2P close p2p
func (p *P2P) CloseP2P() {
	log.Info("p2p closing")

	p.connManag.Close()
	p.peerInfoManag.Close()
	p.discovery.Close()
	p.waitTaskDone()
	p.db.Close()

	protocol.ClearEventHandler()
	prototypes.ClearEventHandler()
	if !p.isRestart() {
		p.mgr.PubSub.Unsub(p.subChan)

	}
	p.host.Close()
	p.cancel()
	log.Info("p2p closed")

}

func (p *P2P) reStart() {
	atomic.StoreInt32(&p.restart, 1)
	log.Info("reStart p2p")
	if p.host == nil {
		//说明p2p还没有开始启动，无需重启
		log.Info("p2p no need restart...")
		atomic.StoreInt32(&p.restart, 0)
		return
	}
	p.CloseP2P()
	p.StartP2P()

}
func (p *P2P) buildHostOptions(priv p2pcrypto.PrivKey, bandwidthTracker metrics.Reporter, maddr multiaddr.Multiaddr) []libp2p.Option {
	if bandwidthTracker == nil {
		bandwidthTracker = metrics.NewBandwidthCounter()
	}

	var options []libp2p.Option
	if p.subCfg.RelayEnable {
		if p.subCfg.RelayHop { //启用中继服务端
			options = append(options, libp2p.EnableRelay(circuit.OptHop))
		} else { //用配置的节点作为中继节点,需要打开HOP选项
			//relays := append(p.subCfg.BootStraps, p.subCfg.RelayNodeAddr...)
			relays := p.subCfg.RelayNodeAddr
			options = append(options, libp2p.AddrsFactory(net.WithRelayAddrs(relays)))
			options = append(options, libp2p.EnableRelay())
		}
	}

	options = append(options, libp2p.NATPortMap())
	if maddr != nil {
		options = append(options, libp2p.ListenAddrs(maddr))
	}
	if priv != nil {
		options = append(options, libp2p.Identity(priv))
	}

	options = append(options, libp2p.BandwidthReporter(bandwidthTracker))

	if p.subCfg.MaxConnectNum > 0 { //如果不设置最大连接数量，默认允许dht自由连接并填充路由表

		var maxconnect = int(p.subCfg.MaxConnectNum)
		//5分钟的宽限期,定期清理
		options = append(options, libp2p.ConnectionManager(connmgr.NewConnManager(maxconnect, maxconnect+int(manage.CacheLimit), time.Minute*5)))
		//ConnectionGater,处理网络连接的策略
		options = append(options, libp2p.ConnectionGater(manage.NewConnGater(&p.host, p.subCfg, p.blackCache)))
	}
	//关闭ping
	options = append(options, libp2p.Ping(false))
	return options
}

func (p *P2P) managePeers() {
	go p.connManag.MonitorAllPeers(p.subCfg.Seeds, p.host)

	for {
		log.Debug("managePeers", "table size", p.discovery.RoutingTableSize())
		select {
		case <-p.ctx.Done():
			log.Info("managePeers", "p2p", "closed")
			return
		case <-time.After(time.Minute * 10):
			//Reflesh addrbook
			peersInfo := p.discovery.FindLocalPeers(p.connManag.FetchNearestPeers())
			if len(peersInfo) != 0 {
				p.addrbook.SaveAddr(peersInfo)
			}

		}

	}
}

//查询本局域网内是否有节点
func (p *P2P) findLANPeers() {
	if p.subCfg.DisableFindLANPeers {
		return
	}
	peerChan, err := p.discovery.FindLANPeers(p.host, fmt.Sprintf("/%s-mdns/%d", p.chainCfg.GetTitle(), p.subCfg.Channel))
	if err != nil {
		log.Error("findLANPeers", "err", err.Error())
		return
	}

	for {
		select {
		case neighbors := <-peerChan:
			log.Debug("^_^! Well,findLANPeers Let's Play ^_^!<<<<<<<<<<<<<<<<<<<", "peerName", neighbors.ID, "addrs:", neighbors.Addrs, "paddr", p.host.Peerstore().Addrs(neighbors.ID))
			//发现局域网内的邻居节点
			err := p.host.Connect(context.Background(), neighbors)
			if err != nil {
				log.Error("findLANPeers", "err", err.Error())
				continue
			}
			log.Info("findLANPeers", "connect neighbors success", neighbors.ID.Pretty())
			p.connManag.AddNeighbors(&neighbors)

		case <-p.ctx.Done():
			log.Info("findLANPeers", "process", "done")
			return
		}
	}

}

func (p *P2P) handleP2PEvent() {

	//TODO, control goroutine num
	for {
		select {
		case <-p.ctx.Done():
			return
		case data, ok := <-p.subChan:
			if !ok {
				return
			}
			msg, ok := data.(*queue.Message)
			if !ok || data == nil {
				log.Error("handleP2PEvent", "recv invalid msg, data=", data)
				continue
			}

			p.taskGroup.Add(1)
			go func(qmsg *queue.Message) {
				defer p.taskGroup.Done()
				//log.Debug("handleP2PEvent", "recv msg ty", qmsg.Ty)
				protocol.HandleEvent(qmsg)

			}(msg)
		}

	}

}

func (p *P2P) isRestart() bool {
	return atomic.LoadInt32(&p.restart) == 1
}

func (p *P2P) waitTaskDone() {

	waitDone := make(chan struct{})
	go func() {
		defer close(waitDone)
		p.taskGroup.Wait()
	}()
	select {
	case <-waitDone:
	case <-time.After(time.Second * 20):
		log.Error("waitTaskDone", "err", "20s timeout")
	}
}

//创建空投地址
func (p *P2P) genAirDropKey() {

	for { //等待钱包创建，解锁
		select {
		case <-p.ctx.Done():
			log.Info("genAirDropKey", "p2p closed")
			return
		case <-time.After(time.Second):
			resp, err := p.api.ExecWalletFunc("wallet", "GetWalletStatus", &types.ReqNil{})
			if err != nil {
				time.Sleep(time.Second)
				continue
			}
			if !resp.(*types.WalletStatus).GetIsHasSeed() {
				continue
			}

			if resp.(*types.WalletStatus).GetIsWalletLock() { //上锁状态，无法用助记词创建空投地址,等待...
				continue
			}

		}
		break
	}

	//用助记词和随机索引创建空投地址
	r := rand.New(rand.NewSource(types.Now().Unix()))
	var minIndex int32 = 100000000
	randIndex := minIndex + r.Int31n(1000000)
	reqIndex := &types.Int32{Data: randIndex}
	msg, err := p.api.ExecWalletFunc("wallet", "NewAccountByIndex", reqIndex)
	if err != nil {
		log.Error("genAirDropKey", "NewAccountByIndex err", err)
		return
	}

	var walletPrivkey string
	if reply, ok := msg.(*types.ReplyString); !ok {
		log.Error("genAirDropKey", "wrong format data", "")
		panic(err)

	} else {
		walletPrivkey = reply.GetData()
	}
	if walletPrivkey != "" && walletPrivkey[:2] == "0x" {
		walletPrivkey = walletPrivkey[2:]
	}

	walletPubkey, err := GenPubkey(walletPrivkey)
	if err != nil {
		return
	}
	//如果addrbook之前保存的savePrivkey相同，则意味着节点启动之前已经创建了airdrop 空投地址
	savePrivkey, _ := p.addrbook.GetPrivPubKey()
	if savePrivkey == walletPrivkey { //addrbook与wallet保存了相同的空投私钥，不需要继续导入
		log.Debug("genAirDropKey", " same privekey ,process done")
		return
	}

	if len(savePrivkey) != 2*privKeyCompressBytesLen { //非压缩私钥,兼容之前老版本的DHT非压缩私钥
		log.Debug("len savePrivkey", len(savePrivkey))
		unCompkey := p.addrbook.GetPrivkey()
		if unCompkey == nil {
			savePrivkey = ""
		} else {
			compkey, err := unCompkey.Raw() //compress key
			if err != nil {
				savePrivkey = ""
				log.Error("genAirDropKey", "compressKey.Raw err", err)
			} else {
				savePrivkey = hex.EncodeToString(compkey)
			}
		}

	}

	if savePrivkey != "" && !p.p2pCfg.WaitPid { //如果是waitpid 则不生成dht node award,保证之后一个空投地址，即钱包创建的airdropaddr
		//savePrivkey是随机私钥，兼容老版本，先对其进行导入钱包处理
		//进行压缩处理
		var parm types.ReqWalletImportPrivkey
		parm.Privkey = savePrivkey
		parm.Label = "dht node award"

		for {
			_, err = p.api.ExecWalletFunc("wallet", "WalletImportPrivkey", &parm)
			if err == types.ErrLabelHasUsed {
				//切换随机lable
				parm.Label = fmt.Sprintf("node award %d", rand.Int31n(1024000))
				time.Sleep(time.Second)
				continue
			}
			break
		}
	}

	p.addrbook.saveKey(walletPrivkey, walletPubkey)
	p.reStart()

}

func (p *P2P) pruePeers(pid core.PeerID, beBlack bool) {

	p.connManag.Delete(pid)
	if beBlack {
		p.blackCache.Add(pid.Pretty(), 0)
	}

}
