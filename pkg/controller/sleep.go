package plank

import (
	"context"
	"fmt"
	"strconv"

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
	WakeUp(ns string) error
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
		newD.Annotations[LegacyReplicasAnnotation] = strconv.FormatInt(int64(*newD.Spec.Replicas), 10)
		_, err := s.clientset.AppsV1().Deployments(ns.Name).Update(context.TODO(), newD, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to set annotation for deployment %s, err: %v", d.Name, err)
		}

		targetScale := &autoscalingv1.Scale{
			ObjectMeta: metav1.ObjectMeta{
				Name:      d.Name,
				Namespace: d.Namespace,
			},
			Spec: autoscalingv1.ScaleSpec{
				Replicas: 0,
			},
		}
		_, err = s.sleepClient.Scales(ns.Name).Update(context.TODO(), schema.GroupResource{Group: "apps", Resource: "deployments"}, targetScale, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to update scale for deployment %s, with err %v", d.Name, err)
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
		newSs.Annotations[LegacyReplicasAnnotation] = strconv.FormatInt(int64(*newSs.Spec.Replicas), 10)
		_, err = s.clientset.AppsV1().StatefulSets(ns.Name).Update(context.TODO(), newSs, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to set annotation for statefulset %s, err: %v", ss.Name, err)
		}

		targetScale := &autoscalingv1.Scale{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ss.Name,
				Namespace: ss.Namespace,
			},
			Spec: autoscalingv1.ScaleSpec{
				Replicas: 0,
			},
		}
		_, err := s.sleepClient.Scales(ns.Name).Update(context.TODO(), schema.GroupResource{Group: "apps", Resource: "statefulsets"}, targetScale, metav1.UpdateOptions{})
		if err != nil {
			return fmt.Errorf("failed to update scale for statefulset %s, with err %v", ss.Name, err)
		}
		logrus.WithField("namespace", ns.Name).WithField("statefulset", ss.Name).Info("sleep statefulset successfully")
	}

	// TODO: others?

	return nil
}

//TODO
func (s *genericSleeper) WakeUp(namespace string) error {
	// 依次判断deployment、statefulset、deamonset，每个执行以下操作
	// 	1. 判断是否有legacy replicas，如果没有则报错
	//  2. 设置 replicas == legacy replicas
	//  3. 删除 legacy replicas

	return nil
}
