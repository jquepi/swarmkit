package dispatcher

import (
	"errors"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"

	"github.com/docker/swarm-v2/api"
	"github.com/docker/swarm-v2/identity"
	"github.com/docker/swarm-v2/manager/dispatcher/heartbeat"
)

var errNodeMustDisconnect = errors.New("node should disconnect immediately")

type registeredNode struct {
	SessionID  string
	Heartbeat  *heartbeat.Heartbeat
	Node       *api.Node
	Disconnect chan struct{} // signal to disconnect
	mu         sync.Mutex
}

// checkSessionID determines if the SessionID has changed and returns the
// appropriate GRPC error code.
//
// This may not belong here in the future.
func (rn *registeredNode) checkSessionID(sessionID string) error {
	rn.mu.Lock()
	defer rn.mu.Unlock()

	// Before each message send, we need to check the nodes sessionID hasn't
	// changed. If it has, we will the stream and make the node
	// re-register.
	if rn.SessionID != sessionID {
		return grpc.Errorf(codes.InvalidArgument, ErrSessionInvalid.Error())
	}

	return nil
}

type nodeStore struct {
	periodChooser         *periodChooser
	gracePeriodMultiplier time.Duration
	nodes                 map[string]*registeredNode
	mu                    sync.RWMutex
}

func newNodeStore(hbPeriod, hbEpsilon time.Duration, graceMultiplier int) *nodeStore {
	return &nodeStore{
		nodes:                 make(map[string]*registeredNode),
		periodChooser:         newPeriodChooser(hbPeriod, hbEpsilon),
		gracePeriodMultiplier: time.Duration(graceMultiplier),
	}
}

// Add adds new node and returns it, it replaces existing without notification.
func (s *nodeStore) Add(n *api.Node, expireFunc func()) *registeredNode {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existRn, ok := s.nodes[n.ID]; ok {
		existRn.Heartbeat.Stop()
		delete(s.nodes, n.ID)
	}
	rn := &registeredNode{
		SessionID:  identity.NewID(), // session ID is local to the dispatcher.
		Node:       n,
		Disconnect: make(chan struct{}),
	}
	s.nodes[n.ID] = rn
	rn.Heartbeat = heartbeat.New(s.periodChooser.Choose()*s.gracePeriodMultiplier, expireFunc)
	return rn
}

func (s *nodeStore) Get(id string) (*registeredNode, error) {
	s.mu.RLock()
	rn, ok := s.nodes[id]
	s.mu.RUnlock()
	if !ok {
		return nil, grpc.Errorf(codes.NotFound, ErrNodeNotRegistered.Error())
	}
	return rn, nil
}

func (s *nodeStore) GetWithSession(id, sid string) (*registeredNode, error) {
	s.mu.RLock()
	rn, ok := s.nodes[id]
	s.mu.RUnlock()
	if !ok {
		return nil, grpc.Errorf(codes.NotFound, ErrNodeNotRegistered.Error())
	}
	return rn, rn.checkSessionID(sid)
}

func (s *nodeStore) Heartbeat(id, sid string) (time.Duration, error) {
	rn, err := s.GetWithSession(id, sid)
	if err != nil {
		return 0, err
	}
	period := s.periodChooser.Choose() // base period for node
	grace := period * time.Duration(s.gracePeriodMultiplier)
	rn.Heartbeat.Update(grace)
	rn.Heartbeat.Beat()
	return period, nil
}

func (s *nodeStore) Delete(id string) *registeredNode {
	s.mu.Lock()
	var node *registeredNode
	if rn, ok := s.nodes[id]; ok {
		delete(s.nodes, id)
		rn.Heartbeat.Stop()
		node = rn
	}
	s.mu.Unlock()
	return node
}

func (s *nodeStore) Disconnect(id string) {
	s.mu.Lock()
	if rn, ok := s.nodes[id]; ok {
		close(rn.Disconnect)
		rn.Heartbeat.Stop()
	}
	s.mu.Unlock()
}