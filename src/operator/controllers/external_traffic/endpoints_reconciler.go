package external_traffic

import (
	"context"
	"github.com/otterize/intents-operator/src/operator/api/v2alpha1"
	"github.com/otterize/intents-operator/src/shared/errors"
	"github.com/otterize/intents-operator/src/shared/injectablerecorder"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	v1 "k8s.io/api/networking/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
)

//+kubebuilder:rbac:groups="",resources=services,verbs=get;list;watch
//+kubebuilder:rbac:groups="discovery.k8s.io",resources=endpointslices,verbs=get;list;watch
//+kubebuilder:rbac:groups="networking.k8s.io",resources=networkpolicies,verbs=get;update;patch;list;watch;delete;create

type EndpointsReconciler interface {
	Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error)
	InitIngressReferencedServicesIndex(mgr ctrl.Manager) error
	SetupWithManager(mgr ctrl.Manager) error
	InjectRecorder(recorder record.EventRecorder)
}

type EndpointsReconcilerImpl struct {
	client.Client
	extNetpolHandler *NetworkPolicyHandler
	injectablerecorder.InjectableRecorder
}

func NewEndpointsReconciler(client client.Client, extNetpolHandler *NetworkPolicyHandler) EndpointsReconciler {
	return &EndpointsReconcilerImpl{
		Client:           client,
		extNetpolHandler: extNetpolHandler,
	}
}

func (r *EndpointsReconcilerImpl) SetupWithManager(mgr ctrl.Manager) error {
	recorder := mgr.GetEventRecorderFor("intents-operator")
	r.InjectRecorder(recorder)

	return ctrl.NewControllerManagedBy(mgr).
		For(&discoveryv1.EndpointSlice{}).
		WithOptions(controller.Options{RecoverPanic: lo.ToPtr(true)}).
		Complete(r)
}

func (r *EndpointsReconcilerImpl) InjectRecorder(recorder record.EventRecorder) {
	r.Recorder = recorder
	r.extNetpolHandler.InjectRecorder(recorder)
}

func (r *EndpointsReconcilerImpl) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	endpointSlice := &discoveryv1.EndpointSlice{}
	err := r.Get(ctx, req.NamespacedName, endpointSlice)
	if k8serrors.IsNotFound(err) {
		// delete is handled by garbage collection - the service owns the network policy
		return ctrl.Result{}, nil
	}

	if err != nil {
		return ctrl.Result{}, errors.Wrap(err)
	}

	// Get service name from EndpointSlice label
	serviceName, ok := endpointSlice.Labels[discoveryv1.LabelServiceName]
	if !ok {
		// EndpointSlice not associated with a service, skip
		return ctrl.Result{}, nil
	}

	// Fetch the corresponding Endpoints object for compatibility with HandleEndpoints
	endpoints := &corev1.Endpoints{}
	err = r.Get(ctx, types.NamespacedName{Name: serviceName, Namespace: req.Namespace}, endpoints)
	if err != nil {
		// If Endpoints doesn't exist, skip (service may not have traditional endpoints)
		if k8serrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, errors.Wrap(err)
	}

	err = r.extNetpolHandler.HandleEndpoints(ctx, endpoints)

	if err != nil {
		return ctrl.Result{}, errors.Wrap(err)
	}

	return ctrl.Result{}, nil
}

func (r *EndpointsReconcilerImpl) InitIngressReferencedServicesIndex(mgr ctrl.Manager) error {
	err := mgr.GetCache().IndexField(
		context.Background(),
		&v1.Ingress{},
		v2alpha1.IngressServiceNamesIndexField,
		func(object client.Object) []string {
			ingress := object.(*v1.Ingress)
			services := serviceNamesFromIngress(ingress)
			return sets.List(services)
		})

	if err != nil {
		return errors.Wrap(err)
	}

	return nil
}
