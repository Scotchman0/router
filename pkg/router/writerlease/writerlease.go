package writerlease

import (
	"fmt"
	"math/rand"
	"sync"
	"time"

	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/util/workqueue"

	logf "github.com/openshift/router/log"
)

var log = logf.Logger.WithName("writerlease")

// Lease performs the equivalent of leader election by competing to perform work (such as
// updating a contended resource). Every successful work unit is considered a lease renewal,
// while work that is observed from others or that fails is treated as renewing another processes
// lease. When a lease expires (no work is detected within the lease term) the writer competes
// to perform work. When competing for the lease, exponential backoff is used.
type Lease interface {
	// Wait waits for the first work function to complete and then returns whether the current
	// process is the leader. This function will block forever if no work has been requested or if the
	// work retries forever.
	Wait() bool
	// WaitUntil waits at most the provided duration for the frist work function to complete.
	// If the duration expires without work completing it will return false for expired, otherwise
	// it will return whether the lease is held by this process.
	WaitUntil(t time.Duration) (leader bool, ok bool)
	// Try runs the provided function when the lease is held is the leader. It retries work until
	// the work func indicates retry is not necessary.
	Try(key WorkKey, fn WorkFunc)
	// Extend indicates that the caller has observed another writer performing work against
	// the specified key. This will clear the work remaining for the lease and extend the lease
	// interval.
	Extend(key WorkKey)
	// Remove clears any pending work for the provided key.
	Remove(key WorkKey)
}

// WorkFunc is a retriable unit of work. It should return an error if the work couldn't be
// completed successfully, or true if we can assume our lease has been extended. If the
// lease could not be extended, we drop this unit of work.
type WorkFunc func() (result WorkResult, retry bool)

type WorkResult int

type WorkKey string

const (
	None WorkResult = iota
	Extend
	Release
)

// LimitRetries allows a work function to be retried up to retries times.
func LimitRetries(retries int, fn WorkFunc) WorkFunc {
	i := 0
	return func() (WorkResult, bool) {
		extend, retry := fn()
		if retry {
			retry = i < retries
			i++
		}
		return extend, retry
	}
}

// State is the state of the lease.
type State int

const (
	// Election is before a work unit has been completed.
	Election State = iota
	Leader
	Follower
)

type work struct {
	id int
	fn WorkFunc
}

type WriterLease struct {
	name          string
	backoff       wait.Backoff
	maxBackoff    time.Duration
	retryInterval time.Duration
	once          chan struct{}
	nowFn         func() time.Time

	lock    sync.Mutex
	id      int
	queued  map[WorkKey]*work
	queue   workqueue.DelayingInterface
	state   State
	expires time.Time
	tick    int
}

// New creates a new Lease. Specify the duration to hold leases for and the retry
// interval on requests that fail.
func New(leaseDuration, retryInterval time.Duration) *WriterLease {
	backoff := wait.Backoff{
		Duration: 20 * time.Millisecond,
		Factor:   4,
		Steps:    5,
		Jitter:   0.5,
	}

	return &WriterLease{
		name:          fmt.Sprintf("%08d", rand.Int31()),
		backoff:       backoff,
		maxBackoff:    leaseDuration,
		retryInterval: retryInterval,

		nowFn:  time.Now,
		queued: make(map[WorkKey]*work),
		queue:  workqueue.NewDelayingQueue(),
		once:   make(chan struct{}),
	}
}

// NewWithBackoff creates a new Lease. Specify the duration to hold leases for and the retry
// interval on requests that fail.
func NewWithBackoff(name string, leaseDuration, retryInterval time.Duration, backoff wait.Backoff) *WriterLease {
	return &WriterLease{
		name:          name,
		backoff:       backoff,
		maxBackoff:    leaseDuration,
		retryInterval: retryInterval,

		nowFn:  time.Now,
		queued: make(map[WorkKey]*work),
		queue:  workqueue.NewNamedDelayingQueue(name),
		once:   make(chan struct{}),
	}
}

func (l *WriterLease) Run(stopCh <-chan struct{}) {
	defer utilruntime.HandleCrash()
	defer l.queue.ShutDown()

	go func() {
		defer utilruntime.HandleCrash()
		for l.work() {
		}
		log.V(4).Info("worker stopped", "worker", l.name)
	}()

	<-stopCh
}

func (l *WriterLease) Expire() {
	l.lock.Lock()
	defer l.lock.Unlock()
	l.expires = time.Time{}
}

func (l *WriterLease) Wait() bool {
	<-l.once
	state, _, _ := l.leaseState()
	return state == Leader
}

func (l *WriterLease) WaitUntil(t time.Duration) (bool, bool) {
	select {
	case <-l.once:
	case <-time.After(t):
		return false, false
	}
	state, _, _ := l.leaseState()
	return state == Leader, true
}

func (l *WriterLease) Try(key WorkKey, fn WorkFunc) {
	l.lock.Lock()
	defer l.lock.Unlock()
	l.id++
	l.queued[key] = &work{fn: fn, id: l.id}
	if l.state == Follower {
		delay := l.expires.Sub(l.nowFn())
		// no matter what, always wait at least some amount of time as a follower to give the nominal
		// leader a chance to win
		if delay < l.backoff.Duration*2 {
			delay = l.backoff.Duration * 2
		}
		l.queue.AddAfter(key, delay)
	} else {
		l.queue.Add(key)
	}
}

func (l *WriterLease) Extend(key WorkKey) {
	l.lock.Lock()
	defer l.lock.Unlock()
	if _, ok := l.queued[key]; ok {
		delete(l.queued, key)
		switch l.state {
		case Follower:
			l.tick++
			backoff := l.nextBackoff()
			log.V(4).Info("clearing work and extending lease", "worker", l.name, "key", key, "duration", backoff)
			l.expires = l.nowFn().Add(backoff)
		}
	}
}

func (l *WriterLease) Len() int {
	l.lock.Lock()
	defer l.lock.Unlock()
	return len(l.queued)
}

func (l *WriterLease) Remove(key WorkKey) {
	l.lock.Lock()
	defer l.lock.Unlock()
	delete(l.queued, key)
}

func (l *WriterLease) get(key WorkKey) *work {
	l.lock.Lock()
	defer l.lock.Unlock()
	return l.queued[key]
}

func (l *WriterLease) leaseState() (State, time.Time, int) {
	l.lock.Lock()
	defer l.lock.Unlock()
	return l.state, l.expires, l.tick
}

func (l *WriterLease) work() bool {
	item, shutdown := l.queue.Get()
	if shutdown {
		return false
	}
	key := item.(WorkKey)

	work := l.get(key)
	if work == nil {
		log.V(4).Info("work item was cleared, done", "worker", l.name, "key", key)
		l.queue.Done(key)
		return true
	}

	leaseState, leaseExpires, _ := l.leaseState()
	if leaseState == Follower {
		// if we are following, continue to defer work until the lease expires
		if remaining := leaseExpires.Sub(l.nowFn()); remaining > 0 {
			log.V(4).Info("follower awaiting lease expiration", "worker", l.name, "key", key, "leaseTimeRemaining", remaining)
			time.Sleep(remaining)
			l.queue.Add(key)
			l.queue.Done(key)
			return true
		}
		log.V(4).Info("lease expired, running", "worker", l.name, "key", key)
	} else {
		log.V(4).Info("lease owner or electing, running", "worker", l.name, "key", key)
	}

	result, retry := work.fn()
	if retry {
		l.retryKey(key, result)
		return true
	}
	l.finishKey(key, result, work.id)
	return true
}

// retryKey schedules the key for a retry in the future.
func (l *WriterLease) retryKey(key WorkKey, result WorkResult) {
	l.lock.Lock()
	defer l.lock.Unlock()

	l.nextState(result)
	l.queue.AddAfter(key, l.retryInterval)
	l.queue.Done(key)

	log.V(4).Info("retrying work", "worker", l.name, "key", key, "state", l.state, "tick", l.tick, "expires", l.expires)
}

func (l *WriterLease) finishKey(key WorkKey, result WorkResult, id int) {
	l.lock.Lock()
	defer l.lock.Unlock()

	l.nextState(result)
	if work, ok := l.queued[key]; ok && work.id == id {
		delete(l.queued, key)
	}
	l.queue.Done(key)
	log.V(4).Info("completed work", "worker", l.name, "key", key, "state", l.state, "tick", l.tick, "expires", l.expires)
}

// nextState must be called while holding the lock.
func (l *WriterLease) nextState(result WorkResult) {
	resolvedElection := l.state == Election
	switch result {
	case Extend:
		switch l.state {
		case Election, Follower:
			l.tick = 0
			l.state = Leader
			log.V(4).Info("state change: elected to leader", "worker", l.name)
		}
		l.expires = l.nowFn().Add(l.maxBackoff)
	case Release:
		switch l.state {
		case Election, Leader:
			l.tick = 0
			l.state = Follower
			log.V(4).Info("state change: demoted to follower", "worker", l.name)
		case Follower:
			l.tick++
		}
		l.expires = l.nowFn().Add(l.nextBackoff())
	default:
		resolvedElection = false
	}
	// close the channel before we remove the key from the queue to prevent races in Wait
	if resolvedElection {
		close(l.once)
	}
}

func (l *WriterLease) nextBackoff() time.Duration {
	step := l.tick
	b := l.backoff
	if step > b.Steps {
		return l.maxBackoff
	}
	duration := b.Duration
	for i := 0; i < step; i++ {
		adjusted := duration
		if b.Jitter > 0.0 {
			adjusted = wait.Jitter(duration, b.Jitter)
		}
		duration = time.Duration(float64(adjusted) * b.Factor)
		if duration > l.maxBackoff {
			return l.maxBackoff
		}
	}
	return duration
}
