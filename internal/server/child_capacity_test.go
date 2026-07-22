package server

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

func TestChildRequestCapacityEnforcesLimitAndReleasesExactlyOnce(t *testing.T) {
	var counter atomic.Int32
	releases := make([]func(), 0, 2)
	for i := 0; i < 2; i++ {
		release, ok := acquireChildRequestSlot(&counter, 2)
		if !ok {
			t.Fatalf("slot %d was rejected", i)
		}
		releases = append(releases, release)
	}
	if _, ok := acquireChildRequestSlot(&counter, 2); ok {
		t.Fatal("slot above limit was admitted")
	}
	releases[0]()
	releases[0]()
	if got := counter.Load(); got != 1 {
		t.Fatalf("count after duplicate release = %d, want 1", got)
	}
	if _, ok := acquireChildRequestSlot(&counter, 2); !ok {
		t.Fatal("released slot was not reusable")
	}
}

func TestChildRequestCapacityConcurrentAdmissionsNeverExceedLimit(t *testing.T) {
	const (
		limit = int32(4)
		burst = 64
	)
	var counter atomic.Int32
	admitted := make(chan struct{}, burst)
	start := make(chan struct{})
	releaseAll := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < burst; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			release, ok := acquireChildRequestSlot(&counter, limit)
			if !ok {
				return
			}
			admitted <- struct{}{}
			<-releaseAll
			release()
		}()
	}
	close(start)
	for i := int32(0); i < limit; i++ {
		select {
		case <-admitted:
		case <-time.After(time.Second):
			t.Fatalf("timed out after %d admissions", i)
		}
	}
	time.Sleep(10 * time.Millisecond)
	if got := len(admitted); got != 0 {
		t.Fatalf("extra admissions = %d, want 0", got)
	}
	if got := counter.Load(); got != limit {
		t.Fatalf("inflight = %d, want %d", got, limit)
	}
	close(releaseAll)
	wg.Wait()
	if got := counter.Load(); got != 0 {
		t.Fatalf("inflight after release = %d, want 0", got)
	}
}

func TestGinServerLogEndpointsShareCapacity(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := &GinServer{}
	releases := make([]func(), 0, maxConcurrentChildLogStreams)
	for i := int32(0); i < maxConcurrentChildLogStreams; i++ {
		release, ok := server.acquireChildLogStream()
		if !ok {
			t.Fatalf("shared stream slot %d was rejected", i)
		}
		releases = append(releases, release)
	}
	if _, ok := server.acquireChildLogStream(); ok {
		t.Fatal("shared stream pool admitted above its limit")
	}

	appRecorder := httptest.NewRecorder()
	appContext, _ := gin.CreateTestContext(appRecorder)
	appContext.Request = httptest.NewRequest(http.MethodGet, "/api/v1/apps/demo/logs/stream", nil)
	appContext.Params = gin.Params{{Key: "name", Value: "demo"}}
	server.handleGinAppLogStream(appContext)
	if got := appRecorder.Code; got != http.StatusTooManyRequests {
		t.Fatalf("app log stream status = %d, want %d", got, http.StatusTooManyRequests)
	}

	systemRecorder := httptest.NewRecorder()
	systemContext, _ := gin.CreateTestContext(systemRecorder)
	systemContext.Request = httptest.NewRequest(http.MethodGet, "/api/v1/system/logs/stream?unit=piccolod", nil)
	server.handleGinSystemLogStream(systemContext)
	if got := systemRecorder.Code; got != http.StatusTooManyRequests {
		t.Fatalf("system log stream status = %d, want %d", got, http.StatusTooManyRequests)
	}
	for _, release := range releases {
		release()
	}
}

func TestChildDiagnosticCapacityReleasesOnEarlyHandlerFailure(t *testing.T) {
	gin.SetMode(gin.TestMode)
	server := &GinServer{}
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Request = httptest.NewRequest(http.MethodGet, "/api/v1/system/storage-check", nil)
	server.handleStorageCheck(context)
	if got := recorder.Code; got != http.StatusServiceUnavailable {
		t.Fatalf("storage check status = %d, want %d", got, http.StatusServiceUnavailable)
	}
	release, ok := server.acquireChildDiagnostic()
	if !ok {
		t.Fatal("diagnostic slot leaked after early handler failure")
	}
	release()
}
