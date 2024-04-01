package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/qiniu/x/log"
	appsv1 "k8s.io/api/apps/v1"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
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
			queue.Forget(old.(*v1.Namespace))
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
			queue.Forget(old.(*appsv1.Deployment))
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
		// no need to handle the namespace if it's not active
		if v.Status.Phase != v1.NamespaceActive {
			return nil
		}
		if err := c.checkNamespace(v); err != nil {
			log.Errorf("checkNamespace failed, ns: %v, err: %v", v, err)
			return err
		}
	case *appsv1.Deployment:
		if err := c.checkDeployment(v); err != nil {
			log.Errorf("checkDeployment failed, deployment: %v, err: %v", v, err)
			return err
		}
	case *v1.Service:
		if err := c.checkService(v); err != nil {
			log.Errorf("checkService failed, service: %v, err: %v", v, err)
			return err
		}
	default:
		log.Infof("unknown object: %v", v)
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

	if strings.Contains(err.Error(), "not found") {
		// No need to retry if the resource is not found
		// Even if this is not a exact 'not found' error, we can still forget the key since it will be requeued
		// after next resync.
		c.queue.Forget(key)
		return
	}

	// This controller retries 5 times if something goes wrong. After that, it stops trying.
	if c.queue.NumRequeues(key) < 5 {
		log.Errorf("Error syncing %v: %v", key, err)

		// Re-enqueue the key rate limited. Based on the rate limiter on the
		// queue and the re-enqueue history, the key will be processed later again.
		c.queue.AddRateLimited(key)
		return
	}

	c.queue.Forget(key)
	// Report to an external entity that, even after several retries, we could not successfully process this key
	runtime.HandleError(err)
	log.Errorf("failed to handle %v, err: %v, dropping this key from the queue", key, err)
}

func (c *controller) patchDeploymentWithAnnotation(d *appsv1.Deployment, annotation, value string) (*appsv1.Deployment, error) {
	if d == nil {
		return nil, fmt.Errorf("unexpected deployment as nil")
	}
	oldDeploymentData, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}

	newD := d.DeepCopy()
	if newD.Annotations == nil {
		newD.Annotations = make(map[string]string)
	}

	newD.Annotations[annotation] = value
	newDeploymentData, err := json.Marshal(newD)
	if err != nil {
		return nil, err
	}

	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldDeploymentData, newDeploymentData, &appsv1.Deployment{})
	if err != nil {
		return nil, err
	}

	result, err := c.clientset.AppsV1().Deployments(d.Namespace).Patch(context.TODO(), d.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		log.Errorf("failed to patch annotation for deployment, deployment: %v, err: %v ", d.Name, err)
		return nil, err
	}
	return result, nil
}

func (c *controller) patchStatefulsetWithAnnotation(s *appsv1.StatefulSet, annotation, value string) (*appsv1.StatefulSet, error) {
	if s == nil {
		return nil, fmt.Errorf("unexpected statefulset as nil")
	}

	oldData, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}

	newD := s.DeepCopy()
	if newD.Annotations == nil {
		newD.Annotations = make(map[string]string)
	}

	newD.Annotations[annotation] = value
	newData, err := json.Marshal(newD)
	if err != nil {
		return nil, err
	}

	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldData, newData, &appsv1.StatefulSet{})
	if err != nil {
		return nil, err
	}

	result, err := c.clientset.AppsV1().StatefulSets(s.Namespace).Patch(context.TODO(), s.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		log.Errorf("failed to patch annotation for statefulset, statefulset: %v, err: %v ", s.Name, err)
		return nil, err
	}
	return result, nil
}

// hardcode flag for sleep daemonset
var defaultExpression = v1.NodeSelectorRequirement{
	Key:      "sleep.kubefree.com/not-existing-key",
	Operator: v1.NodeSelectorOpIn,
	Values:   []string{"sleep"},
}

func (c *controller) patchDaemonsetWithNodeAffinity(s *appsv1.DaemonSet, annotation, value string) (*appsv1.DaemonSet, error) {
	if s == nil {
		return nil, fmt.Errorf("unexpected statefulset as nil")
	}

	oldData, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}

	newD := s.DeepCopy()
	if newD.Annotations == nil {
		newD.Annotations = make(map[string]string)
	}

	// still patch daemonset with legacy-replicas annotation
	newD.Annotations[annotation] = value

	if newD.Spec.Template.Spec.Affinity == nil {
		newD.Spec.Template.Spec.Affinity = &v1.Affinity{}
	}

	if newD.Spec.Template.Spec.Affinity.NodeAffinity == nil {
		newD.Spec.Template.Spec.Affinity.NodeAffinity = &v1.NodeAffinity{}
	}

	if newD.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		newD.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = &v1.NodeSelector{}
	}

	if newD.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms == nil {
		newD.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = []v1.NodeSelectorTerm{}
	}

	selectorTerm := v1.NodeSelectorTerm{}
	selectorTerm.MatchExpressions = append(selectorTerm.MatchExpressions, defaultExpression)
	newD.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = append(newD.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms, selectorTerm)

	newData, err := json.Marshal(newD)
	if err != nil {
		return nil, err
	}

	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldData, newData, &appsv1.DaemonSet{})
	if err != nil {
		return nil, err
	}

	result, err := c.clientset.AppsV1().DaemonSets(s.Namespace).Patch(context.TODO(), s.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		log.Errorf("failed to patch annotation for daemonsets, daemonsets: %v, err: %v", s.Name, err)
		return nil, err
	}

	return result, nil
}

func (c *controller) patchDaemonsetWithoutSpecificNodeAffinity(s *appsv1.DaemonSet, key, value string) (*appsv1.DaemonSet, error) {
	if s == nil {
		return nil, fmt.Errorf("unexpected statefulset as nil")
	}

	oldData, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}

	newD := s.DeepCopy()
	if newD.Spec.Template.Spec.Affinity == nil ||
		newD.Spec.Template.Spec.Affinity.NodeAffinity == nil ||
		newD.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil ||
		newD.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms == nil ||
		len(newD.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms) == 0 {

		return nil, nil
	}

	var newSelectorTerm = []v1.NodeSelectorTerm{}
	for _, st := range newD.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms {
		var newExpression = []v1.NodeSelectorRequirement{}
		for _, express := range st.MatchExpressions {
			// remove default expression patched by kubefree
			if express.Key != defaultExpression.Key {
				newExpression = append(newExpression, express)
			}
		}

		if len(newExpression) != 0 {
			var newNodeSelectorTerm = st
			newNodeSelectorTerm.MatchExpressions = newExpression
			newSelectorTerm = append(newSelectorTerm, newNodeSelectorTerm)
		}
	}

	if len(newSelectorTerm) != 0 {
		newD.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms = newSelectorTerm
	} else {
		newD.Spec.Template.Spec.Affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution = nil
	}

	newData, err := json.Marshal(newD)
	if err != nil {
		return nil, err
	}

	patchBytes, err := strategicpatch.CreateTwoWayMergePatch(oldData, newData, &appsv1.DaemonSet{})
	if err != nil {
		return nil, err
	}

	result, err := c.clientset.AppsV1().DaemonSets(s.Namespace).Patch(context.TODO(), s.Name, types.StrategicMergePatchType, patchBytes, metav1.PatchOptions{})
	if err != nil {
		log.Errorf("failed to patch annotation for daemonsets, daemonsets: %v, err: %v ", s.Name, err)
		return nil, err
	}

	return result, nil
}

func (c *controller) scale(ctx context.Context, name, namespace, scale string, resource schema.GroupResource) (*autoscalingv1.Scale, error) {
	i, err := strconv.ParseInt(scale, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("strconv.ParseInt failed for scale %s/%s", namespace, name)
	}

	targetScale := &autoscalingv1.Scale{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: autoscalingv1.ScaleSpec{
			Replicas: int32(i),
		},
	}

	return c.scalesGetter.Scales(namespace).Update(ctx, resource, targetScale, metav1.UpdateOptions{})
}
