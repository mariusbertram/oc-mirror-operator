package mirror

import (
	"context"
	mirrorclient "github.com/mariusbertram/oc-mirror-operator/pkg/mirror/client"
	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

var _ = Describe("WorkerPool", func() {
	var (
		pool *WorkerPool
		mc   *mirrorclient.MirrorClient
	)

	BeforeEach(func() {
		mc = mirrorclient.NewMirrorClient()
		pool = NewWorkerPool(context.TODO(), mc, 1)
	})

	Context("Task execution", func() {
		It("should process submitted tasks", func() {
			task := Task{
				Source:      "src",
				Destination: "dest",
			}
			pool.Submit(task)

			// We expect a result (likely an error because no real registry)
			var result TaskResult
			Eventually(pool.Results(), "2s").Should(Receive(&result))
			Expect(result.Task.Source).To(Equal("src"))
		})
	})

	AfterEach(func() {
		pool.Stop()
	})
})
