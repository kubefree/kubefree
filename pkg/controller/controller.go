package plank

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/util/wait"
	cacheddiscovery "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/scale"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
)

type controller struct {
	clientset *kubernetes.Clientset
	sleeper   Sleeper

	indexer  cache.Indexer
	informer cache.Controller
	queue    workqueue.RateLimitingInterface

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

	sleepNamespaceListWatcher := cache.NewListWatchFromClient(clientset.CoreV1().RESTClient(), "namespaces", v1.NamespaceAll, fields.Everything())
	// create the workqueue
	queue := workqueue.NewRateLimitingQueue(workqueue.DefaultControllerRateLimiter())
	// Bind the workqueue to a cache with the help of an informer. This way we make sure that
	// whenever the cache is updated, the pod key is added to the workqueue.
	// Note that when we finally process the item from the workqueue, we might see a newer version
	// of the Pod than the version which was responsible for triggering the update.
	indexer, informer := cache.NewIndexerInformer(sleepNamespaceListWatcher, &v1.Namespace{}, resyncDuration, cache.ResourceEventHandlerFuncs{
		AddFunc: func(obj interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(obj)
			if err == nil {
				queue.Add(key)
			}
		},
		UpdateFunc: func(old interface{}, new interface{}) {
			key, err := cache.MetaNamespaceKeyFunc(new)
			if err == nil {
				queue.Add(key)
			}
		},
		DeleteFunc: func(obj interface{}) {
			// IndexerInformer uses a delta queue, therefore for deletes we have to use this
			// key function.
			key, err := cache.DeletionHandlingMetaNamespaceKeyFunc(obj)
			if err == nil {
				// no need to handle this object again
				queue.Forget(key)
			}
		},
	}, cache.Indexers{})

	return &controller{
		clientset:                clientset,
		sleeper:                  NewSleeper(scaler, clientset),
		indexer:                  indexer,
		informer:                 informer,
		queue:                    queue,
		DeleteAfterSelector:      DeleteAfterLabel,
		SleepAfterSelector:       SleepAfterLabel,
		ActivityStatusAnnotation: NamespaceActivityStatusAnnotation,
		ExecutionStateSelector:   ExecutionStateLabel,
	}, nil
}

func (c *controller) Run(workers int, stopCh chan struct{}) {
	defer runtime.HandleCrash()

	// Let the workers stop when we are done
	defer c.queue.ShutDown()
	klog.Info("starting kubefree controller")

	go c.informer.Run(stopCh)

	if !cache.WaitForCacheSync(stopCh, c.informer.HasSynced) {
		runtime.HandleError(fmt.Errorf("%s", "Time out waitting for cache synced"))
		return
	}

	for i := 0; i < workers; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
	klog.Info("Stopping kubefree controller")
}

func (c *controller) runWorker() {
	for c.processNextItem() {
	}
}

func (c *controller) processNextItem() bool {
	// Wait until there is a new item in the working queue
	key, quit := c.queue.Get()
	if quit {
		return false
	}

	// Tell the queue that we are done with processing this key. This unblocks the key for other workers
	// This allows safe parallel processing because two pods with the same key are never processed in
	// parallel.
	defer c.queue.Done(key)

	err := c.processItem(key.(string))
	c.handleErr(err, key)

	return true
}

func (c *controller) processItem(key string) error {
	obj, exists, err := c.indexer.GetByKey(key)
	if err != nil {
		logrus.WithError(err).Infof("Fetching object with key %v from store failed", key)
		return err
	}

	if !exists {
		return nil
	}

	// no need to handle namespace in terminating state
	ns := obj.(*v1.Namespace)
	if ns.Status.Phase != v1.NamespaceActive {
		return nil
	}

	if err := c.checkNamespace(obj.(*v1.Namespace)); err != nil {
		logrus.WithError(err).Infof("checkNamespace failed, ns: %s", key)
		return err
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
		klog.Infof("Error syncing namespace %v: %v", key, err)

		// Re-enqueue the key rate limited. Based on the rate limiter on the
		// queue and the re-enqueue history, the key will be processed later again.
		c.queue.AddRateLimited(key)
		return
	}

	c.queue.Forget(key)
	// Report to an external entity that, even after several retries, we could not successfully process this key
	runtime.HandleError(err)
	klog.Infof("Dropping namespace %q out of the queue: %v", key, err)
}

func (c *controller) checkNamespace(ns *v1.Namespace) error {
	ac, ok := ns.Annotations[c.ActivityStatusAnnotation]
	if !ok || ac == "" {
		// activity status not exists, do nothing
		return nil
	}

	lastActivityStatus, err := getActivity(ac)
	if err != nil {
		logrus.WithError(err).Fatalln("Error getActivity")
		return err
	}

	logrus.Debugf("pick namespace %s", ns.Name)

	// check delete-after-seconds rules
	if err = c.syncDeleteAfterRules(ns, *lastActivityStatus); err != nil {
		logrus.WithError(err).Fatalln("Error SyncDeleteAfterRules")
		return err
	}

	// check sleep-after rules
	if err = c.syncSleepAfterRules(ns, *lastActivityStatus); err != nil {
		logrus.WithError(err).Fatalln("Error SyncSleepAfterRules")
		return err
	}

	return nil
}

// target sleep-after rules
func (c *controller) syncSleepAfterRules(namespace *v1.Namespace, lastActivity activity) error {
	v, ok := namespace.Labels[c.SleepAfterSelector]
	if !ok || v == "" {
		// namespace doesn't have sleep-after label, do nothing
		return nil
	}
	logrus.WithField("namespace", namespace.Name).WithField("sleep-after", v).Debug("check it's sleep-after rule")

	thresholdDuration, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("time.ParseDuration failed, label %s, value %s", c.SleepAfterSelector, v)
	}

	state := namespace.Labels[c.ExecutionStateSelector]
	if time.Since(lastActivity.LastActivityTime) > thresholdDuration {
		switch state {
		case DELETING:
			// do nothing
			// TODO: how to deal it if namespace hangs?
		case SLEEP, SLEEPING:
			// delete namespace if the namespace still in inactivity status
			// after thresholdDuration * 2 time
			if namespace.Status.Phase != v1.NamespaceTerminating && time.Since(lastActivity.LastActivityTime) > thresholdDuration*2 {
				logrus.WithField("namespace", namespace.Name).
					WithField("lastActivityTime", lastActivity.LastActivityTime).
					WithField("sleep-after", v).Info("delete inactivity namespace")
				if !c.DryRun {
					err = c.clientset.CoreV1().Namespaces().Delete(context.TODO(), namespace.Name, metav1.DeleteOptions{})
					if err != nil {
						logrus.WithField("namespace", namespace.Name).Error("Error delete namespace")
						return err
					}
				}
			}
		default:
			/*
				Enter into sleep mode
				step1: update execution state with sleeping
				step2: sleep namespace
				step3: update execution state with sleep
			*/
			logrus.WithField("namespace", namespace.Name).
				WithField("lastActivityTime", lastActivity.LastActivityTime).
				WithField("sleep-after", v).Info("sleep inactivity namespace")
			if _, err := c.setKubefreeExecutionState(namespace, SLEEPING); err != nil {
				logrus.WithField("namespace", namespace.Name).WithError(err).Error("failed to SetKubefreeExecutionState")
			}
			if !c.DryRun {
				if err := c.sleeper.Sleep(namespace); err != nil {
					logrus.WithField("namespace", namespace.Name).WithError(err).Error("failed to sleep namespace")
				}
			}
			if _, err := c.setKubefreeExecutionState(namespace, SLEEP); err != nil {
				logrus.WithField("namespace", namespace.Name).WithError(err).Error("failed to SetKubefreeExecutionState")
			}
		}
	} else {
		switch state {
		case SLEEPING, SLEEP:
			// TODO: do recovery
			// 这块需要考虑服务的启动顺序。
			// 也许并发的恢复所有的服务，出错的概率会小？
			// klog.Info("recovery namespace", namespace.Name)
			// if _, err := c.SetKubefreeExecutionState(namespace, NORMAL); err != nil {
			// 	logrus.WithField("namespace", namespace.Name).WithError(err).Error("failed to SetKubefreeExecutionState")
			// }
		default:
			// still in activity time scope,so do nothing
		}
	}
	return nil
}

// target delete-after rules
func (c *controller) syncDeleteAfterRules(namespace *v1.Namespace, lastActivity activity) error {
	v, ok := namespace.Labels[c.DeleteAfterSelector]
	if !ok || v == "" {
		// namespace doesn't have delete-after label, do nothing
		return nil
	}
	logrus.WithField("namespace", namespace.Name).WithField("delete-after", v).Debug("check it's delete-after rule")

	thresholdDuration, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("time.ParseDuration failed, label %s, value %s", c.DeleteAfterSelector, v)
	}

	if time.Since(lastActivity.LastActivityTime) > thresholdDuration && namespace.Status.Phase != v1.NamespaceTerminating {
		logrus.WithField("namespace", namespace.Name).
			WithField("lastActivityTime", lastActivity.LastActivityTime).
			WithField("delete-after", v).Info("deleting inactivity namespace")
		if !c.DryRun {
			if err != c.clientset.CoreV1().Namespaces().Delete(context.Background(), namespace.Name, metav1.DeleteOptions{}) {
				logrus.WithField("namespace", namespace.Name).Error("Error delete namespace")
				return err
			}
		}
	}
	return nil
}

func (c *controller) setKubefreeExecutionState(namespace *v1.Namespace, state string) (*v1.Namespace, error) {
	ns, err := c.clientset.CoreV1().Namespaces().Get(context.TODO(), namespace.Name, metav1.GetOptions{})
	if err != nil {
		return nil, err
	}

	oldNsData, err := json.Marshal(ns)
	if err != nil {
		return nil, err
	}

	if ns.Labels == nil {
		ns.Labels = make(map[string]string)
	}
	ns.Labels[c.ExecutionStateSelector] = state
	newNsData, err := json.Marshal(ns)
	if err != nil {
		return nil, err
	}

	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldNsData, newNsData, &v1.Namespace{})
	if err != nil {
		return nil, err
	}

	result, err := c.clientset.CoreV1().Namespaces().Patch(context.TODO(), namespace.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		klog.V(2).InfoS("failed to patch annotation for namespace", "namespace", namespace.Name, "err", err)
		return nil, err
	}
	return result, nil
}
