package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
	appsv1 "k8s.io/api/apps/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/strategicpatch"
	"k8s.io/apimachinery/pkg/util/wait"
	cacheddiscovery "k8s.io/client-go/discovery/cached/memory"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/scale"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
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
		clientset:                clientset,
		scalesGetter:             scaler,
		queue:                    queue,
		DeleteAfterSelector:      DeleteAfterLabel,
		SleepAfterSelector:       SleepAfterLabel,
		ActivityStatusAnnotation: NamespaceActivityStatusAnnotation,
		ExecutionStateSelector:   ExecutionStateLabel,

		namespaceInformer:  namespaceInformer,
		deploymentInformer: deploymentInformer,
		serviceInformer:    serviceInformer,
	}, nil
}

func (c *controller) Run(workers int, stopCh chan struct{}) {
	defer runtime.HandleCrash()

	// Let the workers stop when we are done
	defer c.queue.ShutDown()
	klog.Info("starting kubefree controller")

	if !cache.WaitForCacheSync(stopCh, c.namespaceInformer.HasSynced, c.deploymentInformer.HasSynced, c.serviceInformer.HasSynced) {
		runtime.HandleError(fmt.Errorf("%s", "Time out waiting for cache synced"))
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
			logrus.WithError(err).Infof("checkNamespace failed, ns: %s", v.Name)
			// Do not handle when namespace check fails, instead of exiting the program
			return nil
		}
	case *appsv1.Deployment:
		// logrus.Debugf("deployment processItem: %s", v.Name)
		// no need to handle the deployment if it's not active
		if err := c.checkDeployment(v); err != nil {
			logrus.WithError(err).Infof("checkDeployment failed, deployment: %s", v.Name)
			return nil
		}
	case *v1.Service:
		// logrus.Debugf("service processItem: %s", v.Name)
		if err := c.checkService(v); err != nil {
			logrus.WithError(err).Infof("checkService failed, service: %s", v.Name)
			return nil
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

// target sleep-after rules
func (c *controller) syncSleepAfterRules(namespace *v1.Namespace, lastActivity activity) error {
	v, ok := namespace.Labels[c.SleepAfterSelector]
	if !ok || v == "" {
		// namespace doesn't have sleep-after label, do nothing
		return nil
	}

	thresholdDuration, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("time.ParseDuration failed, label %s, value %s", c.SleepAfterSelector, v)
	}

	state := namespace.Labels[c.ExecutionStateSelector]
	if time.Since(lastActivity.LastActivityTime.Time()) > thresholdDuration {
		switch state {
		case DELETING:
			// do nothing
			// TODO: how to deal it if namespace hangs?
		case SLEEP, SLEEPING:
			// 当存在delete标签时不走此处的删除逻辑
			v, ok := namespace.Labels[c.DeleteAfterSelector]
			if ok && v != "" {
				logrus.WithField("namespace", namespace.Name).WithField("delete-after", v).Debug("skip delete namespace")
				return nil
			}
			// delete namespace if the namespace still in inactivity status
			// after thresholdDuration * 2 time
			if namespace.Status.Phase != v1.NamespaceTerminating && time.Since(lastActivity.LastActivityTime.Time()) > thresholdDuration*2 {
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
				logrus.WithField("namespace", namespace.Name).WithError(err).Fatal("failed to SetKubefreeExecutionState")
			}
			if !c.DryRun {
				if err := c.Sleep(namespace); err != nil {
					logrus.WithField("namespace", namespace.Name).WithError(err).Fatal("failed to sleep namespace")
				}
			}
			if _, err := c.setKubefreeExecutionState(namespace, SLEEP); err != nil {
				logrus.WithField("namespace", namespace.Name).WithError(err).Fatal("failed to SetKubefreeExecutionState")
			}
		}
	} else {
		switch state {
		case SLEEPING, SLEEP:
			klog.Infof("wake up namespace %s", namespace.Name)
			if !c.DryRun {
				if err := c.WakeUp(namespace); err != nil {
					logrus.WithField("namespace", namespace.Name).WithError(err).Errorln("failed to wake up namespace")
				}
			}

			if _, err := c.setKubefreeExecutionState(namespace, NORMAL); err != nil {
				logrus.WithField("namespace", namespace.Name).WithError(err).Errorln("failed to SetKubefreeExecutionState")
			}
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

	thresholdDuration, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("time.ParseDuration failed, label %s, value %s", c.DeleteAfterSelector, v)
	}

	if time.Since(lastActivity.LastActivityTime.Time()) > thresholdDuration && namespace.Status.Phase != v1.NamespaceTerminating {
		// Delete the namespace
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
