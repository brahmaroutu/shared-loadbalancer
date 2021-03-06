/*
Copyright 2018 The Shared LoadBalancer Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package providers

import (
	"errors"
	"fmt"

	kubeconv1alpha1 "github.com/Huang-Wei/shared-loadbalancer/pkg/apis/kubecon/v1alpha1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

// IKS stands for IBM Kubernetes Service
type IKS struct {
	// key is namespacedName of a LB Serivce, val is the service
	cacheMap map[types.NamespacedName]*corev1.Service

	// cr to LB is 1:1 mapping
	crToLB map[types.NamespacedName]types.NamespacedName
	// lb to CRD is 1:N mapping
	lbToCRs map[types.NamespacedName]nameSet
	// lbToPorts is keyed with ns/name of a LB, and valued with ports info it holds
	lbToPorts map[types.NamespacedName]int32Set

	capacityPerLB int
}

var _ LBProvider = &IKS{}

func newIKSProvider() *IKS {
	return &IKS{
		cacheMap:      make(map[types.NamespacedName]*corev1.Service),
		crToLB:        make(map[types.NamespacedName]types.NamespacedName),
		lbToCRs:       make(map[types.NamespacedName]nameSet),
		lbToPorts:     make(map[types.NamespacedName]int32Set),
		capacityPerLB: capacity,
	}
}

func (i *IKS) GetCapacityPerLB() int {
	return i.capacityPerLB
}

func (i *IKS) UpdateCache(key types.NamespacedName, lbSvc *corev1.Service) {
	if lbSvc == nil {
		delete(i.cacheMap, key)
	} else {
		i.cacheMap[key] = lbSvc
	}
}

func (i *IKS) NewService(sharedLB *kubeconv1alpha1.SharedLB) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      sharedLB.Name + SvcPostfix,
			Namespace: sharedLB.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Ports:    sharedLB.Spec.Ports,
			Selector: sharedLB.Spec.Selector,
		},
	}
}

func (i *IKS) NewLBService() *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "lb-" + RandStringRunes(8),
			Namespace: namespace,
			Labels:    map[string]string{"lb-template": ""},
		},
		Spec: corev1.ServiceSpec{
			Ports: []corev1.ServicePort{
				{
					Name:     "tcp",
					Protocol: corev1.ProtocolTCP,
					Port:     33333,
				},
				// TODO(Huang-Wei): handle UDP case
				// {
				// 	Name:     "UDP",
				// 	Protocol: corev1.ProtocolUDP,
				// 	Port:     33333,
				// },
			},
			Type: corev1.ServiceTypeLoadBalancer,
		},
	}
}

func (i *IKS) GetAvailabelLB(clusterSvc *corev1.Service) *corev1.Service {
	// we leverage the randomness of golang "for range" when iterating
OUTERLOOP:
	for lbKey, lbSvc := range i.cacheMap {
		if len(i.lbToCRs[lbKey]) >= i.capacityPerLB || len(lbSvc.Status.LoadBalancer.Ingress) == 0 {
			continue
		}
		// must satisfy that all svc ports are not occupied in lbSvc
		for _, svcPort := range clusterSvc.Spec.Ports {
			if i.lbToPorts[lbKey] == nil {
				i.lbToPorts[lbKey] = int32Set{}
			}
			if _, ok := i.lbToPorts[lbKey][svcPort.Port]; ok {
				log.WithName("iks").Info(fmt.Sprintf("incoming service has port conflict with lbSvc %q on port %d", lbKey, svcPort.Port))
				continue OUTERLOOP
			}
		}
		return lbSvc
	}
	return nil
}

func (i *IKS) AssociateLB(crName, lbName types.NamespacedName, clusterSvc *corev1.Service) error {
	if clusterSvc != nil {
		if lbSvc, ok := i.cacheMap[lbName]; !ok || len(lbSvc.Status.LoadBalancer.Ingress) == 0 {
			return errors.New("LoadBalancer service not exist yet")
		}
		// upon program starts, i.lbToPorts[lbName] can be nil
		if i.lbToPorts[lbName] == nil {
			i.lbToPorts[lbName] = int32Set{}
		}
		// update crToPorts
		for _, svcPort := range clusterSvc.Spec.Ports {
			i.lbToPorts[lbName][svcPort.Port] = struct{}{}
		}
	}

	// following code might be called multiple times, but shouldn't impact
	// performance a lot as all of them are O(1) operation
	_, ok := i.lbToCRs[lbName]
	if !ok {
		i.lbToCRs[lbName] = make(nameSet)
	}
	i.lbToCRs[lbName][crName] = struct{}{}
	i.crToLB[crName] = lbName
	log.WithName("iks").Info("AssociateLB", "cr", crName, "lb", lbName)
	return nil
}

// DeassociateLB is called by IKS finalizer to clean internal cache
// no IaaS things should be done for IKS
func (i *IKS) DeassociateLB(crName types.NamespacedName, clusterSvc *corev1.Service) error {
	// update internal cache
	if lb, ok := i.crToLB[crName]; ok {
		delete(i.crToLB, crName)
		delete(i.lbToCRs[lb], crName)
		for _, svcPort := range clusterSvc.Spec.Ports {
			delete(i.lbToPorts[lb], svcPort.Port)
		}
		log.WithName("iks").Info("DeassociateLB", "cr", crName, "lb", lb)
	}
	return nil
}

func (i *IKS) UpdateService(svc, lb *corev1.Service) (bool, bool) {
	lbName := types.NamespacedName{Name: lb.Name, Namespace: lb.Namespace}
	occupiedPorts := i.lbToPorts[lbName]
	if len(occupiedPorts) == 0 {
		occupiedPorts = int32Set{}
	}
	portUpdated := updatePort(svc, lb, occupiedPorts)
	externalIPUpdated := updateExternalIP(svc, lb)
	return portUpdated, externalIPUpdated
}

func updateExternalIP(svc, lb *corev1.Service) bool {
	if len(lb.Status.LoadBalancer.Ingress) != 1 {
		log.Info("No ingress info in lb.Status.LoadBalancer. Skip.")
		return false
	}
	// for IKS, we're setting loadbalancer info as "externalIP" to the service
	ingress := lb.Status.LoadBalancer.Ingress[0]
	svc.Spec.ExternalIPs = append(svc.Spec.ExternalIPs, ingress.IP)
	log.Info("Setting ExternalIP to service", "externalIP", ingress.IP)
	return true
}
