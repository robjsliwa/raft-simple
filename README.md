# Simple Raft Implementation in Go

This repository contains a simple Raft consensus implementation in Go, covered in the article:
Learning Raft Consensus Algorithm (Placeholder: Replace with actual link)

# 📖 About

Raft is a distributed consensus algorithm designed to be easier to understand than Paxos. It ensures a replicated state machine across multiple nodes. This implementation provides:
- Leader Election 🏆 - Nodes elect a leader when one is unavailable.
- Log Replication 📜 - The leader replicates logs to followers.
- Client Command Handling 🎮 - The leader accepts commands and ensures they are applied to all nodes.

This implementation is not production-ready but serves as an educational tool to understand the fundamentals of Raft.

# 🚀 Getting Started

## Prerequisites
- Go (1.18+ recommended)

## 📦 Installation

Clone the repository:

```bash
git clone https://github.com/your-username/raft-simple.git
cd raft-simple
```

Build the Raft node executable:

```bash
go build -o raft-node main.go
```

## 🏃 Running the Raft Cluster

You can start multiple Raft nodes on different terminals.

Starting 3 Raft Nodes

Each node requires:
- A unique ID
- Its own address
- A list of peer nodes

Start Node 1

```bash
./raft-node -id=node1 -addr=127.0.0.1:8001 -peers=127.0.0.1:8002,127.0.0.1:8003
```

Start Node 2

```bash
./raft-node -id=node2 -addr=127.0.0.1:8002 -peers=127.0.0.1:8001,127.0.0.1:8003
```

Start Node 3

```bash
./raft-node -id=node3 -addr=127.0.0.1:8003 -peers=127.0.0.1:8001,127.0.0.1:8002
```

Once the nodes are running, they will elect a leader automatically.

## 🔍 Checking Node Status

Each node provides an endpoint to check its state.

Query the Raft State

Run:

```bash
curl http://127.0.0.1:8001/raft/state | jq
```

Example output:
```json
{
  "id": "node1",
  "term": 5,
  "state": "Leader",
  "commitIndex": 3,
  "lastApplied": 3,
  "log": [
    {"term": 3, "command": "test1"},
    {"term": 3, "command": "test2"}
  ],
  "peerAddresses": ["127.0.0.1:8002", "127.0.0.1:8003"]
}
```

## ✨ Sending Commands to Raft

Only the leader accepts commands. If a client sends a command to a follower, it will be rejected.

Send a Command

```bash
curl -X POST http://127.0.0.1:8001/client/commands -d '{"command": "test1"}'
```

Response:

```json
{"index":0}
```

Verify Log Replication

Run:

```bash
curl http://127.0.0.1:8002/raft/state | jq
```

You should see "command": "test1" replicated in the logs.

