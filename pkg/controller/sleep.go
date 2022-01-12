package plank

import (
	"context"
	"fmt"
	"strconv"
	"sync"

	"github.com/sirupsen/logrus"
	autoscalingv1 "k8s.io/api/autoscaling/v1"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/scale"
)

// method to scale workload's replica to 0 or recovery
type Sleeper interface {
	Sleep(ns *v1.Namespace) error
	WakeUp(ns *v1.Namespace) error
}

func NewSleeper(scalesGetter scale.ScalesGetter, clientset *kubernetes.Clientset) Sleeper {
	return &genericSleeper{sleepClient: scalesGetter, clientset: clientset}
}

type genericSleeper struct {
	sleepClient scale.ScalesGetter
	clientset   *kubernetes.Clientset
}

var _ Sleeper = &genericSleeper{}

func (s *genericSleeper) Sleep(ns *v1.Namespace) error {
	// 依次判断deployment、statefulset、deamonset，每个执行以下操作
	// 	1. 判断是否有legacy replicas，如果有则报错
	// 	2. 获得原来的replicas，并保存为legacy replicas annotation
	//  3. 设置 replicas ==0

	//TODO: refactor
	// deployment
	deploymentLists, err := s.clientset.AppsV1().Deployments(ns.Name).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list deployments, with err %v", err)
	}

	for _, d := range deploymentLists.Items {
		if v, ok := d.Annotations[LegacyReplicasAnnotation]; ok && v != "" {
			return fmt.Errorf("deployment %s already has legacy replicas annotation %s", d.Name, LegacyReplicasAnnotation)
		}

		newD := d.DeepCopy()
		if newD.Annotations == nil {
			newD.Annotations = make(map[string]string)
		}
		newD.Annotations[LegacyReplicasAnnotation] = strconv.FormatInt(int64(*newD.Spec.Replicas), 10)
		_, err := s.clientset.AppsV1().Deployments(ns.Name).Update(context.TODO(), newD, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to set annotation for deployment %s, err: %v", d.Name, err)
		}
		_, err = s.scale(context.TODO(), d.Name, d.Namespace, "0", schema.GroupResource{Group: "apps", Resource: "deployments"})
		if err != nil {
			return fmt.Errorf("failed to update scale for deployment %s/%s, with err %v", d.Namespace, d.Name, err)
		}

		logrus.WithField("namespace", ns.Name).WithField("deployment", d.Name).Info("sleep deployment successfully")
	}

	// statefulset
	ssLists, err := s.clientset.AppsV1().StatefulSets(ns.Name).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list StatefulSets, with err %v", err)
	}

	for _, ss := range ssLists.Items {
		if v, ok := ss.Annotations[LegacyReplicasAnnotation]; ok && v != "" {
			return fmt.Errorf("statefulset %s already has legacy replicas annotation %s", ss.Name, LegacyReplicasAnnotation)
		}

		newSs := ss.DeepCopy()
		if newSs.Annotations == nil {
			newSs.Annotations = make(map[string]string)
		}
		newSs.Annotations[LegacyReplicasAnnotation] = strconv.FormatInt(int64(*newSs.Spec.Replicas), 10)
		_, err = s.clientset.AppsV1().StatefulSets(ns.Name).Update(context.TODO(), newSs, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to set annotation for statefulset %s, err: %v", ss.Name, err)
		}

		_, err = s.scale(context.TODO(), ss.Name, ss.Namespace, "0", schema.GroupResource{Group: "apps", Resource: "statefulsets"})
		if err != nil {
			return fmt.Errorf("failed to update scale for statefulset %s/%s, with err %v", ss.Namespace, ss.Name, err)
		}
		logrus.WithField("namespace", ns.Name).WithField("statefulset", ss.Name).Info("sleep statefulset successfully")
	}

	// TODO: others?

	return nil
}

//TODO
func (s *genericSleeper) WakeUp(ns *v1.Namespace) error {
	// 依次判断deployment、statefulset、deamonset，每个执行以下操作
	// 	1. 判断是否有legacy replicas，如果没有则报错
	//  2. 设置 replicas == legacy replicas
	//  3. 删除 legacy replicas
	// 另，因为pod原始可能会有启动顺序依赖，为降低错误率，此处采用
	// 并行恢复所有的workloads

	workloadWg := &sync.WaitGroup{}

	// deployment
	deploymentLists, err := s.clientset.AppsV1().Deployments(ns.Name).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list deployments, with err %v", err)
	}
	if deploymentLists != nil {
		workloadWg.Add(len(deploymentLists.Items))
	}

	// statefulset
	ssLists, err := s.clientset.AppsV1().StatefulSets(ns.Name).List(context.TODO(), metav1.ListOptions{})
	if err != nil {
		return fmt.Errorf("failed to list StatefulSets, with err %v", err)
	}
	if ssLists != nil {
		workloadWg.Add(len(ssLists.Items))
	}

	for _, d := range deploymentLists.Items {
		v, ok := d.Annotations[LegacyReplicasAnnotation]
		if !ok || len(v) == 0 {
			return fmt.Errorf("unexpected legacy replicas annotation %s for the deployment %s", LegacyReplicasAnnotation, d.Name)
		}

		newD := d.DeepCopy()
		if newD.Annotations == nil {
			newD.Annotations = make(map[string]string)
		}
		newD.Annotations[LegacyReplicasAnnotation] = strconv.FormatInt(int64(*newD.Spec.Replicas), 10)
		_, err := s.clientset.AppsV1().Deployments(ns.Name).Update(context.TODO(), newD, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to set annotation for deployment %s, err: %v", d.Name, err)
		}

		_, err = s.scale(context.TODO(), d.Name, d.Namespace, v, schema.GroupResource{Group: "apps", Resource: "deployments"})
		if err != nil {
			return fmt.Errorf("failed to update scale for deployment %s/%s, with err %v", d.Namespace, d.Name, err)
		}

		logrus.WithField("namespace", ns.Name).WithField("deployment", d.Name).Info("sleep deployment successfully")
	}

	return nil
}

func (s *genericSleeper) scale(ctx context.Context, name, namespace, scale string, resource schema.GroupResource) (*autoscalingv1.Scale, error) {
	i, err := strconv.ParseInt(scale, 10, 32)
	if err != nil {
		return nil, fmt.Errorf("strconv.ParseInt failed for scale %s", scale)
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

	return s.sleepClient.Scales(namespace).Update(ctx, resource, targetScale, metav1.UpdateOptions{})
}
