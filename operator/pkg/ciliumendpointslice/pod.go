// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package ciliumendpointslice

import (
	v2alpha1 "github.com/cilium/cilium/pkg/k8s/apis/cilium.io/v2alpha1"
	slim_corev1 "github.com/cilium/cilium/pkg/k8s/slim/k8s/api/core/v1"
	"github.com/cilium/cilium/operator/pkg/ciliumidentity/cache"
)

// ConvertCEPToCoreCEP converts a CiliumEndpoint to a CoreCiliumEndpoint
// containing only a minimal set of entities used to
func ConvertPodToCoreCiliumEndpoint(pod *slim_corev1.Pod) *v2alpha1.CoreCiliumEndpoint {
	identityID := cache.
	GetPodCID(pod.GetName())

	

	return &cilium_v2alpha1.CoreCiliumEndpoint{
		Name: pod.GetName(),
		IdentityID: ,
	}
	/*
		// Copy Networking field into core CEP
		var epNetworking *cilium_v2.EndpointNetworking
		if cep.Status.Networking != nil {
			epNetworking = new(cilium_v2.EndpointNetworking)
			cep.Status.Networking.DeepCopyInto(epNetworking)
		}

		// TODO: Get Identity of Pod
		var identityID int64 = 0
		if cep.Status.Identity != nil {
			identityID = cep.Status.Identity.ID
		}
		return &cilium_v2alpha1.CoreCiliumEndpoint{
			Name:       pod.GetName(),
			Networking: epNetworking,
			Encryption: cep.Status.Encryption,
			IdentityID: identityID,
			NamedPorts: cep.Status.NamedPorts.DeepCopy(),
		}
	*/
}