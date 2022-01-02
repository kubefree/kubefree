package main

import (
	"flag"
	"fmt"
	"path/filepath"

	"github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/homedir"

	ctl "github.com/kubefree/pkg/controller"
	"github.com/kubefree/pkg/logrusutil"
)

func main() {
	logrusutil.ComponentInit("kubefree-controller")

	var logLevel string
	flag.StringVar(&logLevel, "log-level", logrus.InfoLevel.String(), fmt.Sprintf("Logging level, one of %v", logrus.AllLevels))

	var kubeconfig *string
	if home := homedir.HomeDir(); home != "" {
		kubeconfig = flag.String("kubeconfig", filepath.Join(home, ".kube", "config"), "(optional) absolute path to the kubeconfig file")
	} else {
		kubeconfig = flag.String("kubeconfig", "", "absolute path to the kubeconfig file")
	}
	flag.Parse()

	level, err := logrus.ParseLevel(logLevel)
	if err != nil {
		logrus.WithError(err).Fatal("Error logrus.ParseLevel")
	}
	logrus.SetLevel(level)

	var config *rest.Config
	if kubeconfig != nil {
		config, err = clientcmd.BuildConfigFromFlags("", *kubeconfig)
		if err != nil {
			logrus.WithError(err).Fatal("Error clientcmd.BuildConfigFromFlags")
		}
	} else {
		config, err = rest.InClusterConfig()
		if err != nil {
			logrus.WithError(err).Fatal("Error logrus.ParseLevel")
		}
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		logrus.WithError(err).Fatal("Error kubernetes.NewForConfig")
	}

	ctl, err := ctl.NewController(clientset)
	if err != nil {
		logrus.WithError(err).Fatal("Error plank.NewController")
	}

	stop := make(chan struct{})
	defer close(stop)

	go ctl.Run(2, stop)

	// wait forever
	select {}
}
