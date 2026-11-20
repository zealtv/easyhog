package core

import (
	"sync"

	"github.com/zealtv/easyhog/internal/groundhog"
	"github.com/zealtv/easyhog/internal/runner"
)

// Clients holds one groundhog.Client per instance, so the refresher and the
// ticker share each instance's capability cache and its default bounds. A
// caller that wants a shorter bound for its calls passes it per call
// (groundhog.WithTimeout); the client's own default stays the longer one.
// Clients are never dropped: an entry is a path and a few words, and an
// instance that vanishes and returns keeps the one it had. Safe for concurrent
// use.
type Clients struct {
	exec runner.Executor

	mu sync.Mutex
	m  map[string]*groundhog.Client
}

// NewClients builds an empty pool whose clients run scripts through exec.
func NewClients(exec runner.Executor) *Clients {
	return &Clients{exec: exec, m: map[string]*groundhog.Client{}}
}

// For returns the instance's client, making it on first use. instance is an
// Instance.Path (canonical, absolute).
func (c *Clients) For(instance string) (*groundhog.Client, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if cl := c.m[instance]; cl != nil {
		return cl, nil
	}
	cl, err := groundhog.New(c.exec, instance)
	if err != nil {
		return nil, err
	}
	c.m[instance] = cl
	return cl, nil
}
