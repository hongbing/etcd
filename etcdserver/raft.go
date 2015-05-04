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
	"log"
	"os"
	"sort"
	"sync/atomic"
	"time"

	pb "github.com/coreos/etcd/etcdserver/etcdserverpb"
	"github.com/coreos/etcd/pkg/pbutil"
	"github.com/coreos/etcd/pkg/types"
	"github.com/coreos/etcd/raft"
	"github.com/coreos/etcd/raft/raftpb"
	"github.com/coreos/etcd/rafthttp"
	"github.com/coreos/etcd/wal"
	"github.com/coreos/etcd/wal/walpb"
)

const (
	// Number of entries for slow follower to catch-up after compacting
	// the raft storage entries.
	// We expect the follower has a millisecond level latency with the leader.
	// The max throughput is around 10K. Keep a 5K entries is enough for helping
	// follower to catch up.
	numberOfCatchUpEntries = 5000

	// The max throughput of etcd will not exceed 100MB/s (100K * 1KB value).
	// Assuming the RTT is around 10ms, 1MB max size is large enough.
	maxSizePerMsg = 1 * 1024 * 1024
	// Never overflow the rafthttp buffer, which is 4096.
	// TODO: a better const?
	maxInflightMsgs = 4096 / 8
)

var (
	// indirection for expvar func interface
	// expvar panics when publishing duplicate name
	// expvar does not support remove a registered name
	// so only register a func that calls raftStatus
	// and change raftStatus as we need.
	raftStatus func() raft.Status
)

func init() {
	expvar.Publish("raft.status", expvar.Func(func() interface{} { return raftStatus() }))
}

type RaftTimer interface {
	Index() uint64
	Term() uint64
}

// apply contains entries, snapshot be applied.
// After applied all the items, the application needs
// to send notification to done chan.
// 包含需要apply的entries和snap
type apply struct {
	entries  []raftpb.Entry
	snapshot raftpb.Snapshot
	done     chan struct{}
}

// 对raft实例的封装
type raftNode struct {
	raft.Node

	// a chan to send out apply
	applyc chan apply

	// TODO: remove the etcdserver related logic from raftNode
	// TODO: add a state machine interface to apply the commit entries
	// and do snapshot/recover
	s *EtcdServer

	// utility
	ticker      <-chan time.Time
	raftStorage *raft.MemoryStorage
	storage     Storage
	// transport specifies the transport to send and receive msgs to members.
	// Sending messages MUST NOT block. It is okay to drop messages, since
	// clients should timeout and reissue their messages.
	// If transport is nil, server will panic.
	transport rafthttp.Transporter

	// Cache of the latest raft index and raft term the server has seen
	// raft最近的index的缓存
	index uint64
	term  uint64
	lead  uint64

	stopped chan struct{}
	done    chan struct{}
}

// 处理raft实例的状态：ready,ticker,sync
func (r *raftNode) run() {
	r.stopped = make(chan struct{})
	r.done = make(chan struct{})

	var syncC <-chan time.Time

	defer r.stop()
	for {
		select {
		case <-r.ticker:
			r.Tick()
		case rd := <-r.Ready():
			if rd.SoftState != nil {
				atomic.StoreUint64(&r.lead, rd.SoftState.Lead)
				if rd.RaftState == raft.StateLeader {
					syncC = r.s.SyncTicker
					// TODO: remove the nil checking
					// current test utility does not provide the stats
					if r.s.stats != nil {
						r.s.stats.BecomeLeader()
					}
				} else {
					syncC = nil
				}
			}

			apply := apply{
				entries:  rd.CommittedEntries,
				snapshot: rd.Snapshot,
				done:     make(chan struct{}),
			}

			select {
			case r.applyc <- apply:
			case <-r.stopped:
				return
			}
			// 保存snapshot
			if !raft.IsEmptySnap(rd.Snapshot) {
				if err := r.storage.SaveSnap(rd.Snapshot); err != nil {
					log.Fatalf("etcdraft: save snapshot error: %v", err)
				}
				r.raftStorage.ApplySnapshot(rd.Snapshot)
				log.Printf("etcdraft: applied incoming snapshot at index %d", rd.Snapshot.Metadata.Index)
			}
			if err := r.storage.Save(rd.HardState, rd.Entries); err != nil {
				log.Fatalf("etcdraft: save state and entries error: %v", err)
			}
			r.raftStorage.Append(rd.Entries)
			// 发送消息给远端peer
			r.s.send(rd.Messages)

			<-apply.done
			r.Advance()
		case <-syncC:
			r.s.sync(defaultSyncTimeout)
		case <-r.stopped:
			return
		}
	}
}

func (r *raftNode) apply() chan apply {
	return r.applyc
}

func (r *raftNode) stop() {
	r.Stop()
	r.transport.Stop()
	if err := r.storage.Close(); err != nil {
		log.Panicf("etcdraft: close storage error: %v", err)
	}
	close(r.done)
}

// for testing
func (r *raftNode) pauseSending() {
	p := r.transport.(rafthttp.Pausable)
	p.Pause()
}

func (r *raftNode) resumeSending() {
	p := r.transport.(rafthttp.Pausable)
	p.Resume()
}

// 启动状态机实例node,
// ids为成员id
func startNode(cfg *ServerConfig, ids []types.ID) (id types.ID, n raft.Node, s *raft.MemoryStorage, w *wal.WAL) {
	var err error
	member := cfg.Cluster.MemberByName(cfg.Name)
	metadata := pbutil.MustMarshal(
		&pb.Metadata{
			NodeID:    uint64(member.ID),
			ClusterID: uint64(cfg.Cluster.ID()),
		},
	)
	if err := os.MkdirAll(cfg.SnapDir(), privateDirMode); err != nil {
		log.Fatalf("etcdserver create snapshot directory error: %v", err)
	}
	if w, err = wal.Create(cfg.WALDir(), metadata); err != nil {
		log.Fatalf("etcdserver: create wal error: %v", err)
	}
	peers := make([]raft.Peer, len(ids))
	for i, id := range ids {
		ctx, err := json.Marshal((*cfg.Cluster).Member(id))
		if err != nil {
			log.Panicf("marshal member should never fail: %v", err)
		}
		peers[i] = raft.Peer{ID: uint64(id), Context: ctx}
	}
	id = member.ID
	log.Printf("etcdserver: start member %s in cluster %s", id, cfg.Cluster.ID())
	s = raft.NewMemoryStorage()
	c := &raft.Config{
		ID:              uint64(id),
		ElectionTick:    cfg.ElectionTicks,
		HeartbeatTick:   1,
		Storage:         s,
		MaxSizePerMsg:   maxSizePerMsg,
		MaxInflightMsgs: maxInflightMsgs,
	}
	// 启动一个raft状态机实例Node
	n = raft.StartNode(c, peers)
	raftStatus = n.Status
	return
}

// 重启node，
func restartNode(cfg *ServerConfig, snapshot *raftpb.Snapshot) (types.ID, raft.Node, *raft.MemoryStorage, *wal.WAL) {
	var walsnap walpb.Snapshot
	if snapshot != nil {
		walsnap.Index, walsnap.Term = snapshot.Metadata.Index, snapshot.Metadata.Term
	}
	w, id, cid, st, ents := readWAL(cfg.WALDir(), walsnap)
	cfg.Cluster.SetID(cid)

	log.Printf("etcdserver: restart member %s in cluster %s at commit index %d", id, cfg.Cluster.ID(), st.Commit)
	s := raft.NewMemoryStorage()
	if snapshot != nil {
		s.ApplySnapshot(*snapshot)
	}
	s.SetHardState(st)
	s.Append(ents)
	c := &raft.Config{
		ID:              uint64(id),
		ElectionTick:    cfg.ElectionTicks,
		HeartbeatTick:   1,
		Storage:         s,
		MaxSizePerMsg:   maxSizePerMsg,
		MaxInflightMsgs: maxInflightMsgs,
	}
	n := raft.RestartNode(c)
	raftStatus = n.Status
	return id, n, s, w
}

func restartAsStandaloneNode(cfg *ServerConfig, snapshot *raftpb.Snapshot) (types.ID, raft.Node, *raft.MemoryStorage, *wal.WAL) {
	var walsnap walpb.Snapshot
	if snapshot != nil {
		walsnap.Index, walsnap.Term = snapshot.Metadata.Index, snapshot.Metadata.Term
	}
	w, id, cid, st, ents := readWAL(cfg.WALDir(), walsnap)
	cfg.Cluster.SetID(cid)

	// discard the previously uncommitted entries
	for i, ent := range ents {
		if ent.Index > st.Commit {
			log.Printf("etcdserver: discarding %d uncommitted WAL entries ", len(ents)-i)
			ents = ents[:i]
			break
		}
	}

	// force append the configuration change entries
	toAppEnts := createConfigChangeEnts(getIDs(snapshot, ents), uint64(id), st.Term, st.Commit)
	ents = append(ents, toAppEnts...)

	// force commit newly appended entries
	err := w.Save(raftpb.HardState{}, toAppEnts)
	if err != nil {
		log.Fatalf("etcdserver: %v", err)
	}
	if len(ents) != 0 {
		st.Commit = ents[len(ents)-1].Index
	}

	log.Printf("etcdserver: forcing restart of member %s in cluster %s at commit index %d", id, cfg.Cluster.ID(), st.Commit)
	s := raft.NewMemoryStorage()
	if snapshot != nil {
		s.ApplySnapshot(*snapshot)
	}
	s.SetHardState(st)
	s.Append(ents)
	c := &raft.Config{
		ID:              uint64(id),
		ElectionTick:    cfg.ElectionTicks,
		HeartbeatTick:   1,
		Storage:         s,
		MaxSizePerMsg:   maxSizePerMsg,
		MaxInflightMsgs: maxInflightMsgs,
	}
	n := raft.RestartNode(c)
	raftStatus = n.Status
	return id, n, s, w
}

// getIDs returns an ordered set of IDs included in the given snapshot and
// the entries. The given snapshot/entries can contain two kinds of
// ID-related entry:
// - ConfChangeAddNode, in which case the contained ID will be added into the set.
// - ConfChangeAddRemove, in which case the contained ID will be removed from the set.
func getIDs(snap *raftpb.Snapshot, ents []raftpb.Entry) []uint64 {
	ids := make(map[uint64]bool)
	if snap != nil {
		for _, id := range snap.Metadata.ConfState.Nodes {
			ids[id] = true
		}
	}
	for _, e := range ents {
		if e.Type != raftpb.EntryConfChange {
			continue
		}
		var cc raftpb.ConfChange
		pbutil.MustUnmarshal(&cc, e.Data)
		switch cc.Type {
		case raftpb.ConfChangeAddNode:
			ids[cc.NodeID] = true
		case raftpb.ConfChangeRemoveNode:
			delete(ids, cc.NodeID)
		default:
			log.Panicf("ConfChange Type should be either ConfChangeAddNode or ConfChangeRemoveNode!")
		}
	}
	sids := make(types.Uint64Slice, 0)
	for id := range ids {
		sids = append(sids, id)
	}
	sort.Sort(sids)
	return []uint64(sids)
}

// createConfigChangeEnts creates a series of Raft entries (i.e.
// EntryConfChange) to remove the set of given IDs from the cluster. The ID
// `self` is _not_ removed, even if present in the set.
// If `self` is not inside the given ids, it creates a Raft entry to add a
// default member with the given `self`.
func createConfigChangeEnts(ids []uint64, self uint64, term, index uint64) []raftpb.Entry {
	ents := make([]raftpb.Entry, 0)
	next := index + 1
	found := false
	for _, id := range ids {
		if id == self {
			found = true
			continue
		}
		cc := &raftpb.ConfChange{
			Type:   raftpb.ConfChangeRemoveNode,
			NodeID: id,
		}
		e := raftpb.Entry{
			Type:  raftpb.EntryConfChange,
			Data:  pbutil.MustMarshal(cc),
			Term:  term,
			Index: next,
		}
		ents = append(ents, e)
		next++
	}
	if !found {
		m := Member{
			ID:             types.ID(self),
			RaftAttributes: RaftAttributes{PeerURLs: []string{"http://localhost:7001", "http://localhost:2380"}},
		}
		ctx, err := json.Marshal(m)
		if err != nil {
			log.Panicf("marshal member should never fail: %v", err)
		}
		cc := &raftpb.ConfChange{
			Type:    raftpb.ConfChangeAddNode,
			NodeID:  self,
			Context: ctx,
		}
		e := raftpb.Entry{
			Type:  raftpb.EntryConfChange,
			Data:  pbutil.MustMarshal(cc),
			Term:  term,
			Index: next,
		}
		ents = append(ents, e)
	}
	return ents
}
