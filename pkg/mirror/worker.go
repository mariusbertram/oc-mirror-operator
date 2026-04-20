package mirror

import (
	"context"
	"sync"

	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
)

// Task defines an image to be mirrored
type Task struct {
	Source      string
	Destination string
	ImageSetKey string
	ImageIndex  int
}

// TaskResult contains the result of a mirroring task
type TaskResult struct {
	Task      Task
	Error     error
	IsSkipped bool
}

// WorkerPool manages a pool of worker goroutines
type WorkerPool struct {
	client  *mirrorclient.MirrorClient
	tasks   chan Task
	results chan TaskResult
	num     int
	wg      sync.WaitGroup
}

// NewWorkerPool creates a new WorkerPool
func NewWorkerPool(ctx context.Context, client *mirrorclient.MirrorClient, num int) *WorkerPool {
	p := &WorkerPool{
		client:  client,
		tasks:   make(chan Task, 100),
		results: make(chan TaskResult, 100),
		num:     num,
	}

	for i := 0; i < num; i++ {
		p.wg.Add(1)
		go p.worker(ctx)
	}

	return p
}

func (p *WorkerPool) Submit(t Task) {
	p.tasks <- t
}

func (p *WorkerPool) Results() <-chan TaskResult {
	return p.results
}

func (p *WorkerPool) worker(ctx context.Context) {
	defer p.wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case t, ok := <-p.tasks:
			if !ok {
				return
			}

			// Check if image exists at destination
			exists, err := p.client.CheckExist(ctx, t.Destination)
			if err == nil && exists {
				p.results <- TaskResult{Task: t, IsSkipped: true}
				continue
			}

			_, err = p.client.CopyImage(ctx, t.Source, t.Destination)
			p.results <- TaskResult{Task: t, Error: err}
		}
	}
}

func (p *WorkerPool) Stop() {
	close(p.tasks)
	p.wg.Wait()
	close(p.results)
}
