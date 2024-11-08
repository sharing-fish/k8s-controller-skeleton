package main

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/joho/godotenv"
	"github.com/slack-go/slack"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	v1 "k8s.io/client-go/listers/core/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"
	metricsclientset "k8s.io/metrics/pkg/client/clientset/versioned"
)

type Controller struct {
	kubeclientset    kubernetes.Interface
	metricsclientset metricsclientset.Interface
	podsLister       v1.PodLister
	podsInformer     cache.SharedIndexInformer
	workqueue        workqueue.RateLimitingInterface
}

func NewController(
	kubeclientset kubernetes.Interface,
	metricsclientset metricsclientset.Interface,
	podsInformer cache.SharedIndexInformer) *Controller {

	workqueue := workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), "Pods")

	controller := &Controller{
		kubeclientset:    kubeclientset,
		metricsclientset: metricsclientset,
		podsLister:       v1.NewPodLister(podsInformer.GetIndexer()),
		podsInformer:     podsInformer,
		workqueue:        workqueue,
	}

	podsInformer.AddEventHandler(cache.ResourceEventHandlerFuncs{
		AddFunc: controller.enqueuePod,
		UpdateFunc: func(old, new interface{}) {
			controller.enqueuePod(new)
		},
		DeleteFunc: controller.enqueuePod,
	})

	return controller
}

func (c *Controller) enqueuePod(obj interface{}) {
	key, err := cache.MetaNamespaceKeyFunc(obj)
	if err != nil {
		runtime.HandleError(err)
		return
	}
	c.workqueue.Add(key)
}

func (c *Controller) runWorker() {
	for c.processNextWorkItem() {
	}
}

func (c *Controller) processNextWorkItem() bool {
	obj, shutdown := c.workqueue.Get()

	if shutdown {
		return false
	}

	err := func(obj interface{}) error {
		defer c.workqueue.Done(obj)
		var key string
		var ok bool
		if key, ok = obj.(string); !ok {
			c.workqueue.Forget(obj)
			klog.Errorf("expected string in workqueue but got %#v", obj)
			return nil
		}
		if err := c.syncHandler(key); err != nil {
			c.workqueue.AddRateLimited(key)
			return fmt.Errorf("error syncing '%s': %s, requeuing", key, err.Error())
		}
		c.workqueue.Forget(key)
		klog.Infof("Successfully synced '%s'", key)
		return nil
	}(obj)

	if err != nil {
		klog.Errorf("error processing item: %v", err)
		return true
	}

	return true
}

func (c *Controller) syncHandler(key string) error {
	namespace, name, err := cache.SplitMetaNamespaceKey(key)
	if err != nil {
		klog.Errorf("invalid resource key: %s", key)
		return nil
	}

	pod, err := c.podsLister.Pods(namespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			klog.Infof("Pod has been deleted: %s", key)
			return nil
		}
		return err
	}

	// Query the metrics server for the Pod's CPU usage
	metrics, err := c.metricsclientset.MetricsV1beta1().PodMetricses(namespace).Get(context.TODO(), name, metav1.GetOptions{})
	if err != nil {
		return fmt.Errorf("failed to get metrics for pod %s/%s: %v", namespace, name, err)
	}

	// Check if the CPU usage exceeds the threshold
	cpuThreshold := int64(100) // Example threshold in millicores
	for _, container := range metrics.Containers {
		cpuUsage := container.Usage.Cpu().MilliValue()
		if cpuUsage > cpuThreshold {
			api := slack.New(os.Getenv("SLACK_API_TOKEN"))
			channelID := os.Getenv("SLACK_CHANNEL_ID")
			message := fmt.Sprintf("Pod %s/%s is using %dm CPU, which exceeds the threshold of %dm", pod.Namespace, pod.Name, cpuUsage, cpuThreshold)
			_, _, err := api.PostMessage(channelID, slack.MsgOptionText(message, false))
			if err != nil {
				return fmt.Errorf("failed to send Slack message: %v", err)
			}
			klog.Infof("Sent Slack message for pod %s/%s", pod.Namespace, pod.Name)
		}
	}

	return nil
}

func (c *Controller) Run(threadiness int, stopCh <-chan struct{}) error {
	defer runtime.HandleCrash()
	defer c.workqueue.ShutDown()

	klog.Info("Starting Pod controller")

	// Ensure the informer is only started once
	go func() {
		c.podsInformer.Run(stopCh)
	}()

	if !cache.WaitForCacheSync(stopCh, c.podsInformer.HasSynced) {
		return fmt.Errorf("failed to wait for caches to sync")
	}

	for i := 0; i < threadiness; i++ {
		go wait.Until(c.runWorker, time.Second, stopCh)
	}

	<-stopCh
	klog.Info("Shutting down workers")

	return nil
}

func main() {
	err := godotenv.Load()
	if err != nil {
		klog.Fatalf("Error loading .env file")
	}

	kubeconfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(
		clientcmd.NewDefaultClientConfigLoadingRules(),
		&clientcmd.ConfigOverrides{},
	)
	config, err := kubeconfig.ClientConfig()
	if err != nil {
		klog.Fatalf("Error building kubeconfig: %s", err.Error())
	}

	kubeclientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Error building kubernetes clientset: %s", err.Error())
	}

	metricsclientset, err := metricsclientset.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Error building metrics clientset: %s", err.Error())
	}

	informerFactory := informers.NewSharedInformerFactoryWithOptions(kubeclientset, time.Second*30, informers.WithNamespace("slackspace"))
	podsInformer := informerFactory.Core().V1().Pods().Informer()

	controller := NewController(kubeclientset, metricsclientset, podsInformer)

	stopCh := make(chan struct{})
	defer close(stopCh)

	go informerFactory.Start(stopCh)

	if err = controller.Run(2, stopCh); err != nil {
		klog.Fatalf("Error running controller: %s", err.Error())
	}
}
