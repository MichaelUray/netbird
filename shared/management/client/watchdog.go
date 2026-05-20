package client

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	log "github.com/sirupsen/logrus"

	proto "github.com/netbirdio/netbird/shared/management/proto"
)

// errWatchdogReconnect is the cancel-cause the streamWatchdog passes into
// a stream context. handleSyncStream and handleJobStream inspect
// context.Cause(ctx) before the generic ctx.Err()!=nil branch so they
// can return this error (retryable) instead of returning nil (which
// signals clean shutdown to withMgmtStream and ends the retry loop).
var errWatchdogReconnect = errors.New("management stream watchdog requested reconnect")

const (
	// watchdogProbeInterval is the cadence at which the watchdog issues
	// a GetServerKey probe to validate end-to-end mgmt reachability.
	watchdogProbeInterval = 60 * time.Second

	// watchdogProbeTimeout bounds each individual probe call.
	watchdogProbeTimeout = 5 * time.Second

	// watchdogFailureBudget is the number of consecutive probe failures
	// after which the watchdog cancels the in-flight stream context.
	// 3 * 60s = ~3 minutes of confirmed unreachability before action.
	watchdogFailureBudget = 3

	// watchdogReconnectBudget is the additional failure count beyond
	// watchdogFailureBudget after which the watchdog rebuilds the
	// ClientConn entirely (reconnectClientConn). 5 * 60s = ~5 minutes.
	watchdogReconnectBudget = 5
)

type streamWatchdog struct {
	c        *GrpcClient
	interval time.Duration
	timeout  time.Duration

	// Counters exposed via WatchdogStats(); persist for the lifetime
	// of the *GrpcClient.
	probeOk  atomic.Uint64
	probeErr atomic.Uint64
	tripped  atomic.Uint64

	ctx    context.Context
	cancel context.CancelFunc
	doneCh chan struct{}

	// startOnce + stopOnce make start()/stop() actually idempotent.
	startOnce sync.Once
	stopOnce  sync.Once
}

func newStreamWatchdog(c *GrpcClient) *streamWatchdog {
	ctx, cancel := context.WithCancel(c.ctx)
	return &streamWatchdog{
		c:        c,
		interval: watchdogProbeInterval,
		timeout:  watchdogProbeTimeout,
		ctx:      ctx,
		cancel:   cancel,
		doneCh:   make(chan struct{}),
	}
}

// start launches the probe loop. Idempotent: only the first call
// spawns the goroutine (subsequent calls are no-ops).
func (w *streamWatchdog) start() {
	w.startOnce.Do(func() {
		go w.run()
	})
}

// stop cancels the watchdog's context and blocks until the probe
// loop exits. Idempotent AFTER start: subsequent calls return
// immediately because doneCh has already been closed by run() exit.
func (w *streamWatchdog) stop() {
	w.stopOnce.Do(func() {
		w.cancel()
		<-w.doneCh
	})
}

func (w *streamWatchdog) run() {
	defer close(w.doneCh)
	t := time.NewTicker(w.interval)
	defer t.Stop()

	consecFailures := 0
	for {
		select {
		case <-w.ctx.Done():
			return
		case <-t.C:
			if w.probeOnce() {
				w.probeOk.Add(1)
				consecFailures = 0
				continue
			}
			w.probeErr.Add(1)
			consecFailures++
			switch {
			case consecFailures == watchdogFailureBudget:
				w.tripped.Add(1)
				w.c.cancelAllStreams(errWatchdogReconnect)
			case consecFailures == watchdogFailureBudget+watchdogReconnectBudget:
				log.Warnf("watchdog: %d probe failures, rebuilding ClientConn", consecFailures)
				if err := w.c.reconnectClientConn(w.ctx); err != nil {
					log.Errorf("watchdog reconnectClientConn: %v", err)
				} else {
					consecFailures = 0
				}
			}
		}
	}
}

func (w *streamWatchdog) probeOnce() bool {
	probeCtx, cancel := context.WithTimeout(w.ctx, w.timeout)
	defer cancel()

	_, client := w.c.snapshotConn()
	if client == nil {
		return false
	}

	_, err := client.GetServerKey(probeCtx, &proto.Empty{})
	if err != nil {
		log.Debugf("watchdog probe failed: %v", err)
		return false
	}
	return true
}
