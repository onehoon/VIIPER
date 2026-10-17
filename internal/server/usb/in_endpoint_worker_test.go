package usb

import (
	"context"
	"runtime"
	"sync"
	"testing"
	"time"
)

func TestINEndpointWorkerReusesReceiverAndCancelsActiveJob(t *testing.T) {
	streamDone := make(chan struct{})
	var workerWG sync.WaitGroup
	entered := make(chan uint32, 2)
	exited := make(chan uint32, 2)
	buffers := make(chan []byte, 2)
	run := func(job inJob, report []byte) {
		buffers <- report
		entered <- job.seq
		<-job.ctx.Done()
		exited <- job.seq
	}
	worker := startINEndpointWorker(streamDone, &workerWG, run)

	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer func() {
		cancel1()
		cancel2()
		close(streamDone)
		workerWG.Wait()
	}()

	first := inJob{seq: 1, ctx: ctx1, cancel: cancel1}
	if !dispatchToINWorker(t, worker, first) {
		t.Fatal("first request was not accepted by the persistent worker")
	}
	waitINJob(t, entered, first.seq)
	firstBuffer := <-buffers
	cancel1()
	waitINJob(t, exited, first.seq)

	second := inJob{seq: 2, ctx: ctx2, cancel: cancel2}
	if !dispatchToINWorker(t, worker, second) {
		t.Fatal("persistent worker did not accept a later sequential request")
	}
	waitINJob(t, entered, second.seq)
	secondBuffer := <-buffers
	if &firstBuffer[0] != &secondBuffer[0] {
		t.Fatal("persistent worker did not reuse its endpoint-owned scratch")
	}
	cancel2()
	waitINJob(t, exited, second.seq)
}

func TestINEndpointWorkerFallsBackWhileBusy(t *testing.T) {
	streamDone := make(chan struct{})
	var workerWG sync.WaitGroup
	var fallbackWG sync.WaitGroup
	entered := make(chan uint32, 2)
	exited := make(chan uint32, 2)
	buffers := make(chan []byte, 2)
	run := func(job inJob, report []byte) {
		buffers <- report
		entered <- job.seq
		<-job.ctx.Done()
		exited <- job.seq
	}
	worker := startINEndpointWorker(streamDone, &workerWG, run)

	ctx1, cancel1 := context.WithCancel(context.Background())
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer func() {
		cancel1()
		cancel2()
		close(streamDone)
		workerWG.Wait()
		fallbackWG.Wait()
	}()

	first := inJob{seq: 1, ctx: ctx1, cancel: cancel1}
	if !dispatchToINWorker(t, worker, first) {
		t.Fatal("first request was not accepted by the persistent worker")
	}
	waitINJob(t, entered, first.seq)
	workerBuffer := <-buffers

	second := inJob{seq: 2, ctx: ctx2, cancel: cancel2}
	if worker.tryDispatch(second) {
		t.Fatal("busy persistent worker accepted an overlapping request")
	}
	fallbackWG.Add(1)
	go func() {
		defer fallbackWG.Done()
		var report [64]byte
		run(second, report[:])
	}()
	waitINJob(t, entered, second.seq)
	fallbackBuffer := <-buffers
	if &workerBuffer[0] == &fallbackBuffer[0] {
		t.Fatal("overlapping fallback shared mutable scratch with persistent worker")
	}

	cancel2()
	waitINJob(t, exited, second.seq)
	if err := ctx1.Err(); err != nil {
		t.Fatalf("cancelling fallback request also cancelled persistent request: %v", err)
	}
	cancel1()
	waitINJob(t, exited, first.seq)
}

func dispatchToINWorker(t *testing.T, worker *inEndpointWorker, job inJob) bool {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if worker.tryDispatch(job) {
			return true
		}
		runtime.Gosched()
	}
	return false
}

func waitINJob(t *testing.T, ch <-chan uint32, seq uint32) {
	t.Helper()
	select {
	case got := <-ch:
		if got != seq {
			t.Fatalf("job sequence = %d, want %d", got, seq)
		}
	case <-time.After(time.Second):
		t.Fatalf("timed out waiting for job %d", seq)
	}
}
