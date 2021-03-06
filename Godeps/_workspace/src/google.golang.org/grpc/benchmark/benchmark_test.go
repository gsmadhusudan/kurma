package benchmark

import (
	"os"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/benchmark/stats"
	testpb "google.golang.org/grpc/interop/grpc_testing"
)

func run(b *testing.B, maxConcurrentCalls int, caller func(testpb.TestServiceClient)) {
	s := stats.AddStats(b, 38)
	b.StopTimer()
	target, stopper := StartServer()
	defer stopper()
	conn := NewClientConn(target)
	tc := testpb.NewTestServiceClient(conn)

	// Warm up connection.
	for i := 0; i < 10; i++ {
		caller(tc)
	}

	ch := make(chan int, maxConcurrentCalls*4)
	var wg sync.WaitGroup
	wg.Add(maxConcurrentCalls)

	// Distribute the b.N calls over maxConcurrentCalls workers.
	for i := 0; i < maxConcurrentCalls; i++ {
		go func() {
			for _ = range ch {
				caller(tc)
			}
			wg.Done()
		}()
	}
	for i := 0; i < b.N; i++ {
		b.StartTimer()
		start := time.Now()
		ch <- i
		elapsed := time.Since(start)
		b.StopTimer()
		s.Add(elapsed)
	}
	close(ch)
	wg.Wait()
	conn.Close()
}

func smallCaller(client testpb.TestServiceClient) {
	DoUnaryCall(client, 1, 1)
}

func BenchmarkClientSmallc1(b *testing.B) {
	run(b, 1, smallCaller)
}

func BenchmarkClientSmallc8(b *testing.B) {
	run(b, 8, smallCaller)
}

func BenchmarkClientSmallc64(b *testing.B) {
	run(b, 64, smallCaller)
}

func BenchmarkClientSmallc512(b *testing.B) {
	run(b, 512, smallCaller)
}

func TestMain(m *testing.M) {
	os.Exit(stats.RunTestMain(m))
}
