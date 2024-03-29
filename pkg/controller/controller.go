package controller

import (
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	cacheddiscovery "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/scale"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
)

type controller struct {
	clientset    *kubernetes.Clientset
	scalesGetter scale.ScalesGetter

	queue workqueue.RateLimitingInterface

	deploymentInformer cache.SharedIndexInformer
	serviceInformer    cache.SharedIndexInformer
	namespaceInformer  cache.SharedIndexInformer

	DeleteAfterSelector      string
	SleepAfterSelector       string
	ActivityStatusAnnotation string
	ExecutionStateSelector   string

	DryRun bool
}

func NewController(clientset *kubernetes.Clientset, resyncDuration time.Duration) (*controller, error) {
	cachedDiscoveryClient := cacheddiscovery.NewMemCacheClient(clientset.DiscoveryClient)
	mapper := restmapper.NewDeferredDiscoveryRESTMapper(cachedDiscoveryClient)
	scaleKindResolver := scale.NewDiscoveryScaleKindResolver(clientset.DiscoveryClient)
	scaler := scale.New(clientset.RESTClient(), mapper, dynamic.LegacyAPIPathResolverFunc, scaleKindResolver)

	factory := informers.NewSharedInformerFactory(clientset, resyncDuration)
	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	namespaceInformer := factory.Core().V1().Namespaces().Informer()
	deploymentInformer := factory.Apps().V1().Deployments().Informer()
	serviceInformer := factory.Core().V1().Services().Informer()

	namespaceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			queue.Add(obj.(*v1.Namespace))
		},
		UpdateFunc: func(old, new interface{}) {
			queue.Add(new.(*v1.Namespace))
		},
		DeleteFunc: func(obj interface{}) {
			queue.Forget(obj.(*v1.Namespace))
		},
	})

	deploymentInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			queue.Add(obj.(*appsv1.Deployment))
		},
		UpdateFunc: func(old, new interface{}) {
			queue.Add(new.(*appsv1.Deployment))
		},
		DeleteFunc: func(obj interface{}) {
			queue.Forget(obj.(*appsv1.Deployment))
		},
	})

	serviceInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			queue.Add(obj.(*v1.Service))
		},
		UpdateFunc: func(old, new interface{}) {
			queue.Add(new.(*v1.Service))
		},
		DeleteFunc: func(obj interface{}) {
			queue.Forget(obj.(*v1.Service))
		},
	})

	go factory.Start(wait.NeverStop)

	return &controller{
		clientset:    clientset,
		scalesGetter: scaler,
		queue:        queue,

		namespaceInformer:  namespaceInformer,
		deploymentInformer: deploymentInformer,
		serviceInformer:    serviceInformer,

		DeleteAfterSelector:      DeleteAfterLabel,
		SleepAfterSelector:       SleepAfterLabel,
		ActivityStatusAnnotation: NamespaceActivityStatusAnnotation,
		ExecutionStateSelector:   ExecutionStateLabel,
	}, nil
}

func (c *controller) Run(workers int, stopCh chan struct{}) {
	defer runtime.HandleCrash()
	defer c.queue.ShutDown()

	if !cache.WaitForCacheSync(stopCh, c.namespaceInformer.HasSynced, c.deploymentInformer.HasSynced, c.serviceInformer.HasSynced) {
		runtime.HandleError(fmt.Errorf("%s", "Time out waiting for cache synced"))
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
}

func (c *controller) runWorker() {
	for c.processNextItem() {
	}
}

func (c *controller) processNextItem() bool {
	key, quit := c.queue.Get()
	if quit {
		return false
	}
	defer c.queue.Done(key)

	err := c.processItem(key)
	c.handleErr(err, key)

	return true
}

func (c *controller) processItem(obj interface{}) error {
	switch v := obj.(type) {
	case *v1.Namespace:
		// logrus.Debugf("Namespace processItem: %s", v.Name)
		// no need to handle the namespace if it's not active
		if v.Status.Phase != v1.NamespaceActive {
			return nil
		}
		if err := c.checkNamespace(v); err != nil {
			logrus.WithError(err).Warnf("checkNamespace failed, ns: %s", v.Name)
			return err
		}
	case *appsv1.Deployment:
		if err := c.checkDeployment(v); err != nil {
			logrus.WithError(err).Warnf("checkDeployment failed, deployment: %s", v.Name)
			return err
		}
	case *v1.Service:
		// logrus.Debugf("service processItem: %s", v.Name)
		if err := c.checkService(v); err != nil {
			logrus.WithError(err).Warnf("checkService failed, service: %s", v.Name)
			return err
		}
	default:
		logrus.Infof("unknown object type: %T", v)
	}

	return nil
}

func (c *controller) handleErr(err error, key interface{}) {
	if err == nil {
		// Forget about the #AddRateLimited history of the key on every successful synchronization.
		// This ensures that future processing of updates for this key is not delayed because of
		// an outdated error history.
		c.queue.Forget(key)
		return
	}

	// This controller retries 5 times if something goes wrong. After that, it stops trying.
	if c.queue.NumRequeues(key) < 5 {
		logrus.Infof("Error syncing %v: %v", key, err)

		// Re-enqueue the key rate limited. Based on the rate limiter on the
		// queue and the re-enqueue history, the key will be processed later again.
		c.queue.AddRateLimited(key)
		return
	}

	c.queue.Forget(key)
	// Report to an external entity that, even after several retries, we could not successfully process this key
	runtime.HandleError(err)
	logrus.Errorf("failed to handle %v, err: %v, dropping this key from the queue", key, err)
}
