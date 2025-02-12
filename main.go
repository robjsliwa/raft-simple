package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"math/rand"

	"github.com/gorilla/mux"
)

type ServerState string

const (
    Follower  ServerState = "Follower"
    Candidate ServerState = "Candidate"
    Leader    ServerState = "Leader"
)

type LogEntry struct {
    Term    int         `json:"term"`
    Command interface{} `json:"command"`
}

type RaftNode struct {
    mu sync.Mutex

    // Server identifier and peer addresses
    id            string   // This node's ID.
    peerAddresses []string // List of other nodes in the cluster (host:port).

    // Persistent state on every node
    currentTerm int        // latest term server has seen
    votedFor    *string    // candidateId that received vote in current term
    log         []LogEntry // log entries

    // Volatile state on every node
    commitIndex int // index of highest log entry known to be committed
    lastApplied int // index of highest log entry applied to state machine

    // Volatile state on leaders for managing followers
    nextIndex  map[string]int // for each follower, index of the next log entry to send
    matchIndex map[string]int // for each follower, index of highest log entry known to be replicated

    // Raft node state
    state ServerState

    // Election timeout
    electionTimeout chan struct{}
    // Node stop channel
    stopChan        chan struct{}
}

type AppendEntriesArgs struct {
    Term         int         `json:"term"`
    LeaderID     string      `json:"leaderId"`
    PrevLogIndex int         `json:"prevLogIndex"`
    PrevLogTerm  int         `json:"prevLogTerm"`
    Entries      []LogEntry  `json:"entries"`
    LeaderCommit int         `json:"leaderCommit"`
}

type AppendEntriesReply struct {
    Term    int  `json:"term"`
    Success bool `json:"success"`
}

type RequestVoteArgs struct {
    Term         int    `json:"term"`
    CandidateID  string `json:"candidateId"`
    LastLogIndex int    `json:"lastLogIndex"`
    LastLogTerm  int    `json:"lastLogTerm"`
}

type RequestVoteReply struct {
    Term        int  `json:"term"`
    VoteGranted bool `json:"voteGranted"`
}

type ClientCommandArgs struct {
    Command interface{} `json:"command"`
}

type ClientCommandReply struct {
    Index int `json:"index"`
}

func NewRaftNode(id string, peers []string) *RaftNode {
    r := &RaftNode{
        id:                  id,
        peerAddresses:       peers,
        currentTerm:         0,
        votedFor:            nil,
        log:                 make([]LogEntry, 0),
        commitIndex:         0,
        lastApplied:         0,
        nextIndex:           make(map[string]int),
        matchIndex:          make(map[string]int),
        state:               Follower,
        electionTimeout:     make(chan struct{}),
        stopChan:            make(chan struct{}),
    }
    return r
}

func (r *RaftNode) Start() {
    go r.runElectionTimer()
    go r.runHeartbeatTimer()
}

func (r *RaftNode) Stop() {
    close(r.stopChan)
}

func (r *RaftNode) resetElectionTimer() {
    select {
    case r.electionTimeout <- struct{}{}:
    default:
    }
}

func (r *RaftNode) runElectionTimer() {
    for {
        select {
        case <-r.stopChan:
            return
        case <-r.electionTimeout:
        case <-time.After(time.Duration(rand.Intn(150)+150) * time.Millisecond):
            if r.state == Follower {
                // Become candidate
                r.startElection()
            } else if r.state == Candidate {
                // Candidate timed out: start a new election
                r.startElection()
            }
        }
    }
}

func (r *RaftNode) startElection() {
    r.state = Candidate
    r.currentTerm++
    r.votedFor = &r.id

    log.Printf("[Node %s] Starting election for term %d\n", r.id, r.currentTerm)

    // Send RequestVote RPC to all peers
    votesReceived := 1 // vote for self
    for _, peer := range r.peerAddresses {
        go func(peerAddr string) {
            args := RequestVoteArgs{
                Term:         r.currentTerm,
                CandidateID:  r.id,
                LastLogIndex: len(r.log) - 1,
                LastLogTerm:  r.lastLogTerm(),
            }
            reply, err := r.sendRequestVote(peerAddr, &args)
            if err != nil {
                return
            }
            r.mu.Lock()
            defer r.mu.Unlock()

            // If the term changed while we were waiting, ignore
            if reply.Term > r.currentTerm {
                // This means we discovered a higher term; revert to follower
                r.currentTerm = reply.Term
                r.state = Follower
                r.votedFor = nil
                return
            }

            if r.state != Candidate || r.currentTerm != args.Term {
                // stale reply
                return
            }

            if reply.VoteGranted {
                votesReceived++
                // Check if we have a majority
                if votesReceived > (len(r.peerAddresses)+1)/2 {
                    // We become the leader
                    r.becomeLeader()
                }
            }
        }(peer)
    }
}

func (r *RaftNode) becomeLeader() {
    log.Printf("[Node %s] Becoming Leader for term %d\n", r.id, r.currentTerm)
    r.state = Leader
    // Initialize nextIndex and matchIndex for each peer
    for _, peer := range r.peerAddresses {
        r.nextIndex[peer] = len(r.log)
        r.matchIndex[peer] = -1
    }

    // Send heartbeats immediately (AppendEntries calls).
    r.broadcastHeartbeats()
}

func (r *RaftNode) broadcastHeartbeats() {
    if r.state != Leader {
        return
    }
    for _, peer := range r.peerAddresses {
        go func(peerAddr string) {
            args := AppendEntriesArgs{
                Term:         r.currentTerm,
                LeaderID:     r.id,
                PrevLogIndex: len(r.log) - 1,
                PrevLogTerm:  r.lastLogTerm(),
                Entries:      []LogEntry{}, // empty for heartbeat
                LeaderCommit: r.commitIndex,
            }
            r.sendAppendEntries(peerAddr, &args)
        }(peer)
    }
}

func (r *RaftNode) runHeartbeatTimer() {
    ticker := time.NewTicker(50 * time.Millisecond)
    defer ticker.Stop()

    for {
        select {
            case <-ticker.C:
                r.mu.Lock()
                if r.state == Leader {                        
                    r.broadcastHeartbeats()
                }
                r.mu.Unlock()
            case <-r.stopChan:
                return
        }
    }
}

func (r *RaftNode) HandleRequestVote(w http.ResponseWriter, req *http.Request) {
    var args RequestVoteArgs
    if err := json.NewDecoder(req.Body).Decode(&args); err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }
    r.mu.Lock()
    defer r.mu.Unlock()

    reply := RequestVoteReply{
        Term:        r.currentTerm,
        VoteGranted: false,
    }

    if r.state == Candidate {
        r.state = Follower
    }

    if args.Term < r.currentTerm {
        // We already have a higher term, reject
        writeJSON(w, reply)
        return
    }

    // If the incoming term is newer, update currentTerm and become follower
    if args.Term > r.currentTerm {
        r.currentTerm = args.Term
        r.state = Follower
        r.votedFor = nil
    }

    if (r.votedFor == nil || *r.votedFor == args.CandidateID) && r.isCandidateLogUpToDate(args.LastLogIndex, args.LastLogTerm) {
        r.votedFor = &args.CandidateID
        reply.VoteGranted = true
        // Reset election timer since we granted a vote
        r.resetElectionTimer()
    }

    writeJSON(w, reply)
}

func (r *RaftNode) isCandidateLogUpToDate(lastLogIndex, lastLogTerm int) bool {
    ourLastTerm := r.lastLogTerm()
    if lastLogTerm != ourLastTerm {
        return lastLogTerm >= ourLastTerm
    }
    // If same term, compare length
    return lastLogIndex >= (len(r.log) - 1)
}

func (r *RaftNode) lastLogTerm() int {
    if len(r.log) == 0 {
        return -1 // no entries
    }
    return r.log[len(r.log)-1].Term
}

func (r *RaftNode) sendRequestVote(peerAddr string, args *RequestVoteArgs) (*RequestVoteReply, error) {
    url := fmt.Sprintf("http://%s/raft/requestvote", peerAddr)

    data, _ := json.Marshal(args)
    resp, err := http.Post(url, "application/json", strings.NewReader(string(data)))
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

    var reply RequestVoteReply
    if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
        return nil, err
    }
    return &reply, nil
}

func (r *RaftNode) HandleAppendEntries(w http.ResponseWriter, req *http.Request) {
    var args AppendEntriesArgs
    if err := json.NewDecoder(req.Body).Decode(&args); err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }

    r.mu.Lock()
    defer r.mu.Unlock()

    reply := AppendEntriesReply{
        Term:    r.currentTerm,
        Success: false,
    }

    // If leader's term is behind, reject
    if args.Term < r.currentTerm {
        writeJSON(w, reply)
        return
    }

    // If the incoming term is newer, update currentTerm and become follower
    if args.Term > r.currentTerm {
        r.currentTerm = args.Term
        r.state = Follower
        r.votedFor = nil
    }

    // This is a valid heartbeat or replication request from the leader.
    r.resetElectionTimer()

    // Check if we have PrevLogIndex in our log with matching term?
    if args.PrevLogIndex >= 0 {
        if args.PrevLogIndex >= len(r.log) {
            // We don't have this index yet
            writeJSON(w, reply)
            return
        }
        if r.log[args.PrevLogIndex].Term != args.PrevLogTerm {
            // Log mismatch
            writeJSON(w, reply)
            return
        }
    }

    // Truncate the log if there is a conflict
    if args.PrevLogIndex+1 < len(r.log) {
        r.log = r.log[:args.PrevLogIndex+1]
    }

    // Append new entries
    r.log = append(r.log, args.Entries...)

    // Update commitIndex
    if args.LeaderCommit > r.commitIndex {
        r.commitIndex = min(args.LeaderCommit, len(r.log)-1)
    }

    // If commitIndex > lastApplied: increment lastApplied, apply
    // log[lastApplied] to state machine. The lastApplied index is the index
    // of highest log entry applied to state machine
    for r.lastApplied < r.commitIndex {
        r.lastApplied++
    }

    reply.Success = true
    writeJSON(w, reply)
}

func (r *RaftNode) sendAppendEntries(peerAddr string, args *AppendEntriesArgs) (*AppendEntriesReply, error) {
    url := fmt.Sprintf("http://%s/raft/appendentries", peerAddr)

    data, _ := json.Marshal(args)
    resp, err := http.Post(url, "application/json", strings.NewReader(string(data)))
    if err != nil {
        return nil, err
    }
    defer resp.Body.Close()

    var reply AppendEntriesReply
    if err := json.NewDecoder(resp.Body).Decode(&reply); err != nil {
        return nil, err
    }
    return &reply, nil
}

func writeJSON(w http.ResponseWriter, v interface{}) {
    w.Header().Set("Content-Type", "application/json")
    if err := json.NewEncoder(w).Encode(v); err != nil {
        http.Error(w, err.Error(), http.StatusInternalServerError)
    }
}

func (r *RaftNode) HandleClientCommand(w http.ResponseWriter, req *http.Request) {
    var args ClientCommandArgs
    if err := json.NewDecoder(req.Body).Decode(&args); err != nil {
        http.Error(w, err.Error(), http.StatusBadRequest)
        return
    }

    r.mu.Lock()
    defer r.mu.Unlock()

    // If not leader, reject the request
    if r.state != Leader {
        http.Error(w, "Not the leader", http.StatusServiceUnavailable)
        return
    }

    // Add client command to the local log
    entry := LogEntry{
        Term:    r.currentTerm,
        Command: args.Command,
    }
    r.log = append(r.log, entry)
    newIndex := len(r.log) - 1

    // Replicate new command to all followers
    if err := r.replicateAndCommit(newIndex); err != nil {
        http.Error(w, "Failed to replicate log entry", http.StatusInternalServerError)
        return
    }

    // Once majority of the followers have committed the entry, reply to the client
    writeJSON(w, ClientCommandReply{Index: newIndex})
}

func (r *RaftNode) replicateAndCommit(index int) error {
    majority := (len(r.peerAddresses)+1)/2 + 0 // 3 nodes => majority is 2
    // Leader node is already a match
    matchedCount := 1

    for _, peer := range r.peerAddresses {
        for {
            nextIdx := r.nextIndex[peer]
            if index < nextIdx {
                break
            }

            entries := r.log[nextIdx:]
            prevLogIndex := nextIdx - 1
            prevLogTerm := -1
            if prevLogIndex >= 0 && prevLogIndex < len(r.log) {
                prevLogTerm = r.log[prevLogIndex].Term
            }

            args := AppendEntriesArgs{
                Term:         r.currentTerm,
                LeaderID:     r.id,
                PrevLogIndex: prevLogIndex,
                PrevLogTerm:  prevLogTerm,
                Entries:      entries,
                LeaderCommit: r.commitIndex,
            }

            r.mu.Unlock()
            reply, err := r.sendAppendEntries(peer, &args)
            r.mu.Lock()

            if err != nil {
                log.Printf("[WARN] sendAppendEntries error to %s: %v", peer, err)
                break
            }

            if reply.Term > r.currentTerm {
                r.currentTerm = reply.Term
                r.state = Follower
                r.votedFor = nil
                return fmt.Errorf("stepped down from leadership; higher term discovered")
            }

            if !reply.Success {
                r.nextIndex[peer] = max(0, r.nextIndex[peer]-1)
                continue
            }

            lastAppended := nextIdx + len(entries) - 1
            r.nextIndex[peer] = lastAppended + 1
            r.matchIndex[peer] = lastAppended

            if lastAppended >= index {
                matchedCount++
            }

            break // Move to next peer
        }
    }

    if matchedCount >= majority && r.log[index].Term == r.currentTerm {
        r.commitIndex = index

        // Apply all newly committed entries to the state machine:
        for r.lastApplied < r.commitIndex {
            r.lastApplied++
            // For illustration implementation, we do nothing and log a message:
            log.Printf("[Leader %s] Applying entry at index %d: %+v",
                r.id, r.lastApplied, r.log[r.lastApplied])
        }
    }

    return nil
}


func main() {
    var (
        nodeID  string
        peers   string
        addr    string
    )

    flag.StringVar(&nodeID, "id", "node1", "Unique ID of this node")
    flag.StringVar(&peers, "peers", "", "Comma-separated list of other node addresses (host:port)")
    flag.StringVar(&addr, "addr", "127.0.0.1:8000", "Address to listen on")
    flag.Parse()

    if nodeID == "" {
        fmt.Println("Must provide -id for this node!")
        os.Exit(1)
    }

    var peerSlice []string
    if peers != "" {
        peerSlice = strings.Split(peers, ",")
    }

    raftNode := NewRaftNode(nodeID, peerSlice)
    // Start raft background processes
    raftNode.Start()
    defer raftNode.Stop()

    r := mux.NewRouter()

    // Raft RPC endpoints
    r.HandleFunc("/raft/requestvote", raftNode.HandleRequestVote).Methods("POST")
    r.HandleFunc("/raft/appendentries", raftNode.HandleAppendEntries).Methods("POST")

    // Client command endpoint
    r.HandleFunc("/client/commands", raftNode.HandleClientCommand).Methods("POST")

    // Debug state check endpoint
    r.HandleFunc("/raft/state", func(w http.ResponseWriter, req *http.Request) {
        raftNode.mu.Lock()
        defer raftNode.mu.Unlock()
        state := map[string]interface{}{
            "id":           raftNode.id,
            "term":         raftNode.currentTerm,
            "votedFor":     raftNode.votedFor,
            "state":        raftNode.state,
            "commitIndex":  raftNode.commitIndex,
            "lastApplied":  raftNode.lastApplied,
            "log":          raftNode.log,
            "peerAddresses": raftNode.peerAddresses,
        }
        writeJSON(w, state)
    }).Methods("GET")

    log.Printf("Raft node %s starting on %s, peers: %v\n", nodeID, addr, peerSlice)
    if err := http.ListenAndServe(addr, r); err != nil {
        log.Fatalf("ListenAndServe failed: %v\n", err)
    }
}