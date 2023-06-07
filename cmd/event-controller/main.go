package main

import (
	"context"
	"flag"
	"fmt"
	"github.com/kubefree/pkg/event"
	"github.com/kubefree/pkg/logrusutil"
	"github.com/sirupsen/logrus"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	logrusutil.ComponentInit("kubefree-event")

	var logLevel string
	flag.StringVar(&logLevel, "log-level", logrus.InfoLevel.String(), fmt.Sprintf("Logging level, one of %v", logrus.AllLevels))
	var kubeconfig string
	flag.StringVar(&kubeconfig, "kubeconfig", "", "absolute path to the kubeconfig file")
	var dryRun bool
	flag.BoolVar(&dryRun, "dryRun", false, "If set, kubefree controller will not delete or sleep namespace, but still annotate it")

	flag.Parse()

	level, err := logrus.ParseLevel(logLevel)
	if err != nil {
		logrus.WithError(err).Fatal("Error logrus.ParseLevel")
	}
	logrus.SetLevel(level)

	var config *rest.Config
	if len(kubeconfig) > 0 {
		config, err = clientcmd.BuildConfigFromFlags("", kubeconfig)
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
	// Create a context with cancellation support
	ctx, cancel := context.WithCancel(context.Background())
	// Handle signals to gracefully shutdown the controller
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		sig := <-sigCh
		logrus.Infof("Received signal %s, shutting down...", sig)
		cancel()
	}()

	done := make(chan struct{})

	// 启动新的 goroutine 执行事件处理逻辑
	go func() {
		event.HandleEvents(ctx, clientset)
		// 发送信号到通道，通知主线程退出
		done <- struct{}{}
	}()

	// 等待取消信号或退出信号
	select {
	case <-ctx.Done():
		// 等待一段时间，允许优雅的关闭
		time.Sleep(5 * time.Second)
		logrus.Info("Controller shutdown successfully.")
		cancel()
		os.Exit(1)
	case <-done:
		logrus.Info("Controller shutdown successfully.")
		cancel()
		os.Exit(1)
	}
}
