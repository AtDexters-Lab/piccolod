package server

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"piccolod/internal/lifecycle"
)

const (
	automaticUnlockCallerTimeout = 20 * time.Second
	unlockExecutionLiveness      = 30 * time.Second
)

var (
	errRecoveryInProgress         = &recoveryInProgressError{}
	errUnlockExecutionFatalCommit = errors.New("unlock recovery restart requested")
)

// recoveryInProgressError is the bounded manual-join result. It is deliberately
// typed so HTTP callers can distinguish an already-owned recovery from an
// ordinary unlock failure without launching a second body.
type recoveryInProgressError struct{}

func (*recoveryInProgressError) Error() string { return "recovery in progress" }
func (*recoveryInProgressError) Code() string  { return errorCodeRecoveryInProgress }

type unlockExecutionCaller uint8

const (
	unlockCallerManual unlockExecutionCaller = iota
	unlockCallerAutomatic
)

type unlockExecutionDecision uint32

const (
	unlockExecutionRunning unlockExecutionDecision = iota
	unlockExecutionCompletionCommitted
	unlockExecutionFatalCommitted
)

type unlockExecutionTimer interface {
	Stop() bool
}

type unlockExecutionAfterFunc func(time.Duration, func()) unlockExecutionTimer

type unlockExecutionAttempt struct {
	decision     atomic.Uint32
	done         chan struct{}
	bodyReturned chan struct{}
	cancelBody   context.CancelFunc
	timer        unlockExecutionTimer

	result unlockChainResult
	err    error
}

type unlockExecutionCoordinator struct {
	mu      sync.Mutex
	active  *unlockExecutionAttempt
	lastOK  unlockChainResult
	hasLast bool

	lifecycle      *lifecycle.Coordinator
	processContext func() context.Context
	body           func(context.Context) (unlockChainResult, error)
	onReady        func()
	onFatal        func()
	liveness       time.Duration
	afterFunc      unlockExecutionAfterFunc
}

type unlockExecutionConfig struct {
	lifecycle      *lifecycle.Coordinator
	processContext func() context.Context
	body           func(context.Context) (unlockChainResult, error)
	onReady        func()
	onFatal        func()
	liveness       time.Duration
	afterFunc      unlockExecutionAfterFunc
}

func newUnlockExecutionCoordinator(cfg unlockExecutionConfig) *unlockExecutionCoordinator {
	if cfg.processContext == nil {
		cfg.processContext = context.Background
	}
	if cfg.liveness <= 0 {
		cfg.liveness = unlockExecutionLiveness
	}
	if cfg.afterFunc == nil {
		cfg.afterFunc = func(d time.Duration, f func()) unlockExecutionTimer {
			return time.AfterFunc(d, f)
		}
	}
	return &unlockExecutionCoordinator{
		lifecycle:      cfg.lifecycle,
		processContext: cfg.processContext,
		body:           cfg.body,
		onReady:        cfg.onReady,
		onFatal:        cfg.onFatal,
		liveness:       cfg.liveness,
		afterFunc:      cfg.afterFunc,
	}
}

// execute joins the one process-local complete-unlock owner. Automatic callers
// wait for its terminal result (or their own caller deadline). A manual loser
// returns the bounded typed in-progress result immediately; it never starts a
// duplicate storage/persistence body.
func (c *unlockExecutionCoordinator) execute(callerCtx context.Context, caller unlockExecutionCaller) (unlockChainResult, error) {
	if callerCtx == nil {
		callerCtx = context.Background()
	}

	c.mu.Lock()
	attempt := c.active
	joined := attempt != nil
	if attempt == nil {
		if c.lifecycle != nil {
			switch c.lifecycle.State() {
			case lifecycle.StateReady:
				result := c.lastOK
				if !c.hasLast {
					// Ready is authoritative even when this coordinator was
					// installed after an earlier setup path completed.
					result.setupComplete = true
				}
				c.mu.Unlock()
				return result, nil
			case lifecycle.StateUnlocking:
				c.mu.Unlock()
				return unlockChainResult{}, errRecoveryInProgress
			}
			if err := c.lifecycle.BeginUnlock(); err != nil {
				c.mu.Unlock()
				return unlockChainResult{}, err
			}
		}

		// The liveness timer below is the sole deadline arbiter. Giving the
		// body its own deadline would let deadline cancellation make the body
		// return and race completion against fatal at the same boundary.
		bodyCtx, cancelBody := context.WithCancel(c.processContext())
		attempt = &unlockExecutionAttempt{
			done:         make(chan struct{}),
			bodyReturned: make(chan struct{}),
			cancelBody:   cancelBody,
		}
		c.active = attempt
		attempt.timer = c.afterFunc(c.liveness, func() {
			c.commitFatal(attempt)
		})
		go c.runBody(attempt, bodyCtx)
	}
	c.mu.Unlock()

	if joined && caller == unlockCallerManual {
		select {
		case <-attempt.done:
			return attempt.result, attempt.err
		default:
			return unlockChainResult{}, errRecoveryInProgress
		}
	}

	select {
	case <-attempt.done:
		return attempt.result, attempt.err
	case <-callerCtx.Done():
		// The caller owns only its wait. The process-scoped body context and
		// the active attempt remain intact until completion or liveness fatal.
		return unlockChainResult{}, callerCtx.Err()
	}
}

func (c *unlockExecutionCoordinator) runBody(attempt *unlockExecutionAttempt, bodyCtx context.Context) {
	defer close(attempt.bodyReturned)
	result, bodyErr := c.body(bodyCtx)
	if !attempt.decision.CompareAndSwap(uint32(unlockExecutionRunning), uint32(unlockExecutionCompletionCommitted)) {
		// Fatal already committed. The late return has no authority to mutate
		// lifecycle state, wake decrypted owners, or replace the fatal result.
		return
	}
	if attempt.timer != nil {
		attempt.timer.Stop()
	}
	attempt.cancelBody()

	terminalErr := bodyErr
	if c.lifecycle != nil {
		if bodyErr != nil {
			if err := c.lifecycle.MarkFailed(bodyErr); err != nil {
				terminalErr = fmt.Errorf("commit failed unlock lifecycle: %w", err)
			}
		} else if err := c.lifecycle.MarkReady(); err != nil {
			terminalErr = fmt.Errorf("commit ready unlock lifecycle: %w", err)
		}
	}

	attempt.result = result
	attempt.err = terminalErr
	c.mu.Lock()
	if c.active == attempt {
		c.active = nil
	}
	if terminalErr == nil {
		c.lastOK = result
		c.hasLast = true
	}
	c.mu.Unlock()
	close(attempt.done)

	if terminalErr == nil && c.onReady != nil {
		c.onReady()
	}
}

func (c *unlockExecutionCoordinator) commitFatal(attempt *unlockExecutionAttempt) {
	if !attempt.decision.CompareAndSwap(uint32(unlockExecutionRunning), uint32(unlockExecutionFatalCommitted)) {
		return
	}
	attempt.cancelBody()
	attempt.err = errUnlockExecutionFatalCommit
	if c.onFatal != nil {
		c.onFatal()
	}
	close(attempt.done)
}

func (s *GinServer) unlockExecutionOwner() *unlockExecutionCoordinator {
	s.unlockExecutionMu.Lock()
	defer s.unlockExecutionMu.Unlock()
	if s.unlockExecution == nil {
		s.unlockExecution = newUnlockExecutionCoordinator(unlockExecutionConfig{
			lifecycle:      s.lifecycle,
			processContext: s.serverContext,
			body:           s.runUnlockChain,
			onReady:        s.onUnlockChainReady,
			onFatal:        s.unlockFatalRecovery,
		})
	}
	return s.unlockExecution
}

func (s *GinServer) completeUnlockChain(ctx context.Context) (unlockChainResult, error) {
	return s.unlockExecutionOwner().execute(ctx, unlockCallerManual)
}

func (s *GinServer) completeAutomaticUnlockChain(ctx context.Context) (unlockChainResult, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	callerCtx, cancel := context.WithTimeout(ctx, automaticUnlockCallerTimeout)
	defer cancel()
	return s.unlockExecutionOwner().execute(callerCtx, unlockCallerAutomatic)
}
