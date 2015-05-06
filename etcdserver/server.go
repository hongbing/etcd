// Copyright 2015 CoreOS, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package etcdserver

import (
	"encoding/json"
	"expvar"
	"fmt"
	"log"
	"math/rand"
	"net/http"
	"path"
	"regexp"
	"sync/atomic"
	"time"

	"github.com/coreos/etcd/Godeps/_workspace/src/golang.org/x/net/context"
	"github.com/coreos/etcd/discovery"
	"github.com/coreos/etcd/etcdserver/etcdhttp/httptypes"
	pb "github.com/coreos/etcd/etcdserver/etcdserverpb"
	"github.com/coreos/etcd/etcdserver/stats"
	"github.com/coreos/etcd/pkg/fileutil"
	"github.com/coreos/etcd/pkg/idutil"
	"github.com/coreos/etcd/pkg/pbutil"
	"github.com/coreos/etcd/pkg/runtime"
	"github.com/coreos/etcd/pkg/timeutil"
	"github.com/coreos/etcd/pkg/types"
	"github.com/coreos/etcd/pkg/wait"
	"github.com/coreos/etcd/raft"
	"github.com/coreos/etcd/raft/raftpb"
	"github.com/coreos/etcd/rafthttp"
	"github.com/coreos/etcd/snap"
	"github.com/coreos/etcd/store"
	"github.com/coreos/etcd/version"
	"github.com/coreos/etcd/wal"
)

const (
	// owner can make/remove files inside the directory
	privateDirMode = 0700

	defaultSyncTimeout = time.Second
	DefaultSnapCount   = 10000
	// TODO: calculate based on heartbeat interval
	defaultPublishRetryInterval = 5 * time.Second

	StoreAdminPrefix = "/0"
	StoreKeysPrefix  = "/1"

	purgeFileInterval = 30 * time.Second
)

var (
	storeMembersPrefix        = path.Join(StoreAdminPrefix, "members")
	storeRemovedMembersPrefix = path.Join(StoreAdminPrefix, "removed_members")

	storeMemberAttributeRegexp = regexp.MustCompile(path.Join(storeMembersPrefix, "[[:xdigit:]]{1,16}", attributesSuffix))
)

func init() {
	rand.Seed(time.Now().UnixNano())

	expvar.Publish(
		"file_descriptor_limit",
		expvar.Func(
			func() interface{} {
				n, _ := runtime.FDLimit()
				return n
			},
		),
	)
}

type Response struct {
	Event   *store.Event
	Watcher store.Watcher
	err     error
}

type Server interface {
	// Start performs any initialization of the Server necessary for it to
	// begin serving requests. It must be called before Do or Process.
	// Start must be non-blocking; any long-running server functionality
	// should be implemented in goroutines.
	Start()
	// Stop terminates the Server and performs any necessary finalization.
	// Do and Process cannot be called after Stop has been invoked.
	Stop()
	// ID returns the ID of the Server.
	ID() types.ID
	// Leader returns the ID of the leader Server.
	Leader() types.ID
	// Do takes a request and attempts to fulfill it, returning a Response.
	Do(ctx context.Context, r pb.Request) (Response, error)
	// Process takes a raft message and applies it to the server's raft state
	// machine, respecting any timeout of the given context.
	Process(ctx context.Context, m raftpb.Message) error
	// AddMember attempts to add a member into the cluster. It will return
	// ErrIDRemoved if member ID is removed from the cluster, or return
	// ErrIDExists if member ID exists in the cluster.
	AddMember(ctx context.Context, memb Member) error
	// RemoveMember attempts to remove a member from the cluster. It will
	// return ErrIDRemoved if member ID is removed from the cluster, or return
	// ErrIDNotFound if member ID is not in the cluster.
	RemoveMember(ctx context.Context, id uint64) error

	// UpdateMember attempts to update a existing member in the cluster. It will
	// return ErrIDNotFound if the member ID does not exist.
	UpdateMember(ctx context.Context, updateMemb Member) error
}

// EtcdServer is the production implementation of the Server interface
type EtcdServer struct {
	cfg       *ServerConfig
	snapCount uint64

	r raftNode

	w          wait.Wait
	stop       chan struct{}
	done       chan struct{}
	errorc     chan error
	id         types.ID
	attributes Attributes

	Cluster *Cluster

	store store.Store

	stats  *stats.ServerStats
	lstats *stats.LeaderStats

	SyncTicker <-chan time.Time

	reqIDGen *idutil.Generator
}

// NewServer creates a new EtcdServer from the supplied configuration. The
// configuration is considered static for the lifetime of the EtcdServer.
// 根据serverConfig来创建一个EtcdServer,在Etcd的整个生命周期，配置都是静态的。
// 启动Node
func NewServer(cfg *ServerConfig) (*EtcdServer, error) {
	st := store.New(StoreAdminPrefix, StoreKeysPrefix)
	var w *wal.WAL
	var n raft.Node
	var s *raft.MemoryStorage
	var id types.ID

	// Run the migrations.
	dataVer, err := version.DetectDataDir(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	if err := upgradeDataDir(cfg.DataDir, cfg.Name, dataVer); err != nil {
		return nil, err
	}

	haveWAL := wal.Exist(cfg.WALDir())
	ss := snap.New(cfg.SnapDir())

	switch {
	//已经存在的cluster，且没有WAL
	case !haveWAL && !cfg.NewCluster:
		if err := cfg.VerifyJoinExisting(); err != nil {
			return nil, err
		}
		existingCluster, err := GetClusterFromRemotePeers(getRemotePeerURLs(cfg.Cluster, cfg.Name), cfg.Transport)
		if err != nil {
			return nil, fmt.Errorf("cannot fetch cluster info from peer urls: %v", err)
		}
		if err := ValidateClusterAndAssignIDs(cfg.Cluster, existingCluster); err != nil {
			return nil, fmt.Errorf("error validating peerURLs %s: %v", existingCluster, err)
		}
		cfg.Cluster.UpdateIndex(existingCluster.index)
		cfg.Cluster.SetID(existingCluster.id)
		cfg.Cluster.SetStore(st)
		cfg.Print()
		id, n, s, w = startNode(cfg, nil)
	//新cluster，且没有WAL
	case !haveWAL && cfg.NewCluster:
		if err := cfg.VerifyBootstrap(); err != nil {
			return nil, err
		}
		m := cfg.Cluster.MemberByName(cfg.Name)
		if isMemberBootstrapped(cfg.Cluster, cfg.Name, cfg.Transport) {
			return nil, fmt.Errorf("member %s has already been bootstrapped", m.ID)
		}
		// 对于新的cluster，启动自身的服务发现功能
		if cfg.ShouldDiscover() {
			str, err := discovery.JoinCluster(cfg.DiscoveryURL, cfg.DiscoveryProxy, m.ID, cfg.Cluster.String())
			if err != nil {
				return nil, err
			}
			if cfg.Cluster, err = NewClusterFromString(cfg.Cluster.token, str); err != nil {
				return nil, err
			}
			if err := cfg.Cluster.Validate(); err != nil {
				return nil, fmt.Errorf("bad discovery cluster: %v", err)
			}
		}
		cfg.Cluster.SetStore(st)
		cfg.PrintWithInitial()
		id, n, s, w = startNode(cfg, cfg.Cluster.MemberIDs())
	case haveWAL:
		if err := fileutil.IsDirWriteable(cfg.DataDir); err != nil {
			return nil, fmt.Errorf("cannot write to data directory: %v", err)
		}

		if err := fileutil.IsDirWriteable(cfg.MemberDir()); err != nil {
			return nil, fmt.Errorf("cannot write to member directory: %v", err)
		}

		if cfg.ShouldDiscover() {
			log.Printf("etcdserver: discovery token ignored since a cluster has already been initialized. Valid log found at %q", cfg.WALDir())
		}
		// 加载snapshot信息
		snapshot, err := ss.Load()
		if err != nil && err != snap.ErrNoSnapshot {
			return nil, err
		}
		// 从snapshot恢复store的数据
		if snapshot != nil {
			if err := st.Recovery(snapshot.Data); err != nil {
				log.Panicf("etcdserver: recovered store from snapshot error: %v", err)
			}
			log.Printf("etcdserver: recovered store from snapshot at index %d", snapshot.Metadata.Index)
		}
		cfg.Cluster = NewClusterFromStore(cfg.Cluster.token, st)
		cfg.Print()
		if snapshot != nil {
			log.Printf("etcdserver: loaded cluster information from store: %s", cfg.Cluster)
		}
		if !cfg.ForceNewCluster {
			id, n, s, w = restartNode(cfg, snapshot)
		} else {
			id, n, s, w = restartAsStandaloneNode(cfg, snapshot)
		}
	default:
		return nil, fmt.Errorf("unsupported bootstrap config")
	}

	sstats := &stats.ServerStats{
		Name: cfg.Name,
		ID:   id.String(),
	}
	// 设置自身node id为leaderid
	lstats := stats.NewLeaderStats(id.String())

	srv := &EtcdServer{
		cfg:       cfg,
		snapCount: cfg.SnapCount,
		errorc:    make(chan error, 1),
		store:     st,
		r: raftNode{
			Node:        n,
			ticker:      time.Tick(time.Duration(cfg.TickMs) * time.Millisecond),
			raftStorage: s,
			storage:     NewStorage(w, ss),
		},
		id:         id,
		attributes: Attributes{Name: cfg.Name, ClientURLs: cfg.ClientURLs.StringSlice()},
		Cluster:    cfg.Cluster,
		stats:      sstats,
		lstats:     lstats,
		SyncTicker: time.Tick(500 * time.Millisecond),
		reqIDGen:   idutil.NewGenerator(uint8(id), time.Now()),
	}

	tr := rafthttp.NewTransporter(cfg.Transport, id, cfg.Cluster.ID(), srv, srv.errorc, sstats, lstats)
	srv.r.transport = tr
	srv.Cluster.SetTransport(tr)
	return srv, nil
}

// Start prepares and starts server in a new goroutine. It is no longer safe to
// modify a server's fields after it has been sent to Start.
// It also starts a goroutine to publish its server information.
// 启动Etcdserver，并且publish server的信息,清理文件，监控FD
func (s *EtcdServer) Start() {
	s.start()
	go s.publish(defaultPublishRetryInterval)
	go s.purgeFile()
	go monitorFileDescriptor(s.done)
}

// start prepares and starts server in a new goroutine. It is no longer safe to
// modify a server's fields after it has been sent to Start.
// This function is just used for testing.
func (s *EtcdServer) start() {
	if s.snapCount == 0 {
		log.Printf("etcdserver: set snapshot count to default %d", DefaultSnapCount)
		s.snapCount = DefaultSnapCount
	}
	s.w = wait.New()
	s.done = make(chan struct{})
	s.stop = make(chan struct{})
	s.stats.Initialize()
	// TODO: if this is an empty log, writes all peer infos
	// into the first entry
	go s.run()
}

// 定时清理超过MaxFile的snapshot和wal文件
func (s *EtcdServer) purgeFile() {
	var serrc, werrc <-chan error
	if s.cfg.MaxSnapFiles > 0 {
		serrc = fileutil.PurgeFile(s.cfg.SnapDir(), "snap", s.cfg.MaxSnapFiles, purgeFileInterval, s.done)
	}
	if s.cfg.MaxWALFiles > 0 {
		werrc = fileutil.PurgeFile(s.cfg.WALDir(), "wal", s.cfg.MaxWALFiles, purgeFileInterval, s.done)
	}
	select {
	case e := <-werrc:
		log.Fatalf("etcdserver: failed to purge wal file %v", e)
	case e := <-serrc:
		log.Fatalf("etcdserver: failed to purge snap file %v", e)
	case <-s.done:
		return
	}
}

func (s *EtcdServer) ID() types.ID { return s.id }

func (s *EtcdServer) RaftHandler() http.Handler { return s.r.transport.Handler() }

/**
* EtcdServer实现<link>rafthttp.Raft</link>接口
 */
// 处理外部的request
func (s *EtcdServer) Process(ctx context.Context, m raftpb.Message) error {
	if s.Cluster.IsIDRemoved(types.ID(m.From)) {
		log.Printf("etcdserver: reject message from removed member %s", types.ID(m.From).String())
		return httptypes.NewHTTPError(http.StatusForbidden, "cannot process message from removed member")
	}
	if m.Type == raftpb.MsgApp {
		s.stats.RecvAppendReq(types.ID(m.From).String(), m.Size())
	}
	return s.r.Step(ctx, m)
}

func (s *EtcdServer) ReportUnreachable(id uint64) { s.r.ReportUnreachable(id) }

func (s *EtcdServer) ReportSnapshot(id uint64, status raft.SnapshotStatus) {
	s.r.ReportSnapshot(id, status)
}

// 启动EtcdServer
// 1. 启动raftNode
func (s *EtcdServer) run() {
	snap, err := s.r.raftStorage.Snapshot()
	if err != nil {
		log.Panicf("etcdserver: get snapshot from raft storage error: %v", err)
	}
	confState := snap.Metadata.ConfState
	snapi := snap.Metadata.Index
	appliedi := snapi
	// TODO: get rid of the raft initialization in etcd server
	s.r.s = s
	s.r.applyc = make(chan apply)
	go s.r.run()
	defer func() {
		s.r.stopped <- struct{}{}
		<-s.r.done
		close(s.done)
	}()

	var shouldstop bool
	for {
		select {
		// apply包含需要apply的entry和snapshot
		case apply := <-s.r.apply():
			// apply snapshot
			if !raft.IsEmptySnap(apply.snapshot) {
				if apply.snapshot.Metadata.Index <= appliedi {
					log.Panicf("etcdserver: snapshot index [%d] should > appliedi[%d] + 1",
						apply.snapshot.Metadata.Index, appliedi)
				}

				if err := s.store.Recovery(apply.snapshot.Data); err != nil {
					log.Panicf("recovery store error: %v", err)
				}

				// Avoid snapshot recovery overwriting newer cluster and
				// transport setting, which may block the communication.
				if s.Cluster.index < apply.snapshot.Metadata.Index {
					s.Cluster.Recover()
				}

				appliedi = apply.snapshot.Metadata.Index
				snapi = appliedi
				confState = apply.snapshot.Metadata.ConfState
				log.Printf("etcdserver: recovered from incoming snapshot at index %d", snapi)
			}

			// apply entries
			if len(apply.entries) != 0 {
				firsti := apply.entries[0].Index
				if firsti > appliedi+1 {
					log.Panicf("etcdserver: first index of committed entry[%d] should <= appliedi[%d] + 1", firsti, appliedi)
				}
				var ents []raftpb.Entry
				if appliedi+1-firsti < uint64(len(apply.entries)) {
					ents = apply.entries[appliedi+1-firsti:]
				}
				// 将apply的entry存储到store里
				if appliedi, shouldstop = s.apply(ents, &confState); shouldstop {
					go s.stopWithDelay(10*100*time.Millisecond, fmt.Errorf("the member has been permanently removed from the cluster"))
				}
			}

			// wait for the raft routine to finish the disk writes before triggering a
			// snapshot. or applied index might be greater than the last index in raft
			// storage, since the raft routine might be slower than apply routine.
			apply.done <- struct{}{}

			// trigger snapshot
			if appliedi-snapi > s.snapCount {
				log.Printf("etcdserver: start to snapshot (applied: %d, lastsnap: %d)", appliedi, snapi)
				s.snapshot(appliedi, confState)
				snapi = appliedi
			}
		case err := <-s.errorc:
			log.Printf("etcdserver: %s", err)
			log.Printf("etcdserver: the data-dir used by this member must be removed.")
			return
		case <-s.stop:
			return
		}
	}
}

// Stop stops the server gracefully, and shuts down the running goroutine.
// Stop should be called after a Start(s), otherwise it will block forever.
func (s *EtcdServer) Stop() {
	select {
	case s.stop <- struct{}{}:
	case <-s.done:
		return
	}
	<-s.done
}

func (s *EtcdServer) stopWithDelay(d time.Duration, err error) {
	time.Sleep(d)
	select {
	case s.errorc <- err:
	default:
	}
}

// StopNotify returns a channel that receives a empty struct
// when the server is stopped.
func (s *EtcdServer) StopNotify() <-chan struct{} { return s.done }

// Do interprets r and performs an operation on s.store according to r.Method
// and other fields. If r.Method is "POST", "PUT", "DELETE", or a "GET" with
// Quorum == true, r will be sent through consensus before performing its
// respective operation. Do will block until an action is performed or there is
// an error.
// 执行client-->server的request,如果Method是POST，PUT，DELETE，Quorum的GET，
// 那么在执行操作之前会进行一致性处理,每个request都会生成一个resq id
func (s *EtcdServer) Do(ctx context.Context, r pb.Request) (Response, error) {
	r.ID = s.reqIDGen.Next()
	if r.Method == "GET" && r.Quorum {
		r.Method = "QGET"
	}
	switch r.Method {
	/**
	例如：curl -L http://127.0.0.1:2379/v2/keys/mykey -XPUT -d value="this is awesome"
	处理client的KV数据请求，需要经过一致性处理
	*/
	case "POST", "PUT", "DELETE", "QGET":
		data, err := r.Marshal()
		if err != nil {
			return Response{}, err
		}
		// 注册该reqId的channel，等待Trigger方法向该channel中写数据
		ch := s.w.Register(r.ID)

		// TODO: benchmark the cost of time.Now()
		// might be sampling?
		start := time.Now()
		s.r.Propose(ctx, data)
		// propose挂起数加1
		proposePending.Inc()
		defer proposePending.Dec()

		select {
		case x := <-ch:
			proposeDurations.Observe(float64(time.Since(start).Nanoseconds() / int64(time.Millisecond)))
			resp := x.(Response)
			return resp, resp.err
		case <-ctx.Done():
			proposeFailed.Inc()
			s.w.Trigger(r.ID, nil) // GC wait
			return Response{}, parseCtxErr(ctx.Err())
		case <-s.done:
			return Response{}, ErrStopped
		}
	case "GET":
		switch {
		case r.Wait:
			wc, err := s.store.Watch(r.Path, r.Recursive, r.Stream, r.Since)
			if err != nil {
				return Response{}, err
			}
			return Response{Watcher: wc}, nil
		default:
			ev, err := s.store.Get(r.Path, r.Recursive, r.Sorted)
			if err != nil {
				return Response{}, err
			}
			return Response{Event: ev}, nil
		}
	case "HEAD":
		ev, err := s.store.Get(r.Path, r.Recursive, r.Sorted)
		if err != nil {
			return Response{}, err
		}
		return Response{Event: ev}, nil
	default:
		return Response{}, ErrUnknownMethod
	}
}

func (s *EtcdServer) SelfStats() []byte { return s.stats.JSON() }

func (s *EtcdServer) LeaderStats() []byte {
	lead := atomic.LoadUint64(&s.r.lead)
	if lead != uint64(s.id) {
		return nil
	}
	return s.lstats.JSON()
}

func (s *EtcdServer) StoreStats() []byte { return s.store.JsonStats() }

func (s *EtcdServer) AddMember(ctx context.Context, memb Member) error {
	// TODO: move Member to protobuf type
	b, err := json.Marshal(memb)
	if err != nil {
		return err
	}
	cc := raftpb.ConfChange{
		Type:    raftpb.ConfChangeAddNode,
		NodeID:  uint64(memb.ID),
		Context: b,
	}
	return s.configure(ctx, cc)
}

func (s *EtcdServer) RemoveMember(ctx context.Context, id uint64) error {
	cc := raftpb.ConfChange{
		Type:   raftpb.ConfChangeRemoveNode,
		NodeID: id,
	}
	return s.configure(ctx, cc)
}

func (s *EtcdServer) UpdateMember(ctx context.Context, memb Member) error {
	b, err := json.Marshal(memb)
	if err != nil {
		return err
	}
	cc := raftpb.ConfChange{
		Type:    raftpb.ConfChangeUpdateNode,
		NodeID:  uint64(memb.ID),
		Context: b,
	}
	return s.configure(ctx, cc)
}

// Implement the RaftTimer interface
func (s *EtcdServer) Index() uint64 { return atomic.LoadUint64(&s.r.index) }

func (s *EtcdServer) Term() uint64 { return atomic.LoadUint64(&s.r.term) }

// Only for testing purpose
// TODO: add Raft server interface to expose raft related info:
// Index, Term, Lead, Committed, Applied, LastIndex, etc.
func (s *EtcdServer) Lead() uint64 { return atomic.LoadUint64(&s.r.lead) }

func (s *EtcdServer) Leader() types.ID { return types.ID(s.Lead()) }

// configure sends a configuration change through consensus and
// then waits for it to be applied to the server. It
// will block until the change is performed or there is an error.
func (s *EtcdServer) configure(ctx context.Context, cc raftpb.ConfChange) error {
	cc.ID = s.reqIDGen.Next()
	ch := s.w.Register(cc.ID)
	if err := s.r.ProposeConfChange(ctx, cc); err != nil {
		s.w.Trigger(cc.ID, nil)
		return err
	}
	select {
	case x := <-ch:
		if err, ok := x.(error); ok {
			return err
		}
		if x != nil {
			log.Panicf("return type should always be error")
		}
		return nil
	case <-ctx.Done():
		s.w.Trigger(cc.ID, nil) // GC wait
		return parseCtxErr(ctx.Err())
	case <-s.done:
		return ErrStopped
	}
}

// sync proposes a SYNC request and is non-blocking.
// This makes no guarantee that the request will be proposed or performed.
// The request will be cancelled after the given timeout.
func (s *EtcdServer) sync(timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	req := pb.Request{
		Method: "SYNC",
		ID:     s.reqIDGen.Next(),
		Time:   time.Now().UnixNano(),
	}
	data := pbutil.MustMarshal(&req)
	// There is no promise that node has leader when do SYNC request,
	// so it uses goroutine to propose.
	go func() {
		s.r.Propose(ctx, data)
		cancel()
	}()
}

// publish registers server information into the cluster. The information
// is the JSON representation of this server's member struct, updated with the
// static clientURLs of the server.
// The function keeps attempting to register until it succeeds,
// or its server is stopped.
// 注册server的clientUrls信息到cluster中，更新server的client urls
func (s *EtcdServer) publish(retryInterval time.Duration) {
	b, err := json.Marshal(s.attributes)
	if err != nil {
		log.Printf("etcdserver: json marshal error: %v", err)
		return
	}
	req := pb.Request{
		Method: "PUT",
		Path:   MemberAttributesStorePath(s.id),
		Val:    string(b),
	}

	for {
		ctx, cancel := context.WithTimeout(context.Background(), retryInterval)
		_, err := s.Do(ctx, req)
		cancel()
		switch err {
		case nil:
			log.Printf("etcdserver: published %+v to cluster %s", s.attributes, s.Cluster.ID())
			return
		case ErrStopped:
			log.Printf("etcdserver: aborting publish because server is stopped")
			return
		default:
			log.Printf("etcdserver: publish error: %v", err)
		}
	}
}

// 发送message,已经移除的node不发送消息。
func (s *EtcdServer) send(ms []raftpb.Message) {
	for i, _ := range ms {
		if s.Cluster.IsIDRemoved(types.ID(ms[i].To)) {
			ms[i].To = 0
		}
	}
	s.r.transport.Send(ms)
}

// apply takes entries received from Raft (after it has been committed) and
// applies them to the current state of the EtcdServer.
// The given entries should not be empty.
func (s *EtcdServer) apply(es []raftpb.Entry, confState *raftpb.ConfState) (uint64, bool) {
	var applied uint64
	var shouldstop bool
	var err error
	for i := range es {
		e := es[i]
		switch e.Type {
		case raftpb.EntryNormal:
			var r pb.Request
			pbutil.MustUnmarshal(&r, e.Data)
			s.w.Trigger(r.ID, s.applyRequest(r))
		case raftpb.EntryConfChange:
			var cc raftpb.ConfChange
			pbutil.MustUnmarshal(&cc, e.Data)
			shouldstop, err = s.applyConfChange(cc, confState, e.Index)
			s.w.Trigger(cc.ID, err)
		default:
			log.Panicf("entry type should be either EntryNormal or EntryConfChange")
		}
		atomic.StoreUint64(&s.r.index, e.Index)
		atomic.StoreUint64(&s.r.term, e.Term)
		applied = e.Index
	}
	return applied, shouldstop
}

// applyRequest interprets r as a call to store.X and returns a Response interpreted
// from store.Event
func (s *EtcdServer) applyRequest(r pb.Request) Response {
	f := func(ev *store.Event, err error) Response {
		return Response{Event: ev, err: err}
	}
	expr := timeutil.UnixNanoToTime(r.Expiration)
	switch r.Method {
	case "POST":
		return f(s.store.Create(r.Path, r.Dir, r.Val, true, expr))
	case "PUT":
		exists, existsSet := pbutil.GetBool(r.PrevExist)
		switch {
		case existsSet:
			if exists {
				if r.PrevIndex == 0 && r.PrevValue == "" {
					return f(s.store.Update(r.Path, r.Val, expr))
				} else {
					return f(s.store.CompareAndSwap(r.Path, r.PrevValue, r.PrevIndex, r.Val, expr))
				}
			}
			return f(s.store.Create(r.Path, r.Dir, r.Val, false, expr))
		case r.PrevIndex > 0 || r.PrevValue != "":
			return f(s.store.CompareAndSwap(r.Path, r.PrevValue, r.PrevIndex, r.Val, expr))
		default:
			if storeMemberAttributeRegexp.MatchString(r.Path) {
				id := mustParseMemberIDFromKey(path.Dir(r.Path))
				var attr Attributes
				if err := json.Unmarshal([]byte(r.Val), &attr); err != nil {
					log.Panicf("unmarshal %s should never fail: %v", r.Val, err)
				}
				s.Cluster.UpdateAttributes(id, attr)
			}
			return f(s.store.Set(r.Path, r.Dir, r.Val, expr))
		}
	case "DELETE":
		switch {
		case r.PrevIndex > 0 || r.PrevValue != "":
			return f(s.store.CompareAndDelete(r.Path, r.PrevValue, r.PrevIndex))
		default:
			return f(s.store.Delete(r.Path, r.Dir, r.Recursive))
		}
	case "QGET":
		return f(s.store.Get(r.Path, r.Recursive, r.Sorted))
	case "SYNC":
		s.store.DeleteExpiredKeys(time.Unix(0, r.Time))
		return Response{}
	default:
		// This should never be reached, but just in case:
		return Response{err: ErrUnknownMethod}
	}
}

// applyConfChange applies a ConfChange to the server at the given index. It is only
// invoked with a ConfChange that has already passed through Raft.
func (s *EtcdServer) applyConfChange(cc raftpb.ConfChange, confState *raftpb.ConfState, index uint64) (bool, error) {
	if err := s.Cluster.ValidateConfigurationChange(cc); err != nil {
		cc.NodeID = raft.None
		s.r.ApplyConfChange(cc)
		return false, err
	}
	*confState = *s.r.ApplyConfChange(cc)
	switch cc.Type {
	case raftpb.ConfChangeAddNode:
		m := new(Member)
		if err := json.Unmarshal(cc.Context, m); err != nil {
			log.Panicf("unmarshal member should never fail: %v", err)
		}
		if cc.NodeID != uint64(m.ID) {
			log.Panicf("nodeID should always be equal to member ID")
		}
		s.Cluster.AddMember(m, index)
		if m.ID == s.id {
			log.Printf("etcdserver: added local member %s %v to cluster %s", m.ID, m.PeerURLs, s.Cluster.ID())
		} else {
			log.Printf("etcdserver: added member %s %v to cluster %s", m.ID, m.PeerURLs, s.Cluster.ID())
		}
	case raftpb.ConfChangeRemoveNode:
		id := types.ID(cc.NodeID)
		s.Cluster.RemoveMember(id, index)
		if id == s.id {
			return true, nil
		} else {
			log.Printf("etcdserver: removed member %s from cluster %s", id, s.Cluster.ID())
		}
	case raftpb.ConfChangeUpdateNode:
		m := new(Member)
		if err := json.Unmarshal(cc.Context, m); err != nil {
			log.Panicf("unmarshal member should never fail: %v", err)
		}
		if cc.NodeID != uint64(m.ID) {
			log.Panicf("nodeID should always be equal to member ID")
		}
		s.Cluster.UpdateRaftAttributes(m.ID, m.RaftAttributes, index)
		if m.ID == s.id {
			log.Printf("etcdserver: update local member %s %v in cluster %s", m.ID, m.PeerURLs, s.Cluster.ID())
		} else {
			log.Printf("etcdserver: update member %s %v in cluster %s", m.ID, m.PeerURLs, s.Cluster.ID())
		}
	}
	return false, nil
}

// TODO: non-blocking snapshot
// 创建snapshot并保存
func (s *EtcdServer) snapshot(snapi uint64, confState raftpb.ConfState) {
	clone := s.store.Clone()

	go func() {
		d, err := clone.SaveNoCopy()
		// TODO: current store will never fail to do a snapshot
		// what should we do if the store might fail?
		if err != nil {
			log.Panicf("etcdserver: store save should never fail: %v", err)
		}
		snap, err := s.r.raftStorage.CreateSnapshot(snapi, &confState, d)
		if err != nil {
			// the snapshot was done asynchronously with the progress of raft.
			// raft might have already got a newer snapshot.
			if err == raft.ErrSnapOutOfDate {
				return
			}
			log.Panicf("etcdserver: unexpected create snapshot error %v", err)
		}
		if err := s.r.storage.SaveSnap(snap); err != nil {
			log.Fatalf("etcdserver: save snapshot error: %v", err)
		}
		log.Printf("etcdserver: saved snapshot at index %d", snap.Metadata.Index)

		// keep some in memory log entries for slow followers.
		compacti := uint64(1)
		if snapi > numberOfCatchUpEntries {
			compacti = snapi - numberOfCatchUpEntries
		}
		err = s.r.raftStorage.Compact(compacti)
		if err != nil {
			// the compaction was done asynchronously with the progress of raft.
			// raft log might already been compact.
			if err == raft.ErrCompacted {
				return
			}
			log.Panicf("etcdserver: unexpected compaction error %v", err)
		}
		log.Printf("etcdserver: compacted raft log at %d", compacti)
	}()
}

func (s *EtcdServer) PauseSending() { s.r.pauseSending() }

func (s *EtcdServer) ResumeSending() { s.r.resumeSending() }
