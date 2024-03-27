package controller

import (
	"github.com/sirupsen/logrus"
	v1 "k8s.io/api/core/v1"
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

	logrus.Debugf("pick service %s, last activity %v", service.Name, activity)

	// TODO(CarlJi): implement the checkService method

	return nil
}
