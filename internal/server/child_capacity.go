package server

import (
	"net/http"
	"sync"
	"sync/atomic"

	"github.com/gin-gonic/gin"
)

// Child-backed request limits reserve task headroom between task-guard
// samples. Log followers share one pool because both app logs and system logs
// keep a child alive for the websocket lifetime. Diagnostics use a separate,
// deliberately single-flight pool because they are short-lived but can start
// several helper processes per request.
const (
	maxConcurrentChildLogStreams  = int32(8)
	maxConcurrentChildDiagnostics = int32(1)
)

type childRequestCapacity struct {
	logStreams  atomic.Int32
	diagnostics atomic.Int32
}

func acquireChildRequestSlot(counter *atomic.Int32, limit int32) (func(), bool) {
	if counter == nil || limit <= 0 {
		return nil, false
	}
	for {
		current := counter.Load()
		if current >= limit {
			return nil, false
		}
		if counter.CompareAndSwap(current, current+1) {
			var once sync.Once
			return func() {
				once.Do(func() { counter.Add(-1) })
			}, true
		}
	}
}

func (s *GinServer) acquireChildLogStream() (func(), bool) {
	if s == nil {
		return nil, false
	}
	return acquireChildRequestSlot(&s.childCapacity.logStreams, maxConcurrentChildLogStreams)
}

func (s *GinServer) acquireChildDiagnostic() (func(), bool) {
	if s == nil {
		return nil, false
	}
	return acquireChildRequestSlot(&s.childCapacity.diagnostics, maxConcurrentChildDiagnostics)
}

func writeChildCapacityError(c *gin.Context, work string) {
	c.Header("Retry-After", "5")
	c.AbortWithStatusJSON(http.StatusTooManyRequests, gin.H{
		"error": "Too many concurrent " + work + "; try again in a moment",
	})
}
