// Code generated by informer-gen. DO NOT EDIT.

package externalversions

import (
	fmt "fmt"

	v1 "github.com/openshift/api/machine/v1"
	v1beta1 "github.com/openshift/api/machine/v1beta1"
	schema "k8s.io/apimachinery/pkg/runtime/schema"
	cache "k8s.io/client-go/tools/cache"
)

// GenericInformer is type of SharedIndexInformer which will locate and delegate to other
// sharedInformers based on type
type GenericInformer interface {
	Informer() cache.SharedIndexInformer
	Lister() cache.GenericLister
}

type genericInformer struct {
	informer cache.SharedIndexInformer
	resource schema.GroupResource
}

// Informer returns the SharedIndexInformer.
func (f *genericInformer) Informer() cache.SharedIndexInformer {
	return f.informer
}

// Lister returns the GenericLister.
func (f *genericInformer) Lister() cache.GenericLister {
	return cache.NewGenericLister(f.Informer().GetIndexer(), f.resource)
}

// ForResource gives generic access to a shared informer of the matching type
// TODO extend this to unknown resources with a client pool
func (f *sharedInformerFactory) ForResource(resource schema.GroupVersionResource) (GenericInformer, error) {
	switch resource {
	// Group=machine.openshift.io, Version=v1
	case v1.SchemeGroupVersion.WithResource("controlplanemachinesets"):
		return &genericInformer{resource: resource.GroupResource(), informer: f.Machine().V1().ControlPlaneMachineSets().Informer()}, nil

		// Group=machine.openshift.io, Version=v1beta1
	case v1beta1.SchemeGroupVersion.WithResource("machines"):
		return &genericInformer{resource: resource.GroupResource(), informer: f.Machine().V1beta1().Machines().Informer()}, nil
	case v1beta1.SchemeGroupVersion.WithResource("machinehealthchecks"):
		return &genericInformer{resource: resource.GroupResource(), informer: f.Machine().V1beta1().MachineHealthChecks().Informer()}, nil
	case v1beta1.SchemeGroupVersion.WithResource("machinesets"):
		return &genericInformer{resource: resource.GroupResource(), informer: f.Machine().V1beta1().MachineSets().Informer()}, nil

	}

	return nil, fmt.Errorf("no informer found for %v", resource)
}
