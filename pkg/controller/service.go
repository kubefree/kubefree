package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func (c *controller) checkService(service *v1.Service) error {
	ac, ok := service.Annotations[c.ActivityStatusAnnotation]
	if !ok || ac == "" {
		// activity status not exists, do nothing
		return nil
	}

	activity, err := getActivity(ac)
	if err != nil {
		return err
	}

	logrus.Debugf("pick service %s, last activity %v", service.Name, activity.LastActivityTime)

	s := &serviceController{controller: *c}

	if err := s.syncDeleteAfterRules(service, *activity); err != nil {
		logrus.WithError(err).Errorln("Error SyncDeleteAfterRules")
		return err
	}

	if err := s.syncSleepAfterRules(service, *activity); err != nil {
		logrus.WithError(err).Errorln("Error SyncSleepAfterRules")
		return err
	}

	return nil
}

type serviceController struct {
	controller
}

func (sc *serviceController) syncDeleteAfterRules(service *v1.Service, lastActivity activity) error {
	v, ok := service.Labels[sc.DeleteAfterSelector]
	if !ok || v == "" {
		// namespace doesn't have delete-after label, do nothing
		return nil
	}

	nl := logrus.WithField("namespace", service.Namespace).
		WithField("service", service.Name).
		WithField("lastActivityTime", lastActivity.LastActivityTime).
		WithField("delete-after", v)

	nl.Debug("checking delete-after rules")

	thresholdDuration, err := time.ParseDuration(v)
	if err != nil {
		return fmt.Errorf("time.ParseDuration failed, label %s, value %s", sc.DeleteAfterSelector, v)
	}

	if time.Since(lastActivity.LastActivityTime.Time()) > thresholdDuration {
		if err := sc.clientset.CoreV1().Services(service.Namespace).Delete(context.TODO(), service.Name, metav1.DeleteOptions{}); err != nil {
			return fmt.Errorf("failed to delete service %s/%s, with err %v", service.Namespace, service.Name, err)
		}
		nl.Info("delete service successfully")
	}

	return nil
}

// TODO(CarlJi): implement the syncSleepAfterRules method
func (sc *serviceController) syncSleepAfterRules(service *v1.Service, lastActivity activity) error {
	v, ok := service.Labels[sc.SleepAfterSelector]
	if !ok || v == "" {
		// namespace doesn't have sleep-after label, do nothing
		return nil
	}

	nl := logrus.WithField("namespace", service.Namespace).
		WithField("service", service.Name).
		WithField("lastActivityTime", lastActivity.LastActivityTime).
		WithField("sleep-after", v)

	nl.Debug("checking sleep-after rules")

	return nil
}
