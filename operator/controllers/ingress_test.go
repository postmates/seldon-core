package controllers

import (
	. "github.com/onsi/gomega"
	contour "github.com/projectcontour/contour/apis/projectcontour/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"testing"
)

func TestContourIngress(t *testing.T) {
	g := NewGomegaWithT(t)
	predictorReplicas := int32(4)
	name := "dep"
	namespace := "default"

	logger := ctrl.Log.WithName("controllers").WithName("SeldonDeployment")
	reconciler := &SeldonDeploymentReconciler{
		Log:     logger,
		Ingress: NewContourIngress(),
	}

	// Just Predictor Replicas
	instance := createSeldonDeploymentWithReplicas(name, namespace, nil, &predictorReplicas, nil, nil)
	instance.Spec.DefaultSeldonDeployment(name, namespace)
	c, err := reconciler.createComponents(instance, nil, logger)
	g.Expect(err).To(BeNil())
	g.Expect(len(c.deployments)).To(Equal(1))
	g.Expect(*c.deployments[0].Spec.Replicas).To(Equal(predictorReplicas))
	// We should have only created Contour HTTPProxies
	g.Expect(len(c.ingressResources)).To(Equal(1))
	// Check HTTPProxy resources are created correctly
	httpProxies, ok := c.ingressResources[ContourHTTPProxies]
	g.Expect(ok).To(BeTrue())
	g.Expect(len(httpProxies)).To(Equal(1))
	httpProxy := httpProxies[0].(*contour.HTTPProxy)
	g.Expect(httpProxy.Name).To(Equal(name))
	g.Expect(httpProxy.Spec.VirtualHost.Fqdn).To(Equal(name))
}
