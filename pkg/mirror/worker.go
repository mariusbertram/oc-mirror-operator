package mirror

import (
	"context"
	"sync"
)

// Task represents a single image mirroring job
type Task struct {
	Source      string
	Destination string
	ImageSetKey string // For status updates
	ImageIndex  int    // Index in the ImageSet status TargetImages
}

// WorkerPool manages parallel mirroring tasks
type WorkerPool struct {
	client     *MirrorClient
	tasks      chan Task
	results    chan TaskResult
	numWorkers int
	ctx        context.Context
	cancel     context.CancelFunc
	wg         sync.WaitGroup
}

// TaskResult contains the result of a mirroring job
type TaskResult struct {
	Task      Task
	Error     error
	IsSkipped bool
}

// NewWorkerPool creates and starts a worker pool
func NewWorkerPool(ctx context.Context, client *MirrorClient, numWorkers int) *WorkerPool {
	p := &WorkerPool{
		client:     client,
		tasks:      make(chan Task, 100),
		results:    make(chan TaskResult, 100),
		numWorkers: numWorkers,
	}
	p.ctx, p.cancel = context.WithCancel(ctx)
	p.start()
	return p
}

func (p *WorkerPool) start() {
	for i := 0; i < p.numWorkers; i++ {
		p.wg.Add(1)
		go p.worker()
	}
}

func (p *WorkerPool) worker() {
	defer p.wg.Done()
	for {
		select {
		case <-p.ctx.Done():
			return
		case t, ok := <-p.tasks:
			if !ok {
				return
			}

			// 1. Check if exists
			exists, err := p.client.CheckExist(p.ctx, t.Destination)
			if err == nil && exists {
				p.results <- TaskResult{Task: t, IsSkipped: true}
				continue
			}

			// 2. Perform copy
			err = p.client.CopyImage(p.ctx, t.Source, t.Destination)
			p.results <- TaskResult{Task: t, Error: err}
		}
	}
}

// Submit adds a task to the pool
func (p *WorkerPool) Submit(t Task) {
	p.tasks <- t
}

// Results returns the results channel
func (p *WorkerPool) Results() <-chan TaskResult {
	return p.results
}

// Shutdown stops all workers
func (p *WorkerPool) Shutdown() {
	p.cancel()
	close(p.tasks)
	p.wg.Wait()
	close(p.results)
}
