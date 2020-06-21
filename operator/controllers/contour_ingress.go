package controllers

import (
	"context"
	"fmt"
	"github.com/go-logr/logr"
	contour "github.com/projectcontour/contour/apis/projectcontour/v1"
	machinelearningv1 "github.com/seldonio/seldon-core/operator/apis/machinelearning.seldon.io/v1"
	v1 "github.com/seldonio/seldon-core/operator/apis/machinelearning.seldon.io/v1"
	"github.com/seldonio/seldon-core/operator/constants"
	v13 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	v12 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	types2 "k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"knative.dev/pkg/kmp"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
)

const (
	ENV_CONTOUR_ENABLED       = "CONTOUR_ENABLED"
	ENV_CONTOUR_BASE_HOST     = "CONTOUR_BASE_HOST"
	ENV_CONTOUR_INGRESS_CLASS = "CONTOUR_INGRESS_CLASS"
)

var grpcProtocol = "h2c"

// +kubebuilder:rbac:groups=projectcontour.io,resources=httpproxies,verbs=get;list;watch;create;update;patch;delete

type ContourIngress struct {
	client   client.Client
	recorder record.EventRecorder
	scheme   *runtime.Scheme
}

func NewContourIngress() Ingress {
	return &ContourIngress{}
}

func (i *ContourIngress) AddToScheme(scheme *runtime.Scheme) {
	contour.AddKnownTypes(scheme)
}

func (i *ContourIngress) SetupWithManager(mgr ctrl.Manager) ([]runtime.Object, error) {
	// Store the client, recorder and scheme for use later
	i.client = mgr.GetClient()
	i.recorder = mgr.GetEventRecorderFor(constants.ControllerName)
	i.scheme = mgr.GetScheme()

	// Index on VirtualService
	if err := mgr.GetFieldIndexer().IndexField(&contour.HTTPProxy{}, ownerKey, func(rawObj runtime.Object) []string {
		// grab the deployment object, extract the owner...
		vsvc := rawObj.(*contour.HTTPProxy)
		owner := metav1.GetControllerOf(vsvc)
		if owner == nil {
			return nil
		}
		// ...make sure it's a SeldonDeployment...
		if owner.APIVersion != apiGVStr || owner.Kind != "SeldonDeployment" {
			return nil
		}

		// ...and if so, return it
		return []string{owner.Name}
	}); err != nil {
		return nil, err
	}
	return []runtime.Object{&contour.HTTPProxy{}}, nil
}

func (i *ContourIngress) GeneratePredictorResources(mlDep *v1.SeldonDeployment, seldonId string, namespace string, ports []httpGrpcPorts, httpAllowed bool, grpcAllowed bool) (map[IngressResourceType][]runtime.Object, error) {
	contourBaseHost := GetEnv(ENV_CONTOUR_BASE_HOST, "") // TODO(jpg): This to be better to handle cases when base host isn't set better
	contourIngressClass := GetEnv(ENV_CONTOUR_INGRESS_CLASS, "")
	var annotations map[string]string
	if contourIngressClass != "" {
		annotations = map[string]string{
			"projectcontour.io/ingress.class": contourIngressClass,
		}
	}
	var httpServices []contour.Service
	var grpcServices []contour.Service
	var routes []contour.Route

	for i, predictor := range mlDep.Spec.Predictors {
		predictorServiceName := v1.GetPredictorKey(mlDep, &predictor)
		httpServices = append(httpServices, contour.Service{
			Name:   predictorServiceName,
			Mirror: predictor.Shadow,
			Weight: int64(predictor.Traffic),
			Port:   ports[i].httpPort,
		})
		grpcServices = append(grpcServices, contour.Service{
			Name:     predictorServiceName,
			Mirror:   predictor.Shadow,
			Weight:   int64(predictor.Traffic),
			Port:     ports[i].grpcPort,
			Protocol: &grpcProtocol,
		})
	}

	if httpAllowed {
		routes = append(routes, contour.Route{
			Conditions: []contour.Condition{{
				Prefix: "/",
			}},
			Services: httpServices,
		})
	}

	if grpcAllowed {
		routes = append(routes, []contour.Route{{
			Conditions: []contour.Condition{{Prefix: constants.GRPCPathPrefixSeldon}},
			Services:   grpcServices,
		}, {
			Conditions: []contour.Condition{{Prefix: constants.GRPCPathPrefixTensorflow}},
			Services:   grpcServices,
		}}...)
	}

	return map[IngressResourceType][]runtime.Object{
		ContourHTTPProxies: {&contour.HTTPProxy{
			ObjectMeta: v12.ObjectMeta{
				Name:        seldonId,
				Namespace:   namespace,
				Annotations: annotations,
			},
			Spec: contour.HTTPProxySpec{
				VirtualHost: &contour.VirtualHost{
					Fqdn: fmt.Sprintf("%s.%s", mlDep.Name, contourBaseHost),
				},
				Routes: routes,
			},
		}},
	}, nil
}

func (i *ContourIngress) GenerateExplainerResources(pSvcName string, p *v1.PredictorSpec, mlDep *v1.SeldonDeployment, seldonId string, namespace string, engineHttpPort int, engineGrpcPort int) (map[IngressResourceType][]runtime.Object, error) {
	contourBaseHost := GetEnv(ENV_CONTOUR_BASE_HOST, "") // TODO(jpg): This to be better to handle cases when base host isn't set better
	var httpProxies []runtime.Object

	if engineHttpPort > 0 {
		httpProxies = append(httpProxies, &contour.HTTPProxy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pSvcName + "-http",
				Namespace: namespace,
			},
			Spec: contour.HTTPProxySpec{
				VirtualHost: &contour.VirtualHost{
					Fqdn: fmt.Sprintf("%s-explainer.%s", mlDep.Name, contourBaseHost),
					TLS:  nil,
				},
				Routes: []contour.Route{{
					Conditions: []contour.Condition{{
						Prefix: "/",
					}},
					Services: []contour.Service{
						{
							Name:   pSvcName,
							Weight: int64(100),
							Port:   engineHttpPort,
						},
					},
				}},
			},
		})
	}

	if engineGrpcPort > 0 {
		httpProxies = append(httpProxies, &contour.HTTPProxy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pSvcName + "-grpc",
				Namespace: namespace,
			},
			Spec: contour.HTTPProxySpec{
				VirtualHost: &contour.VirtualHost{
					Fqdn: fmt.Sprintf("%s-explainer-grpc.%s", mlDep.Name, contourBaseHost),
					TLS:  nil,
				},
				Routes: []contour.Route{{
					Conditions: []contour.Condition{{
						Prefix: "/",
					}},
					Services: []contour.Service{
						{
							Name:     pSvcName,
							Weight:   int64(100),
							Port:     engineGrpcPort,
							Protocol: &grpcProtocol,
						},
					},
				}},
			},
		})
	}

	return map[IngressResourceType][]runtime.Object{
		ContourHTTPProxies: httpProxies,
	}, nil
}

func (i *ContourIngress) CreateResources(resources map[IngressResourceType][]runtime.Object, instance *v1.SeldonDeployment, log logr.Logger) (bool, error) {
	ready := true
	if httpProxies, ok := resources[ContourHTTPProxies]; ok == true {
		for _, s := range httpProxies {
			httpProxy := s.(*contour.HTTPProxy)
			if err := controllerutil.SetControllerReference(instance, httpProxy, i.scheme); err != nil {
				return ready, err
			}
			found := &contour.HTTPProxy{}
			err := i.client.Get(context.TODO(), types2.NamespacedName{Name: httpProxy.Name, Namespace: httpProxy.Namespace}, found)
			if err != nil && errors.IsNotFound(err) {
				ready = false
				log.Info("Creating HTTPProxy", "namespace", httpProxy.Namespace, "name", httpProxy.Name)
				err = i.client.Create(context.TODO(), httpProxy)
				if err != nil {
					return ready, err
				}
				i.recorder.Eventf(instance, v13.EventTypeNormal, constants.EventsCreateHTTPProxy, "Created HTTPProxy %q", httpProxy.GetName())
			} else if err != nil {
				return ready, err
			} else {
				// Update the found object and write the result back if there are any changes
				if !equality.Semantic.DeepEqual(httpProxy.Spec, found.Spec) {
					desiredSvc := found.DeepCopy()
					found.Spec = httpProxy.Spec
					log.Info("Updating HTTPProxy", "namespace", httpProxy.Namespace, "name", httpProxy.Name)
					err = i.client.Update(context.TODO(), found)
					if err != nil {
						return ready, err
					}

					// Check if what came back from server modulo the defaults applied by k8s is the same or not
					if !equality.Semantic.DeepEqual(desiredSvc.Spec, found.Spec) {
						ready = false
						i.recorder.Eventf(instance, v13.EventTypeNormal, constants.EventsUpdateHTTPProxy, "Updated HTTPProxy %q", httpProxy.GetName())
						// For debugging we will show the difference
						diff, err := kmp.SafeDiff(desiredSvc.Spec, found.Spec)
						if err != nil {
							log.Error(err, "Failed to diff")
						} else {
							log.Info(fmt.Sprintf("Difference in HTTPProxy: %v", diff))
						}
					} else {
						log.Info("The HTTPProxy objects are the same - API server defaults ignored")
					}
				} else {
					log.Info("Found identical HTTPProxy", "namespace", found.Namespace, "name", found.Name)
				}
			}
		}

		httpProxyList := &contour.HTTPProxyList{}
		// TODO(jpg): What to do on error here?
		_ = i.client.List(context.Background(), httpProxyList, &client.ListOptions{Namespace: instance.Namespace})
		for _, httpProxy := range httpProxyList.Items {
			for _, ownerRef := range httpProxy.OwnerReferences {
				if ownerRef.Name == instance.Name {
					found := false
					for _, p := range httpProxies {
						expectedHttpProxy := p.(*contour.HTTPProxy)
						if expectedHttpProxy.Name == httpProxy.Name {
							found = true
							break
						}
					}
					if !found {
						log.Info("Will delete HTTPProxy", "name", httpProxy.Name, "namespace", httpProxy.Namespace)
						_ = i.client.Delete(context.Background(), &httpProxy, client.PropagationPolicy(metav1.DeletePropagationForeground))
					}
				}
			}
		}
	}
	return true, nil
}

func (i *ContourIngress) GenerateServiceAnnotations(mlDep *machinelearningv1.SeldonDeployment, p *machinelearningv1.PredictorSpec, serviceName string, engineHttpPort, engineGrpcPort int, isExplainer bool) (map[string]string, error) {
	return nil, nil
}
