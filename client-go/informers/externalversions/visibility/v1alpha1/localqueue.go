/*
Copyright 2022 The Kubernetes Authors.

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
// Code generated by informer-gen. DO NOT EDIT.

package v1alpha1

import (
	"context"
	time "time"

	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	runtime "k8s.io/apimachinery/pkg/runtime"
	watch "k8s.io/apimachinery/pkg/watch"
	cache "k8s.io/client-go/tools/cache"
	visibilityv1alpha1 "sigs.k8s.io/kueue/apis/visibility/v1alpha1"
	versioned "sigs.k8s.io/kueue/client-go/clientset/versioned"
	internalinterfaces "sigs.k8s.io/kueue/client-go/informers/externalversions/internalinterfaces"
	v1alpha1 "sigs.k8s.io/kueue/client-go/listers/visibility/v1alpha1"
)

// LocalQueueInformer provides access to a shared informer and lister for
// LocalQueues.
type LocalQueueInformer interface {
	Informer() cache.SharedIndexInformer
	Lister() v1alpha1.LocalQueueLister
}

type localQueueInformer struct {
	factory          internalinterfaces.SharedInformerFactory
	tweakListOptions internalinterfaces.TweakListOptionsFunc
	namespace        string
}

// NewLocalQueueInformer constructs a new informer for LocalQueue type.
// Always prefer using an informer factory to get a shared informer instead of getting an independent
// one. This reduces memory footprint and number of connections to the server.
func NewLocalQueueInformer(client versioned.Interface, namespace string, resyncPeriod time.Duration, indexers cache.Indexers) cache.SharedIndexInformer {
	return NewFilteredLocalQueueInformer(client, namespace, resyncPeriod, indexers, nil)
}

// NewFilteredLocalQueueInformer constructs a new informer for LocalQueue type.
// Always prefer using an informer factory to get a shared informer instead of getting an independent
// one. This reduces memory footprint and number of connections to the server.
func NewFilteredLocalQueueInformer(client versioned.Interface, namespace string, resyncPeriod time.Duration, indexers cache.Indexers, tweakListOptions internalinterfaces.TweakListOptionsFunc) cache.SharedIndexInformer {
	return cache.NewSharedIndexInformer(
		&cache.ListWatch{
			ListFunc: func(options v1.ListOptions) (runtime.Object, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.VisibilityV1alpha1().LocalQueues(namespace).List(context.TODO(), options)
			},
			WatchFunc: func(options v1.ListOptions) (watch.Interface, error) {
				if tweakListOptions != nil {
					tweakListOptions(&options)
				}
				return client.VisibilityV1alpha1().LocalQueues(namespace).Watch(context.TODO(), options)
			},
		},
		&visibilityv1alpha1.LocalQueue{},
		resyncPeriod,
		indexers,
	)
}

func (f *localQueueInformer) defaultInformer(client versioned.Interface, resyncPeriod time.Duration) cache.SharedIndexInformer {
	return NewFilteredLocalQueueInformer(client, f.namespace, resyncPeriod, cache.Indexers{cache.NamespaceIndex: cache.MetaNamespaceIndexFunc}, f.tweakListOptions)
}

func (f *localQueueInformer) Informer() cache.SharedIndexInformer {
	return f.factory.InformerFor(&visibilityv1alpha1.LocalQueue{}, f.defaultInformer)
}

func (f *localQueueInformer) Lister() v1alpha1.LocalQueueLister {
	return v1alpha1.NewLocalQueueLister(f.Informer().GetIndexer())
}