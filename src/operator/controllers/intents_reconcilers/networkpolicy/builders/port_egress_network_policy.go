package builders

import (
	"context"
	"fmt"
	otterizev2alpha1 "github.com/otterize/intents-operator/src/operator/api/v2alpha1"
	"github.com/otterize/intents-operator/src/operator/effectivepolicy"
	"github.com/otterize/intents-operator/src/shared/errors"
	"github.com/otterize/intents-operator/src/shared/injectablerecorder"
	"github.com/samber/lo"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	v1 "k8s.io/api/networking/v1"
	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// The PortEgressRulesBuilder creates network policies that allow egress traffic from pods to specific ports,
// based on which Kubernetes service is specified in the intents.
type PortEgressRulesBuilder struct {
	client.Client
	injectablerecorder.InjectableRecorder
}

func NewPortEgressRulesBuilder(c client.Client) *PortEgressRulesBuilder {
	return &PortEgressRulesBuilder{Client: c}
}

func (r *PortEgressRulesBuilder) buildEgressRulesFromEffectivePolicy(ctx context.Context, ep effectivepolicy.ServiceEffectivePolicy) ([]v1.NetworkPolicyEgressRule, error) {
	egressRules := make([]v1.NetworkPolicyEgressRule, 0)
	for _, intent := range ep.Calls {
		if intent.IsTargetOutOfCluster() {
			continue
		}
		if !intent.IsTargetServerKubernetesService() {
			continue
		}
		svc := corev1.Service{}
		err := r.Get(ctx, types.NamespacedName{Name: intent.GetTargetServerName(), Namespace: intent.GetTargetServerNamespace(ep.Service.Namespace)}, &svc)
		if err != nil {
			if k8serrors.IsNotFound(err) {
				continue
			}
			return nil, errors.Wrap(err)
		}
		var egressRule v1.NetworkPolicyEgressRule
		if svc.Spec.Selector != nil {
			egressRule = getEgressRuleBasedOnServicePodSelector(&svc)
		} else if intent.IsTargetTheKubernetesAPIServer(ep.Service.Namespace) {
			egressRule, err = r.collectIpsFromSVC(ctx, &svc)
			if err != nil {
				return nil, errors.Wrap(err)
			}
		} else {
			// Services without selectors (e.g., ExternalName services) don't route to pods
			// Skip creating egress rules for them
			continue
		}
		egressRules = append(egressRules, egressRule)
	}
	return egressRules, nil
}

// getEgressRuleBasedOnServicePodSelector returns a network policy egress rule that allows traffic to pods selected by the service
func getEgressRuleBasedOnServicePodSelector(svc *corev1.Service) v1.NetworkPolicyEgressRule {
	svcPodSelector := metav1.LabelSelector{MatchLabels: svc.Spec.Selector}
	podSelectorEgressRule := v1.NetworkPolicyEgressRule{
		To: []v1.NetworkPolicyPeer{
			{
				PodSelector: &svcPodSelector,
				NamespaceSelector: &metav1.LabelSelector{
					MatchLabels: map[string]string{
						otterizev2alpha1.KubernetesStandardNamespaceNameLabelKey: svc.Namespace,
					},
				},
			},
		},
	}

	// Create a list of network policy ports
	networkPolicyPorts := make([]v1.NetworkPolicyPort, 0)
	for _, port := range svc.Spec.Ports {
		targetPort := v1.NetworkPolicyPort{
			Port: lo.ToPtr(port.TargetPort),
		}
		if len(port.Protocol) != 0 {
			targetPort.Protocol = lo.ToPtr(port.Protocol)
		}
		// Adding service port to the list to solve some off-brand CNIs having issues with allowing traffic correctly
		servicePort := v1.NetworkPolicyPort{
			Port: &intstr.IntOrString{
				Type:   intstr.Int,
				IntVal: port.Port,
			},
		}
		if len(port.Protocol) != 0 {
			servicePort.Protocol = lo.ToPtr(port.Protocol)
		}
		networkPolicyPorts = append(networkPolicyPorts, targetPort, servicePort)
	}

	podSelectorEgressRule.Ports = networkPolicyPorts

	return podSelectorEgressRule
}

func (r *PortEgressRulesBuilder) collectIpsFromSVC(ctx context.Context, svc *corev1.Service) (v1.NetworkPolicyEgressRule, error) {
	ipAddresses := make([]string, 0)
	ports := make([]v1.NetworkPolicyPort, 0)

	if svc.Spec.ClusterIP != "" {
		ipAddresses = append(ipAddresses, svc.Spec.ClusterIP)
	}
	if len(svc.Spec.ClusterIPs) > 0 {
		ipAddresses = append(ipAddresses, svc.Spec.ClusterIPs...)
	}

	// Use EndpointSlice API instead of deprecated Endpoints API
	endpointSliceList := &discoveryv1.EndpointSliceList{}
	err := r.Client.List(ctx, endpointSliceList,
		client.InNamespace(svc.Namespace),
		client.MatchingLabels{discoveryv1.LabelServiceName: svc.Name})
	if err != nil {
		return v1.NetworkPolicyEgressRule{}, errors.Wrap(err)
	}

	if len(endpointSliceList.Items) == 0 {
		return v1.NetworkPolicyEgressRule{}, errors.Errorf("no endpoint slices found for service %s/%s", svc.Namespace, svc.Name)
	}

	portSet := make(map[string]v1.NetworkPolicyPort) // Use map to deduplicate ports
	hasEndpoints := false

	for _, endpointSlice := range endpointSliceList.Items {
		// Collect addresses from each endpoint
		for _, endpoint := range endpointSlice.Endpoints {
			if endpoint.Conditions.Ready != nil && !*endpoint.Conditions.Ready {
				continue // Skip non-ready endpoints
			}
			for _, address := range endpoint.Addresses {
				ipAddresses = append(ipAddresses, address)
				hasEndpoints = true
			}
		}

		// Collect ports (ports are at EndpointSlice level, not per endpoint)
		for _, port := range endpointSlice.Ports {
			if port.Port != nil {
				key := fmt.Sprintf("%d-%s", *port.Port, lo.FromPtrOr(port.Protocol, corev1.ProtocolTCP))
				portSet[key] = v1.NetworkPolicyPort{
					Port:     &intstr.IntOrString{IntVal: *port.Port},
					Protocol: port.Protocol,
				}
			}
		}
	}

	if !hasEndpoints {
		return v1.NetworkPolicyEgressRule{}, errors.Errorf("no ready endpoints found for service %s/%s", svc.Namespace, svc.Name)
	}

	// Convert port map to slice
	for _, port := range portSet {
		ports = append(ports, port)
	}

	if len(ipAddresses) == 0 {
		return v1.NetworkPolicyEgressRule{}, errors.Errorf("no endpoint addresses found for service %s/%s", svc.Namespace, svc.Name)
	}

	podSelectorEgressRule := v1.NetworkPolicyEgressRule{}
	for _, ip := range ipAddresses {
		podSelectorEgressRule.To = append(podSelectorEgressRule.To, v1.NetworkPolicyPeer{
			IPBlock: &v1.IPBlock{
				CIDR:   fmt.Sprintf("%s/32", ip),
				Except: nil,
			},
		})
	}

	if len(ports) > 0 {
		podSelectorEgressRule.Ports = ports
	}

	return podSelectorEgressRule, nil
}

func (r *PortEgressRulesBuilder) Build(ctx context.Context, ep effectivepolicy.ServiceEffectivePolicy) ([]v1.NetworkPolicyEgressRule, error) {
	return r.buildEgressRulesFromEffectivePolicy(ctx, ep)
}
