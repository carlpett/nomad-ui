package main

import (
	"fmt"
	"time"

	"gopkg.in/fatih/set.v0"

	"github.com/gorilla/websocket"
	"github.com/hashicorp/nomad/api"
)

// Connection monitors the websocket connection. It processes any action
// received on the websocket and sends out actions on Nomad state changes. It
// maintains a set to keep track of the running watches.
type Connection struct {
	socket *websocket.Conn

	hub *Hub

	receive chan *Action
	send    chan *Action

	destroyCh chan struct{}

	watches *set.Set
}

// NewConnection creates a new connection.
func NewConnection(hub *Hub, socket *websocket.Conn) *Connection {
	return &Connection{
		watches:   set.New(),
		hub:       hub,
		socket:    socket,
		receive:   make(chan *Action),
		send:      make(chan *Action),
		destroyCh: make(chan struct{}),
	}
}

func (c *Connection) writePump() {
	defer func() {
		c.socket.Close()
	}()
	for {
		action, ok := <-c.send
		if !ok {
			if err := c.socket.WriteMessage(websocket.CloseMessage, []byte{}); err != nil {
				logger.Errorf("Could not write close message to websocket: %s", err)
			}
			return
		}
		if err := c.socket.WriteJSON(action); err != nil {
			logger.Errorf("Could not write action to websocket: %s", err)
		}
	}
}

func (c *Connection) readPump() {
	defer func() {
		c.watches.Clear()
		c.hub.unregister <- c
		c.socket.Close()
	}()

	// Register this connection with the hub for broadcast updates
	c.hub.register <- c

	// Flush all current state to the websocket
	c.hub.nomad.FlushAll(c)

	var action Action
	for {
		err := c.socket.ReadJSON(&action)
		if err != nil {
			break
		}
		c.process(action)
	}
}

func (c *Connection) process(action Action) {
	switch action.Type {
	case fetchMember:
		c.fetchMember(action)
	case fetchNode:
		c.fetchNode(action)
	case fetchDir:
		c.fetchDir(action)
	case watchJob:
		go c.watchJob(action)
	case watchAlloc:
		go c.watchAlloc(action)
	case watchEval:
		go c.watchEval(action)
	case watchMember:
		go c.watchMember(action)
	case watchNode:
		go c.watchNode(action)
	case watchFile:
		go c.watchFile(action)
	case unwatchEval:
		fallthrough
	case unwatchMember:
		fallthrough
	case unwatchNode:
		fallthrough
	case unwatchJob:
		fallthrough
	case unwatchAlloc:
		c.watches.Remove(action.Payload.(string))
	case unwatchFile:
		c.watches.Remove(action.Payload.(string))
	}
}

// Handle monitors the websocket connection for incoming actions. It sends
// out actions on state changes.
func (c *Connection) Handle() {
	go c.writePump()
	c.readPump()

	// Kill any remaining watcher routines
	close(c.destroyCh)
}

func (c *Connection) watchAlloc(action Action) {
	allocID := action.Payload.(string)

	defer func() {
		c.watches.Remove(allocID)
		logger.Infof("Stopped watching alloc with id: %s", allocID)
	}()
	c.watches.Add(allocID)

	logger.Infof("Started watching alloc with id: %s", allocID)

	q := &api.QueryOptions{WaitIndex: 1}
	for {
		select {
		case <-c.destroyCh:
			return
		default:
			alloc, meta, err := c.hub.nomad.Client.Allocations().Info(allocID, q)
			if err != nil {
				logger.Errorf("connection: unable to fetch alloc info: %s", err)
				time.Sleep(10 * time.Second)
				continue
			}
			if !c.watches.Has(allocID) {
				return
			}
			c.send <- &Action{Type: fetchedAlloc, Payload: alloc}

			waitIndex := meta.LastIndex
			if q.WaitIndex > meta.LastIndex {
				waitIndex = q.WaitIndex
			}
			q = &api.QueryOptions{WaitIndex: waitIndex, WaitTime: 10 * time.Second}
		}
	}
}

func (c *Connection) watchEval(action Action) {
	evalID := action.Payload.(string)

	defer func() {
		c.watches.Remove(evalID)
		logger.Infof("Stopped watching eval with id: %s", evalID)
	}()
	c.watches.Add(evalID)

	logger.Infof("Started watching eval with id: %s", evalID)

	q := &api.QueryOptions{WaitIndex: 1}
	for {
		select {
		case <-c.destroyCh:
			return
		default:
			eval, meta, err := c.hub.nomad.Client.Evaluations().Info(evalID, q)
			if err != nil {
				logger.Errorf("connection: unable to fetch eval info: %s", err)
				time.Sleep(10 * time.Second)
				continue
			}
			if !c.watches.Has(evalID) {
				return
			}
			c.send <- &Action{Type: fetchedEval, Payload: eval}

			waitIndex := meta.LastIndex
			if q.WaitIndex > meta.LastIndex {
				waitIndex = q.WaitIndex
			}
			q = &api.QueryOptions{WaitIndex: waitIndex, WaitTime: 10 * time.Second}
		}
	}
}

func (c *Connection) fetchMember(action Action) {
	memberID := action.Payload.(string)
	member, err := c.hub.nomad.MemberWithID(memberID)
	if err != nil {
		logger.Errorf("websocket: unable to fetch member %q: %s", memberID, err)
		return
	}

	c.send <- &Action{Type: fetchedMember, Payload: member}
}

func (c *Connection) watchMember(action Action) {
	memberID := action.Payload.(string)

	defer func() {
		c.watches.Remove(memberID)
		logger.Infof("Stopped watching member with id: %s", memberID)
	}()
	c.watches.Add(memberID)

	logger.Infof("Started watching member with id: %s", memberID)

	for {
		select {
		case <-c.destroyCh:
			return
		default:
			member, err := c.hub.nomad.MemberWithID(memberID)
			if err != nil {
				logger.Errorf("connection: unable to fetch member info: %s", err)
				time.Sleep(10 * time.Second)
				continue
			}
			if !c.watches.Has(memberID) {
				return
			}
			c.send <- &Action{Type: fetchedMember, Payload: member}

			time.Sleep(10 * time.Second)
		}
	}
}

func (c *Connection) fetchNode(action Action) {
	nodeID := action.Payload.(string)
	node, _, err := c.hub.nomad.Client.Nodes().Info(nodeID, nil)
	if err != nil {
		logger.Errorf("websocket: unable to fetch node %q: %s", nodeID, err)
	}
	c.send <- &Action{Type: fetchedNode, Payload: node}
}

func (c *Connection) watchNode(action Action) {
	nodeID := action.Payload.(string)

	defer func() {
		c.watches.Remove(nodeID)
		logger.Infof("Stopped watching node with id: %s", nodeID)
	}()
	c.watches.Add(nodeID)

	logger.Infof("Started watching node with id: %s", nodeID)

	q := &api.QueryOptions{WaitIndex: 1}
	for {
		select {
		case <-c.destroyCh:
			return
		default:
			node, meta, err := c.hub.nomad.Client.Nodes().Info(nodeID, q)
			if err != nil {
				logger.Errorf("connection: unable to fetch node info: %s", err)
				time.Sleep(10 * time.Second)
				continue
			}
			if !c.watches.Has(nodeID) {
				return
			}
			c.send <- &Action{Type: fetchedNode, Payload: node}

			waitIndex := meta.LastIndex
			if q.WaitIndex > meta.LastIndex {
				waitIndex = q.WaitIndex
			}
			q = &api.QueryOptions{WaitIndex: waitIndex, WaitTime: 10 * time.Second}
		}
	}
}

func (c *Connection) watchJob(action Action) {
	jobID := action.Payload.(string)

	defer func() {
		c.watches.Remove(jobID)
		logger.Infof("Stopped watching job with id: %s", jobID)
	}()
	c.watches.Add(jobID)

	logger.Infof("Started watching job with id: %s", jobID)

	q := &api.QueryOptions{WaitIndex: 1}
	for {
		select {
		case <-c.destroyCh:
			return
		default:
			job, meta, err := c.hub.nomad.Client.Jobs().Info(jobID, q)
			if err != nil {
				logger.Errorf("connection: unable to fetch job info: %s", err)
				time.Sleep(10 * time.Second)
				continue
			}
			if !c.watches.Has(jobID) {
				return
			}
			c.send <- &Action{Type: fetchedJob, Payload: job}

			waitIndex := meta.LastIndex
			if q.WaitIndex > meta.LastIndex {
				waitIndex = q.WaitIndex
			}
			q = &api.QueryOptions{WaitIndex: waitIndex, WaitTime: 10 * time.Second}
		}
	}
}

func (c *Connection) fetchDir(action Action) {
	params, ok := action.Payload.(map[string]interface{})
	if !ok {
		logger.Errorf("Could not decode payload")
		return
	}
	addr := params["addr"].(string)
	path := params["path"].(string)
	allocID := params["allocID"].(string)

	config := api.DefaultConfig()
	config.Address = fmt.Sprintf("http://%s", addr)

	client, err := api.NewClient(config)
	if err != nil {
		logger.Fatalf("Could not create client: %s", err)
		return
	}
	alloc, _, err := client.Allocations().Info(allocID, nil)
	if err != nil {
		logger.Errorf("Unable to fetch alloc: %s", err)
		return
	}
	dir, _, err := client.AllocFS().List(alloc, path, nil)
	if err != nil {
		logger.Errorf("Unable to fetch directory: %s", err)
	}

	c.send <- &Action{Type: fetchedDir, Payload: dir}
}

type Frame struct {
	Data   string
	File   string
	Offset int64
}

func (c *Connection) watchFile(action Action) {
	params, ok := action.Payload.(map[string]interface{})
	if !ok {
		logger.Error("Could not decode payload")
		return
	}
	addr := params["addr"].(string)
	path := params["path"].(string)
	allocID := params["allocID"].(string)

	config := api.DefaultConfig()
	config.Address = fmt.Sprintf("http://%s", addr)

	client, err := api.NewClient(config)
	if err != nil {
		logger.Errorf("Could not create client: %s", err)
		return
	}
	alloc, _, err := client.Allocations().Info(allocID, nil)
	if err != nil {
		logger.Errorf("Unable to fetch alloc: %s", err)
		return
	}

	cancel := make(chan struct{})
	frames, err := client.AllocFS().Stream(alloc, path, api.OriginStart, 0, cancel, nil)
	if err != nil {
		logger.Errorf("Unable to stream file: %s", err)
		return
	}

	c.watches.Add(path)
	defer func() {
		logger.Infof("Stopped watching file with path: %s", path)
		c.watches.Remove(path)
		close(cancel)
	}()

	logger.Infof("Started watching file with path: %s", path)
	ticker := time.NewTicker(10 * time.Second)
	for {
		select {
		case <-c.destroyCh:
			return
		case frame := <-frames:
			if !c.watches.Has(path) {
				return
			}
			c.send <- &Action{
				Type: fetchedFile,
				Payload: Frame{
					File:   frame.File,
					Data:   string(frame.Data),
					Offset: frame.Offset,
				},
			}
		case <-ticker.C:
			if !c.watches.Has(path) {
				return
			}
		}
	}
}
