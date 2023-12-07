package event

import (
	"context"
	"encoding/json"
	"github.com/kubefree/pkg/controller"
	"github.com/sirupsen/logrus"
	"gopkg.in/yaml.v3"
	"io/ioutil"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/kubernetes"
	"regexp"
	"time"
)

type MatchRule struct {
	Name   string            `yaml:"name"`
	Labels map[string]string `yaml:"labels"`
}
type Config struct {
	MatchRule       []MatchRule `yaml:"matchRule"`
	SleepAfterTime  string      `yaml:"sleepAfterTime"`
	DeleteAfterTime string      `yaml:"deleteAfterTime"`
}

func loadConfig(filePath string) (*Config, error) {
	// Read config file
	data, err := ioutil.ReadFile(filePath)
	if err != nil {
		return nil, err
	}
	// Unmarshal YAML data into config struct
	var config Config
	if err := yaml.Unmarshal(data, &config); err != nil {
		return nil, err
	}
	return &config, nil
}

func labelsMatch(ruleLabels map[string]string, namespaceLabels map[string]string) bool {
	for key, value := range ruleLabels {
		if namespaceLabels[key] != value {
			return false
		}
	}
	return true
}

func matchNamespaceName(ruleName string, namespaceName string) bool {
	match, _ := regexp.MatchString(ruleName, namespaceName)
	return match
}

func HandleEvents(ctx context.Context, clientset *kubernetes.Clientset) {
	startTime := time.Now()
	workload := "pod"
	// Watch for events on Deployments, StatefulSets, and DaemonSets
	needCreateEvent := true
	var eventChan <-chan watch.Event

	for {
		if needCreateEvent {
			startTime = time.Now()
			podWatcher, err := clientset.CoreV1().Pods("").Watch(ctx, metav1.ListOptions{})
			if err != nil {
				logrus.Fatal("create pod Watcher failure")
			}
			logrus.Debug("start")
			// 创建一个用于通知主线程退出的通道
			eventChan = podWatcher.ResultChan()
		}

		needCreateEvent = false

		select {
		case event, ok := <-eventChan:
			if !ok {
				// 事件通道已关闭，跳出循环
				logrus.Error("event chan exit...continue the loop")
				needCreateEvent = true
				continue
			}
			eventTime := event.Object.(*corev1.Pod).CreationTimestamp.Time

			// 如果Pod启动时间在程序启动前，则放弃处理事件
			if eventTime.Before(startTime) {
				// 处理事件逻辑
				logrus.Debugf("pod %s create time before program start, skipping setting", event.Object.(*corev1.Pod).Name)
				continue
			}

			switch event.Type {
			case watch.Added:
				handleEvent(ctx, clientset, workload, "Added", event.Object)
			case watch.Error:
				handleError(workload, event.Object)
			}

		case <-ctx.Done():
			// 收到取消信号，提前退出
			return
		}
	}
}

func handleEvent(ctx context.Context, clientset *kubernetes.Clientset, workload, action string, obj interface{}) {
	// 根据不同的工作负载资源进行处理
	namespaceName := ""
	podName := ""
	switch workload {
	case "pod":
		pod, ok := obj.(*corev1.Pod)
		if !ok {
			logrus.Error("Failed to cast event object to deployment")
			return
		}
		namespaceName = pod.Namespace
		podName = pod.Name
		logrus.Debugf("listen event Added, namespace: %s, pod: %s", namespaceName, podName)

	}
	if err := setNamespaceLabelsAnnotations(ctx, clientset, namespaceName, action, podName); err != nil {
		logrus.Error(err)
	}
}

func handleError(workload string, obj interface{}) {
	err, ok := obj.(error)
	if !ok {
		logrus.Errorf("Unknown error occurred for %s", workload)
		return
	}

	logrus.Errorf("Error for %s: %v\n", workload, err)
}

func setNamespaceLabelsAnnotations(ctx context.Context, clientset *kubernetes.Clientset, namespaceName string, action string, resource string) error {
	// 获取命名空间
	namespace, err := clientset.CoreV1().Namespaces().Get(ctx, namespaceName, metav1.GetOptions{})
	if err != nil {
		logrus.Errorf("Failed to get namespace: %v", err)
		return err
	}

	config, _ := loadConfig("config.yaml")

	// Check if namespace matches any rules in config
	isMatch := false
	for _, rule := range config.MatchRule {
		if matchNamespaceName(rule.Name, namespace.Name) && labelsMatch(rule.Labels, namespace.Labels) {
			// Namespace matches a rule in config, continue
			logrus.Infof("Namespace %s matches rule: %v, action: %s, workload: %s", namespaceName, rule, action, resource)
			isMatch = true
			break
		}
	}
	if !isMatch {
		logrus.Infof("Namespace %s not matches rule, action: %s, workload: %s", namespaceName, action, resource)
		return nil
	}

	if _, ok := namespace.Labels[plank.DeleteAfterLabel]; !ok {
		namespace.Labels[plank.DeleteAfterLabel] = config.DeleteAfterTime
	}
	if _, ok := namespace.Labels[plank.SleepAfterLabel]; !ok {
		namespace.Labels[plank.SleepAfterLabel] = config.SleepAfterTime
	}

	// 设置注解

	var activity *plank.Activity
	if activityString, ok := namespace.Annotations[plank.NamespaceActivityStatusAnnotation]; ok {
		activity, err = plank.GetActivity(activityString)
		if err != nil {
			return err
		}
	} else {
		activity = &plank.Activity{}
	}

	now := time.Now()
	activity.LastActivityTime = plank.CustomTime(now)
	activity.Namespace = namespaceName
	activity.Action = action
	activity.Resource = "Pod/" + resource
	activityString, _ := json.Marshal(activity)
	namespace.Annotations[plank.NamespaceActivityStatusAnnotation] = string(activityString)

	// 更新命名空间
	_ = clientset.CoreV1().RESTClient().Put().
		Resource("namespaces").
		Name(namespaceName).
		Body(namespace).
		Timeout(10 * time.Second).
		Do(ctx)
	if err != nil {
		logrus.Errorf("Failed to update namespace %v", err)
	}
	return nil
}
