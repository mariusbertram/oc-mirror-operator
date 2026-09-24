package resourceapi

import "sigs.k8s.io/controller-runtime/pkg/client"

// NewServerForTest returns a namespace-bound Server without a base REST
// config, so requests carrying a Bearer token use c instead of building a
// client against whatever cluster the test environment's kubeconfig points
// at.
func NewServerForTest(c client.Client, namespace string) *Server {
	return &Server{client: c, namespace: namespace, scheme: c.Scheme()}
}
