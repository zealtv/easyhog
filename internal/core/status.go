package core

import (
	"context"
	"sync"
	"time"

	"github.com/zealtv/easyhog/internal/groundhog"
	"github.com/zealtv/easyhog/internal/runner"
)

// Clients holds one groundhog.Client per instance, so the refresher and the
// supervisor share each instance's capability cache and its default bounds. A
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

// readPolicy bounds one coherent read.
type readPolicy struct {
	// Tries is the most reads to make, at least one.
	Tries int
	// Delay is the wait between two tries; Wait performs it and returns an error
	// when ctx ends first.
	Delay time.Duration
	Wait  func(ctx context.Context, d time.Duration) error
	// RetryBusy makes a busy script (exit 4) cost one try instead of ending the
	// read at once.
	RetryBusy bool
	// Opts are passed to every `status` call, for a per-call timeout.
	Opts []groundhog.Option
}

// readCoherent is the one place that reads a coherent status: it reads
// `status`, and re-reads while the answer is inconsistent (or, with RetryBusy,
// while the script is busy), up to Tries reads. It returns the last status
// read, how many reads answered, and whether that status is coherent.
//
//   - A coherent status: ok is true, err nil.
//   - Tries used up on statuses that stayed inconsistent: ok false, err nil and
//     the last status returned, for a caller that shows it marked (the
//     refresher) or refuses it (the supervisor).
//   - Any other failure, or Tries used up while the script was busy: err set.
func readCoherent(ctx context.Context, c *groundhog.Client, p readPolicy) (st groundhog.Status, reads int, ok bool, err error) {
	var last error // the newest try's failure; nil when it answered
	for i := 0; i < max(p.Tries, 1); i++ {
		if i > 0 && p.Wait(ctx, p.Delay) != nil {
			break
		}
		st, err = c.Status(ctx, p.Opts...)
		switch {
		case err == nil:
			reads++
			if st.Consistent { // the script says no writer was caught mid-update
				return st, reads, true, nil
			}
			last = nil
		case p.RetryBusy && groundhog.IsRetryable(err):
			last = err
		default:
			return groundhog.Status{}, reads, false, err
		}
	}
	if last != nil {
		return groundhog.Status{}, reads, false, last
	}
	if reads == 0 { // the context ended before any read
		return groundhog.Status{}, 0, false, ctx.Err()
	}
	return st, reads, false, nil
}
