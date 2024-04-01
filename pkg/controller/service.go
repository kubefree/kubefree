package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/qiniu/x/log"
	v1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

	log.Debugf("pick service %s", service.Name)

	s := &serviceController{controller: *c}

	if deleted, err := s.syncDeleteAfterRules(service, *activity); err != nil {
		log.Errorf("Error SyncDeleteAfterRules: %v", err)
		return err
	} else if deleted {
		// service has been deleted, do nothing
		return nil
	}

	if err := s.syncSleepAfterRules(service, *activity); err != nil {
		log.Errorf("Error SyncSleepAfterRules: %v", err)
		return err
	}

	return nil
}

type serviceController struct {
	controller
}

func (sc *serviceController) syncDeleteAfterRules(service *v1.Service, lastActivity activity) (deleted bool, err error) {
	v, ok := service.Labels[sc.DeleteAfterSelector]
	if !ok || v == "" {
		// namespace doesn't have delete-after label, do nothing
		return
	}

	thresholdDuration, err := time.ParseDuration(v)
	if err != nil {
		return false, fmt.Errorf("time.ParseDuration failed, label %s, value %s", sc.DeleteAfterSelector, v)
	}

	if time.Since(lastActivity.LastActivityTime.Time()) > thresholdDuration {
		service, err := sc.clientset.CoreV1().Services(service.Namespace).Get(context.TODO(), service.Name, metav1.GetOptions{})
		if err != nil {
			if apierrors.IsNotFound(err) {
				return true, nil
			}
			return false, err
		}

		if err := sc.clientset.CoreV1().Services(service.Namespace).Delete(context.TODO(), service.Name, metav1.DeleteOptions{}); err != nil {
			return false, fmt.Errorf("failed to delete service %s/%s, with err %v", service.Namespace, service.Name, err)
		}
		log.Infof("delete service %s/%s successfully", service.Namespace, service.Name)
		return true, nil
	}

	return
}

// ignore sleep-after rules for service, since service do not need support sleep-after rules
// currently, this function do nothing
func (sc *serviceController) syncSleepAfterRules(service *v1.Service, lastActivity activity) error {
	v, ok := service.Labels[sc.SleepAfterSelector]
	if !ok || v == "" {
		// namespace doesn't have sleep-after label, do nothing
		return nil
	}

	log.Infof("ignore sleep-after rules for service %s/%s", service.Namespace, service.Name)

	return nil
}
