package raft

import (
	"fmt"
	"io"
	"log"
	"net"
	"sync"
	"time"
)

var (
	keyCurrentTerm  = []byte("CurrentTerm")
	keyLastVoteTerm = []byte("LastVoteTerm")
	keyLastVoteCand = []byte("LastVoteCand")
	NotLeader       = fmt.Errorf("node is not the leader")
	LeadershipLost  = fmt.Errorf("leadership lost while committing log")
	RaftShutdown    = fmt.Errorf("raft is already shutdown")
	EnqueueTimeout  = fmt.Errorf("timed out enqueuing operation")
	KnownPeer       = fmt.Errorf("peer already known")
	UnknownPeer     = fmt.Errorf("peer is unknown")
)

// commitTupel is used to send an index that was committed,
// with an optional associated future that should be invoked
type commitTuple struct {
	log    *Log
	future *logFuture
}

// leaderState is state that is used while we are a leader
type leaderState struct {
	commitCh  chan *logFuture
	inflight  *inflight
	replState map[string]*followerReplication
}

type Raft struct {
	raftState

	// applyCh is used to async send logs to the main thread to
	// be committed and applied to the FSM.
	applyCh chan *logFuture

	// Configuration provided at Raft initialization
	conf *Config

	// FSM is the client state machine to apply commands to
	fsm FSM

	// fsmCommitCh is used to trigger async application of logs to the fsm
	fsmCommitCh chan commitTuple

	// fsmRestoreCh is used to trigger a restore from snapshot
	fsmRestoreCh chan *restoreFuture

	// fsmSnapshotCh is used to trigger a new snapshot being taken
	fsmSnapshotCh chan *reqSnapshotFuture

	// leaderState used only while state is leader
	leaderState leaderState

	// Stores our local addr
	localAddr net.Addr

	// LogStore provides durable storage for logs
	logs LogStore

	// Track our known peers
	peers     []net.Addr
	peerStore PeerStore

	// RPC chan comes from the transport layer
	rpcCh <-chan RPC

	// Shutdown channel to exit, protected to prevent concurrent exits
	shutdown     bool
	shutdownCh   chan struct{}
	shutdownLock sync.Mutex

	// snapshots is used to store and retrieve snapshots
	snapshots SnapshotStore

	// snapshotCh is used for user triggered snapshots
	snapshotCh chan *snapshotFuture

	// stable is a StableStore implementation for durable state
	// It provides stable storage for many fields in raftState
	stable StableStore

	// The transport layer we use
	trans Transport
}

// NewRaft is used to construct a new Raft node
func NewRaft(conf *Config, fsm FSM, logs LogStore, stable StableStore, snaps SnapshotStore,
	peerStore PeerStore, trans Transport) (*Raft, error) {
	// Try to restore the current term
	currentTerm, err := stable.GetUint64(keyCurrentTerm)
	if err != nil && err.Error() != "not found" {
		return nil, fmt.Errorf("Failed to load current term: %v", err)
	}

	// Read the last log value
	lastLog, err := logs.LastIndex()
	if err != nil {
		return nil, fmt.Errorf("Failed to find last log: %v", err)
	}

	// Construct the list of peers that excludes us
	localAddr := trans.LocalAddr()
	peers, err := peerStore.Peers()
	if err != nil {
		return nil, fmt.Errorf("Failed to get list of peers: %v", err)
	}
	peers = excludePeer(peers, localAddr)

	// Create Raft struct
	r := &Raft{
		applyCh:       make(chan *logFuture),
		conf:          conf,
		fsm:           fsm,
		fsmCommitCh:   make(chan commitTuple, 128),
		fsmRestoreCh:  make(chan *restoreFuture),
		fsmSnapshotCh: make(chan *reqSnapshotFuture),
		localAddr:     localAddr,
		logs:          logs,
		peers:         peers,
		peerStore:     peerStore,
		rpcCh:         trans.Consumer(),
		snapshots:     snaps,
		snapshotCh:    make(chan *snapshotFuture),
		shutdownCh:    make(chan struct{}),
		stable:        stable,
		trans:         trans,
	}

	// Initialize as a follower
	r.setState(Follower)

	// Restore the current term and the last log
	r.setCurrentTerm(currentTerm)
	r.setLastLog(lastLog)

	// Attempt to restore a snapshot if there are any
	if err := r.restoreSnapshot(); err != nil {
		return nil, err
	}

	// Start the background work
	r.goFunc(r.run)
	r.goFunc(r.runFSM)
	r.goFunc(r.runSnapshots)
	return r, nil
}

// Apply is used to apply a command to the FSM in a highly consistent
// manner. This returns a future that can be used to wait on the application.
// An optional timeout can be provided to limit the amount of time we wait
// for the command to be started. This must be run on the leader or it
// will fail.
func (r *Raft) Apply(cmd []byte, timeout time.Duration) Future {
	var timer <-chan time.Time
	if timeout > 0 {
		timer = time.After(timeout)
	}

	// Create a log future, no index or term yet
	logFuture := &logFuture{
		log: Log{
			Type: LogCommand,
			Data: cmd,
		},
	}
	logFuture.init()

	select {
	case <-timer:
		return errorFuture{EnqueueTimeout}
	case <-r.shutdownCh:
		return errorFuture{RaftShutdown}
	case r.applyCh <- logFuture:
		return logFuture
	}
}

// AddPeer is used to add a new peer into the cluster. This must be
// run on the leader or it will fail.
func (r *Raft) AddPeer(peer net.Addr) Future {
	logFuture := &logFuture{
		log: Log{
			Type: LogAddPeer,
			peer: peer,
		},
	}
	logFuture.init()
	select {
	case r.applyCh <- logFuture:
		return logFuture
	case <-r.shutdownCh:
		return errorFuture{RaftShutdown}
	}
}

// RemovePeer is used to remove a peer from the cluster. If the
// current leader is being removed, it will cause a new election
// to occur. This must be run on the leader or it will fail.
func (r *Raft) RemovePeer(peer net.Addr) Future {
	logFuture := &logFuture{
		log: Log{
			Type: LogRemovePeer,
			peer: peer,
		},
		policy: newExcludeNodeQuorum(len(r.peers)+1, peer),
	}
	logFuture.init()
	select {
	case r.applyCh <- logFuture:
		return logFuture
	case <-r.shutdownCh:
		return errorFuture{RaftShutdown}
	}
}

// Shutdown is used to stop the Raft background routines.
// This is not a graceful operation. Provides a future that
// can be used to block until all background routines have exited.
func (r *Raft) Shutdown() Future {
	r.shutdownLock.Lock()
	defer r.shutdownLock.Unlock()

	if !r.shutdown {
		close(r.shutdownCh)
		r.shutdown = true
		r.setState(Shutdown)
	}

	return &shutdownFuture{r}
}

// Snapshot is used to manually force Raft to take a snapshot
// Returns a future that can be used to block until complete.
func (r *Raft) Snapshot() Future {
	snapFuture := &snapshotFuture{}
	snapFuture.init()
	select {
	case r.snapshotCh <- snapFuture:
		return snapFuture
	case <-r.shutdownCh:
		return errorFuture{RaftShutdown}
	}

}

// State is used to return the state raft is currently in
func (r *Raft) State() RaftState {
	return r.getState()
}

func (r *Raft) String() string {
	return fmt.Sprintf("Node at %s", r.localAddr.String())
}

// runFSM is a long running goroutine responsible for applying logs
// to the FSM. This is done async of other logs since we don't want
// the FSM to block our internal operations.
func (r *Raft) runFSM() {
	var lastIndex, lastTerm uint64
	for {
		select {
		case req := <-r.fsmRestoreCh:
			// Open the snapshot
			meta, source, err := r.snapshots.Open(req.ID)
			if err != nil {
				req.respond(fmt.Errorf("Failed to open snapshot %v: %v",
					req.ID, err))
				continue
			}

			// Attempt to restore
			if err := r.fsm.Restore(source); err != nil {
				req.respond(fmt.Errorf("Failed to restore snapshot %v: %v",
					req.ID, err))
				source.Close()
				continue
			}
			source.Close()

			// Update the last index and term
			lastIndex = meta.Index
			lastTerm = meta.Term

		case req := <-r.fsmSnapshotCh:
			// Get our peers
			peers, err := r.peerStore.Peers()
			if err != nil {
				req.respond(err)
			}

			// Start a snapshot
			snap, err := r.fsm.Snapshot()

			// Respond to the request
			req.index = lastIndex
			req.term = lastTerm
			req.peers = peers
			req.snapshot = snap
			req.respond(err)

		case commitTuple := <-r.fsmCommitCh:
			// Apply the log
			r.fsm.Apply(commitTuple.log.Data)

			// Update the indexes
			lastIndex = commitTuple.log.Index
			lastTerm = commitTuple.log.Term

			// Invoke the future if given
			if commitTuple.future != nil {
				commitTuple.future.respond(nil)
			}
		case <-r.shutdownCh:
			return
		}
	}
}

// run is a long running goroutine that runs the Raft FSM
func (r *Raft) run() {
	for {
		// Check if we are doing a shutdown
		select {
		case <-r.shutdownCh:
			return
		default:
		}

		// Enter into a sub-FSM
		switch r.getState() {
		case Follower:
			r.runFollower()
		case Candidate:
			r.runCandidate()
		case Leader:
			r.runLeader()
		}
	}
}

// runFollower runs the FSM for a follower
func (r *Raft) runFollower() {
	log.Printf("[INFO] %v entering Follower state", r)
	for {
		select {
		case rpc := <-r.rpcCh:
			switch cmd := rpc.Command.(type) {
			case *AppendEntriesRequest:
				r.appendEntries(rpc, cmd)
			case *RequestVoteRequest:
				r.requestVote(rpc, cmd)
			case *InstallSnapshotRequest:
				r.installSnapshot(rpc, cmd)
			default:
				log.Printf("[ERR] Follower state, got unexpected command: %#v",
					rpc.Command)
				rpc.Respond(nil, fmt.Errorf("Unexpected command"))
			}

		case a := <-r.applyCh:
			// Reject any operations since we are not the leader
			a.respond(NotLeader)

		case <-randomTimeout(r.conf.HeartbeatTimeout):
			// Heartbeat failed! Transition to the candidate state
			log.Printf("[WARN] Heartbeat timeout reached, starting election")
			r.setState(Candidate)
			return

		case <-r.shutdownCh:
			return
		}
	}
}

// runCandidate runs the FSM for a candidate
func (r *Raft) runCandidate() {
	log.Printf("[INFO] %v entering Candidate state", r)

	// Start vote for us, and set a timeout
	voteCh := r.electSelf()
	electionTimer := randomTimeout(r.conf.ElectionTimeout)

	// Tally the votes, need a simple majority
	grantedVotes := 0
	votesNeeded := ((len(r.peers) + 1) / 2) + 1
	log.Printf("[DEBUG] Votes needed: %d", votesNeeded)

	for r.getState() == Candidate {
		select {
		case rpc := <-r.rpcCh:
			switch cmd := rpc.Command.(type) {
			case *AppendEntriesRequest:
				r.appendEntries(rpc, cmd)
			case *RequestVoteRequest:
				r.requestVote(rpc, cmd)
			default:
				log.Printf("[ERR] Candidate state, got unexpected command: %#v",
					rpc.Command)
				rpc.Respond(nil, fmt.Errorf("Unexpected command"))
			}

		case vote := <-voteCh:
			// Check if the term is greater than ours, bail
			if vote.Term > r.getCurrentTerm() {
				log.Printf("[DEBUG] Newer term discovered")
				r.setState(Follower)
				if err := r.setCurrentTerm(vote.Term); err != nil {
					log.Printf("[ERR] Failed to update current term: %v", err)
				}
				return
			}

			// Check if the vote is granted
			if vote.Granted {
				grantedVotes++
				log.Printf("[DEBUG] Vote granted. Tally: %d", grantedVotes)
			}

			// Check if we've become the leader
			if grantedVotes >= votesNeeded {
				log.Printf("[INFO] Election won. Tally: %d", grantedVotes)
				r.setState(Leader)
				return
			}

		case a := <-r.applyCh:
			// Reject any operations since we are not the leader
			a.respond(NotLeader)

		case <-electionTimer:
			// Election failed! Restart the elction. We simply return,
			// which will kick us back into runCandidate
			log.Printf("[WARN] Election timeout reached, restarting election")
			return

		case <-r.shutdownCh:
			return
		}
	}
}

// runLeader runs the FSM for a leader. Do the setup here and drop into
// the leaderLoop for the hot loop
func (r *Raft) runLeader() {
	log.Printf("[INFO] %v entering Leader state", r)

	// Setup leader state
	r.leaderState.commitCh = make(chan *logFuture, 128)
	r.leaderState.inflight = newInflight(r.leaderState.commitCh)
	r.leaderState.replState = make(map[string]*followerReplication)

	// Cleanup state on step down
	defer func() {
		// Stop replication
		for _, p := range r.leaderState.replState {
			close(p.stopCh)
		}

		// Cancel inflight requests
		r.leaderState.inflight.Cancel(LeadershipLost)

		// Clear all the staet
		r.leaderState.commitCh = nil
		r.leaderState.inflight = nil
		r.leaderState.replState = nil
	}()

	// Start a replication routine for each peer
	for _, peer := range r.peers {
		r.startReplication(peer)
	}

	// Dispatch a no-op log first
	noop := &logFuture{log: Log{Type: LogNoop}}
	r.dispatchLog(noop)

	// Sit in the leader loop until we step down
	r.leaderLoop()
}

// startReplication is a helper to setup state and start async replication to a peer
func (r *Raft) startReplication(peer net.Addr) {
	s := &followerReplication{
		peer:        peer,
		inflight:    r.leaderState.inflight,
		stopCh:      make(chan uint64, 1),
		triggerCh:   make(chan struct{}, 1),
		currentTerm: r.getCurrentTerm(),
		matchIndex:  r.getLastLog(),
		nextIndex:   r.getLastLog() + 1,
	}
	r.leaderState.replState[peer.String()] = s
	r.goFunc(func() { r.replicate(s) })
}

// leaderLoop is the hot loop for a leader, it is invoked
// after all the various leader setup is done
func (r *Raft) leaderLoop() {
	for r.getState() == Leader {
		select {
		case rpc := <-r.rpcCh:
			switch cmd := rpc.Command.(type) {
			case *AppendEntriesRequest:
				r.appendEntries(rpc, cmd)
			case *RequestVoteRequest:
				r.requestVote(rpc, cmd)
			default:
				log.Printf("[ERR] Leader state, got unexpected command: %#v",
					rpc.Command)
				rpc.Respond(nil, fmt.Errorf("Unexpected command"))
			}

		case commitLog := <-r.leaderState.commitCh:
			// Increment the commit index
			idx := commitLog.log.Index
			r.setCommitIndex(idx)
			r.processLogs(idx, commitLog)

		case newLog := <-r.applyCh:
			// Prepare peer set changes
			if newLog.log.Type == LogAddPeer || newLog.log.Type == LogRemovePeer {
				if !r.preparePeerChange(newLog) {
					continue
				}
			}
			r.dispatchLog(newLog)

		case <-r.shutdownCh:
			return
		}
	}
}

// preparePeerChange checks if a LogAddPeer or LogRemovePeer should be performed,
// and properly formats the data field on the log before dispatching it.
func (r *Raft) preparePeerChange(l *logFuture) bool {
	// Check if this is a known peer
	p := l.log.peer
	knownPeer := peerContained(r.peers, p) || r.localAddr.String() == p.String()

	// Ignore known peers on add
	if l.log.Type == LogAddPeer && knownPeer {
		l.respond(KnownPeer)
		return false
	}

	// Ignore unknown peers on remove
	if l.log.Type == LogRemovePeer && !knownPeer {
		l.respond(UnknownPeer)
		return false
	}

	// Construct the peer set
	var peerSet []net.Addr
	if l.log.Type == LogAddPeer {
		peerSet = append([]net.Addr{p, r.localAddr}, r.peers...)
	} else {
		peerSet = excludePeer(append([]net.Addr{r.localAddr}, r.peers...), p)
	}

	// Setup the log
	l.log.Data = encodePeers(peerSet, r.trans)
	return true
}

// dispatchLog is called to push a log to disk, mark it
// as inflight and begin replication of it
func (r *Raft) dispatchLog(applyLog *logFuture) {
	// Prepare log
	applyLog.log.Index = r.getLastLog() + 1
	applyLog.log.Term = r.getCurrentTerm()

	// Write the log entry locally
	if err := r.logs.StoreLog(&applyLog.log); err != nil {
		log.Printf("[ERR] Failed to commit log: %v", err)
		applyLog.respond(err)
		r.setState(Follower)
		return
	}

	// Add a quorum policy if none
	if applyLog.policy == nil {
		applyLog.policy = newMajorityQuorum(len(r.peers) + 1)
	}

	// Add this to the inflight logs, commit
	r.leaderState.inflight.Start(applyLog)
	r.leaderState.inflight.Commit(applyLog.log.Index, r.localAddr)

	// Update the last log since it's on disk now
	r.setLastLog(applyLog.log.Index)

	// Notify the replicators of the new log
	for _, f := range r.leaderState.replState {
		asyncNotifyCh(f.triggerCh)
	}
}

// processLogs is used to process all the logs from the lastApplied
// up to the given index
func (r *Raft) processLogs(index uint64, future *logFuture) {
	// Reject logs we've applied already
	if index <= r.getLastApplied() {
		log.Printf("[WARN] Skipping application of old log: %d",
			index)
		return
	}

	// Apply all the preceeding logs
	for idx := r.getLastApplied() + 1; idx <= index; idx++ {
		// Get the log, either from the future or from our log store
		if future != nil && future.log.Index == idx {
			r.processLog(&future.log, future)

		} else {
			l := new(Log)
			if err := r.logs.GetLog(idx, l); err != nil {
				log.Printf("[ERR] Failed to get log at %d: %v", idx, err)
				panic(err)
			}
			r.processLog(l, nil)
		}
	}

	// Update the lastApplied
	r.setLastApplied(index)
}

// processLog is invoked to process the application of a single committed log
func (r *Raft) processLog(l *Log, future *logFuture) {
	switch l.Type {
	case LogCommand:
		// Forward to the fsm handler
		select {
		case r.fsmCommitCh <- commitTuple{l, future}:
		case <-r.shutdownCh:
			if future != nil {
				future.respond(RaftShutdown)
			}
		}

		// Return so that the future is only responded to
		// by the FSM handler when the application is done
		return

	case LogAddPeer:
		peers := decodePeers(l.Data, r.trans)
		log.Printf("[DEBUG] Node %v updated peer set (add): %v", r.localAddr, peers)

		// Update our peer set
		r.peers = excludePeer(peers, r.localAddr)
		r.peerStore.SetPeers(peers)

		// Handle replication if we are the leader
		if r.getState() == Leader {
			for _, p := range r.peers {
				if _, ok := r.leaderState.replState[p.String()]; !ok {
					log.Printf("[INFO] Added peer %v, starting replication", p)
					r.startReplication(p)
				}
			}
		}

	case LogRemovePeer:
		peers := decodePeers(l.Data, r.trans)
		log.Printf("[DEBUG] Node %v updated peer set (remove): %v", r.localAddr, peers)

		// If the peer set does not include us, remove all other peers
		removeSelf := !peerContained(peers, r.localAddr)
		if removeSelf {
			r.peers = nil
			r.peerStore.SetPeers([]net.Addr{r.localAddr})
		} else {
			r.peers = excludePeer(peers, r.localAddr)
			r.peerStore.SetPeers(peers)
		}

		// Stop replication for old nodes
		if r.getState() == Leader {
			var toDelete []string
			for _, repl := range r.leaderState.replState {
				if !peerContained(r.peers, repl.peer) {
					log.Printf("[INFO] Removed peer %v, stopping replication", repl.peer)

					// Replicate up to this index and stop
					repl.stopCh <- l.Index
					toDelete = append(toDelete, repl.peer.String())
				}
			}
			for _, name := range toDelete {
				delete(r.leaderState.replState, name)
			}
		}

		// Handle removing ourself
		if removeSelf {
			if r.conf.ShutdownOnRemove {
				log.Printf("[INFO] Removed ourself, shutting down")
				r.Shutdown()
			} else {
				log.Printf("[INFO] Removed ourself, transitioning to follower")
				r.setState(Follower)
			}
		}

	case LogNoop:
		// Ignore the no-op
	default:
		log.Printf("[ERR] Got unrecognized log type: %#v", l)
	}

	// Invoke the future if given
	if future != nil {
		future.respond(nil)
	}
}

// appendEntries is invoked when we get an append entries RPC call
func (r *Raft) appendEntries(rpc RPC, a *AppendEntriesRequest) {
	// Setup a response
	resp := &AppendEntriesResponse{
		Term:    r.getCurrentTerm(),
		LastLog: r.getLastLog(),
		Success: false,
	}
	var rpcErr error
	defer rpc.Respond(resp, rpcErr)

	// Ignore an older term
	if a.Term < r.getCurrentTerm() {
		return
	}

	// Increase the term if we see a newer one, also transition to follower
	// if we ever get an appendEntries call
	if a.Term > r.getCurrentTerm() || r.getState() != Follower {
		// Ensure transition to follower
		r.setState(Follower)
		if err := r.setCurrentTerm(a.Term); err != nil {
			log.Printf("[ERR] Failed to update current term: %v", err)
			return
		}
		resp.Term = a.Term
	}

	// Verify the last log entry
	var prevLog Log
	if a.PrevLogEntry > 0 {
		if err := r.logs.GetLog(a.PrevLogEntry, &prevLog); err != nil {
			log.Printf("[WARN] Failed to get previous log: %d %v",
				a.PrevLogEntry, err)
			return
		}
		if a.PrevLogTerm != prevLog.Term {
			log.Printf("[WARN] Previous log term mis-match: ours: %d remote: %d",
				prevLog.Term, a.PrevLogTerm)
			return
		}
	}

	// Add all the entries
	for _, entry := range a.Entries {
		// Delete any conflicting entries
		if entry.Index <= r.getLastLog() {
			log.Printf("[WARN] Clearing log suffix from %d to %d", entry.Index, r.getLastLog())
			if err := r.logs.DeleteRange(entry.Index, r.getLastLog()); err != nil {
				log.Printf("[ERR] Failed to clear log suffix: %v", err)
				return
			}
		}

		// Append the entry
		if err := r.logs.StoreLog(entry); err != nil {
			log.Printf("[ERR] Failed to append to log: %v", err)
			return
		}

		// Update the lastLog
		r.setLastLog(entry.Index)
	}

	// Update the commit index
	if a.LeaderCommitIndex > 0 && a.LeaderCommitIndex > r.getCommitIndex() {
		idx := min(a.LeaderCommitIndex, r.getLastLog())
		r.setCommitIndex(idx)
		r.processLogs(idx, nil)
	}

	// Everything went well, set success
	resp.Success = true
	return
}

// requestVote is invoked when we get an request vote RPC call
func (r *Raft) requestVote(rpc RPC, req *RequestVoteRequest) {
	// Setup a response
	resp := &RequestVoteResponse{
		Term:    r.getCurrentTerm(),
		Peers:   r.peers,
		Granted: false,
	}
	var rpcErr error
	defer rpc.Respond(resp, rpcErr)

	// Ignore an older term
	if req.Term < r.getCurrentTerm() {
		return
	}

	// Increase the term if we see a newer one
	if req.Term > r.getCurrentTerm() {
		// Ensure transition to follower
		r.setState(Follower)
		if err := r.setCurrentTerm(req.Term); err != nil {
			log.Printf("[ERR] Failed to update current term: %v", err)
			return
		}
		resp.Term = req.Term
	}

	// Check if we have voted yet
	lastVoteTerm, err := r.stable.GetUint64(keyLastVoteTerm)
	if err != nil && err.Error() != "not found" {
		log.Printf("[ERR] Failed to get last vote term: %v", err)
		return
	}
	lastVoteCandBytes, err := r.stable.Get(keyLastVoteCand)
	if err != nil && err.Error() != "not found" {
		log.Printf("[ERR] Failed to get last vote candidate: %v", err)
		return
	}

	// Check if we've voted in this election before
	if lastVoteTerm == req.Term && lastVoteCandBytes != nil {
		log.Printf("[INFO] Duplicate RequestVote for same term: %d", req.Term)
		if string(lastVoteCandBytes) == req.Candidate.String() {
			log.Printf("[WARN] Duplicate RequestVote from candidate: %s", req.Candidate)
			resp.Granted = true
		}
		return
	}

	// Reject if their term is older
	if r.getLastLog() > 0 {
		var lastLog Log
		if err := r.logs.GetLog(r.getLastLog(), &lastLog); err != nil {
			log.Printf("[ERR] Failed to get last log: %d %v",
				r.getLastLog(), err)
			return
		}
		if lastLog.Term > req.LastLogTerm {
			log.Printf("[WARN] Rejecting vote since our last term is greater")
			return
		}

		if lastLog.Index > req.LastLogIndex {
			log.Printf("[WARN] Rejecting vote since our last index is greater")
			return
		}
	}

	// Persist a vote for safety
	if err := r.persistVote(req.Term, req.Candidate.String()); err != nil {
		log.Printf("[ERR] Failed to persist vote: %v", err)
		return
	}

	resp.Granted = true
	return
}

// installSnapshot is invoked when we get a InstallSnapshot RPC call.
// We must be in the follower state for this, since it means we are
// too far behind a leader for log replay.
func (r *Raft) installSnapshot(rpc RPC, req *InstallSnapshotRequest) {
	// Setup a response
	resp := &InstallSnapshotResponse{
		Term:    r.getCurrentTerm(),
		Success: false,
	}
	var rpcErr error
	defer rpc.Respond(resp, rpcErr)

	// Ignore an older term
	if req.Term < r.getCurrentTerm() {
		return
	}

	// Increase the term if we see a newer one
	if req.Term > r.getCurrentTerm() {
		// Ensure transition to follower
		r.setState(Follower)
		if err := r.setCurrentTerm(req.Term); err != nil {
			log.Printf("[ERR] Failed to update current term: %v", err)
			return
		}
		resp.Term = req.Term
	}

	// Encode the peerset
	peerSet := encodePeers(req.Peers, r.trans)

	// Create a new snapshot
	sink, err := r.snapshots.Create(req.LastLogIndex, req.LastLogTerm, peerSet)
	if err != nil {
		log.Printf("[ERR] Failed to create snapshot to install: %v", err)
		rpcErr = fmt.Errorf("Failed to create snapshot: %v", err)
		return
	}

	// Spill the remote snapshot to disk
	n, err := io.Copy(sink, rpc.Reader)
	if err != nil {
		sink.Cancel()
		log.Printf("[ERR] Failed to copy snapshot: %v", err)
		rpcErr = err
		return
	}

	// Finalize the snapshot
	if err := sink.Close(); err != nil {
		log.Printf("[ERR] Failed to finalize snapshot: %v", err)
		rpcErr = err
		return
	} else {
		log.Printf("[INFO] Copied %d bytes to local snapshot", n)
	}

	// Restore snapshot
	future := &restoreFuture{ID: sink.ID()}
	select {
	case r.fsmRestoreCh <- future:
	case <-r.shutdownCh:
		future.respond(RaftShutdown)
	}

	// Wait for the restore to happen
	if err := future.Error(); err != nil {
		log.Printf("[ERR] Failed to restore snapshot: %v", err)
		rpcErr = err
		return
	}

	// Update the lastApplied so we don't replay old logs
	r.setLastApplied(req.LastLogIndex)

	// Restore the peer set
	r.peers = excludePeer(req.Peers, r.localAddr)

	// Compact logs, continue even if this fails
	if err := r.compactLogs(req.LastLogIndex); err != nil {
		log.Printf("[ERR] Failed to compact logs: %v", err)
	}

	resp.Success = true
	return
}

// electSelf is used to send a RequestVote RPC to all peers,
// and vote for ourself. This has the side affecting of incrementing
// the current term. The response channel returned is used to wait
// for all the responses (including a vote for ourself).
func (r *Raft) electSelf() <-chan *RequestVoteResponse {
	// Create a response channel
	respCh := make(chan *RequestVoteResponse, len(r.peers)+1)

	// Get the last log
	var lastLog Log
	if r.getLastLog() > 0 {
		if err := r.logs.GetLog(r.getLastLog(), &lastLog); err != nil {
			log.Printf("[ERR] Failed to get last log: %d %v",
				r.getLastLog(), err)
			return nil
		}
	}

	// Increment the term
	if err := r.setCurrentTerm(r.getCurrentTerm() + 1); err != nil {
		log.Printf("[ERR] Failed to update current term: %v", err)
		return nil
	}

	// Construct the request
	req := &RequestVoteRequest{
		Term:         r.getCurrentTerm(),
		Candidate:    r.localAddr,
		LastLogIndex: lastLog.Index,
		LastLogTerm:  lastLog.Term,
	}

	// Construct a function to ask for a vote
	askPeer := func(peer net.Addr) {
		r.goFunc(func() {
			resp := new(RequestVoteResponse)
			err := r.trans.RequestVote(peer, req, resp)
			if err != nil {
				log.Printf("[ERR] Failed to make RequestVote RPC to %v: %v", peer, err)
				resp.Term = req.Term
				resp.Granted = false
			}

			// If we are not a peer, we could have been removed but failed
			// to receive the log message. OR it could mean an improperly configured
			// cluster. Either way, we should warn
			if err == nil && !peerContained(resp.Peers, r.localAddr) {
				log.Printf("[WARN] Remote peer %v does not have local node %v as a peer",
					peer, r.localAddr)
			}

			respCh <- resp
		})
	}

	// For each peer, request a vote
	for _, peer := range r.peers {
		askPeer(peer)
	}

	// Persist a vote for ourselves
	if err := r.persistVote(req.Term, req.Candidate.String()); err != nil {
		log.Printf("[ERR] Failed to persist vote : %v", err)
		return nil
	}

	// Include our own vote
	respCh <- &RequestVoteResponse{
		Term:    req.Term,
		Peers:   []net.Addr{r.localAddr},
		Granted: true,
	}
	return respCh
}

// persistVote is used to persist our vote for safety
func (r *Raft) persistVote(term uint64, candidate string) error {
	if err := r.stable.SetUint64(keyLastVoteTerm, term); err != nil {
		return err
	}
	if err := r.stable.Set(keyLastVoteCand, []byte(candidate)); err != nil {
		return err
	}
	return nil
}

// setCurrentTerm is used to set the current term in a durable manner
func (r *Raft) setCurrentTerm(t uint64) error {
	// Persist to disk first
	if err := r.stable.SetUint64(keyCurrentTerm, t); err != nil {
		log.Printf("[ERR] Failed to save current term: %v", err)
		return err
	}
	r.raftState.setCurrentTerm(t)
	return nil
}

// runSnapshots is a long running goroutine used to manage taking
// new snapshots of the FSM. It runs in parallel to the FSM and
// main goroutines, so that snapshots do not block normal operation.
func (r *Raft) runSnapshots() {
	for {
		select {
		case <-randomTimeout(r.conf.SnapshotInterval):
			// Check if we should snapshot
			if !r.shouldSnapshot() {
				continue
			}

			// Trigger a snapshot
			if err := r.takeSnapshot(); err != nil {
				log.Printf("[ERR] Failed to take snapshot: %v", err)
			}

		case future := <-r.snapshotCh:
			// User-triggered, run immediately
			err := r.takeSnapshot()
			if err != nil {
				log.Printf("[ERR] Failed to take snapshot: %v", err)
			}
			future.respond(err)

		case <-r.shutdownCh:
			return
		}
	}
}

// shouldSnapshot checks if we meet the conditions to take
// a new snapshot
func (r *Raft) shouldSnapshot() bool {
	// Check the spread of logs
	firstIdx, err := r.logs.FirstIndex()
	if err != nil {
		log.Printf("[ERR] Failed to get first log index: %v", err)
		return false
	}
	lastIdx, err := r.logs.LastIndex()
	if err != nil {
		log.Printf("[ERR] Failed to get last log index: %v", err)
		return false
	}

	// Compare the delta to the threshold
	delta := lastIdx - firstIdx
	return delta >= r.conf.SnapshotThreshold
}

// takeSnapshot is used to take a new snapshot
func (r *Raft) takeSnapshot() error {
	// Create a snapshot request
	req := &reqSnapshotFuture{}
	req.init()

	// Wait for dispatch or shutdown
	select {
	case r.fsmSnapshotCh <- req:
	case <-r.shutdownCh:
		return RaftShutdown
	}

	// Wait until we get a response
	if err := req.Error(); err != nil {
		return fmt.Errorf("Failed to start snapshot: %v", err)
	}
	defer req.snapshot.Release()

	// Log that we are starting the snapshot
	log.Printf("[INFO] Starting snapshot up to %d", req.index)

	// Encode the peerset
	peerSet := encodePeers(req.peers, r.trans)

	// Create a new snapshot
	sink, err := r.snapshots.Create(req.index, req.term, peerSet)
	if err != nil {
		return fmt.Errorf("Failed to create snapshot: %v", err)
	}

	// Try to persist the snapshot
	if err := req.snapshot.Persist(sink); err != nil {
		sink.Cancel()
		return fmt.Errorf("Failed to persist snapshot: %v", err)
	}

	// Close and check for error
	if err := sink.Close(); err != nil {
		return fmt.Errorf("Failed to close snapshot: %v", err)
	}

	// Compact the logs
	if err := r.compactLogs(req.index); err != nil {
		return err
	}

	// Log completion
	log.Printf("[INFO] Snapshot to %d complete", req.index)
	return nil
}

// compactLogs takes the last inclusive index of a snapshot
// and trims the logs that are no longer needed
func (r *Raft) compactLogs(snapIdx uint64) error {
	// Determine log ranges to compact
	minLog, err := r.logs.FirstIndex()
	if err != nil {
		return fmt.Errorf("Failed to get first log index: %v", err)
	}

	// Truncate up to the end of the snapshot, or `TrailingLogs`
	// back from the head, which ever is futher back. This ensures
	// at least `TrailingLogs` entries, but does not allow logs
	// after the snapshot to be removed
	maxLog := min(snapIdx, r.getLastLog()-r.conf.TrailingLogs)

	// Log this
	log.Printf("[INFO] Compacting logs from %d to %d", minLog, maxLog)

	// Compact the logs
	if err := r.logs.DeleteRange(minLog, maxLog); err != nil {
		return fmt.Errorf("Log compaction failed: %v", err)
	}
	return nil
}

// restoreSnapshot attempts to restore the latest snapshots, and fails
// if none of them can be restored. This is called at initialization time,
// and is completely unsafe to call at any other time.
func (r *Raft) restoreSnapshot() error {
	snapshots, err := r.snapshots.List()
	if err != nil {
		log.Printf("[ERR] Failed to list snapshots: %v", err)
		return err
	}

	// Try to load in order of newest to oldest
	for _, snapshot := range snapshots {
		_, source, err := r.snapshots.Open(snapshot.ID)
		if err != nil {
			log.Printf("[ERR] Failed to open snapshot %v: %v", snapshot.ID, err)
			continue
		}
		defer source.Close()

		if err := r.fsm.Restore(source); err != nil {
			log.Printf("[ERR] Failed to restore snapshot %v: %v", snapshot.ID, err)
			continue
		}

		// Update the lastApplied so we don't replay old logs
		r.setLastApplied(snapshot.Index)

		// Restore the peer set
		peerSet := decodePeers(snapshot.Peers, r.trans)
		r.peers = excludePeer(peerSet, r.localAddr)

		// Success!
		return nil
	}

	// If we had snapshots and failed to load them, its an error
	if len(snapshots) > 0 {
		return fmt.Errorf("Failed to load any existing snapshots!")
	}
	return nil
}
