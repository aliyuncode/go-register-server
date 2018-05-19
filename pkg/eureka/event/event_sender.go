package event

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"

	"github.com/Shopify/sarama"
	"github.com/golang/glog"
	"k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	v1core "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/client-go/tools/record"

	"github.com/choerodon/go-register-server/pkg/eureka/apps"
)

const (
	DefaultNamespace = "io-choerodon"
	DefaultName      = "register-server"
	topic            = "register-server"
)

func NewEventSender(client *kubernetes.Clientset, instance <-chan apps.Instance, stopCh <-chan struct{}) error {
	namespace := os.Getenv("REGISTER_SERVER_NAMESPACE")
	if namespace == "" {
		glog.Info("use default namespace")
		namespace = DefaultNamespace
	}
	config := sarama.NewConfig()
	config.Producer.RequiredAcks = sarama.WaitForAll
	config.Producer.Partitioner = sarama.NewRandomPartitioner
	config.Producer.Return.Successes = true
	config.Producer.Timeout = 5 * time.Second
	kafkaAddresses := os.Getenv("KAFKA_ADDRESSES")
	if len(kafkaAddresses) == 0 {
		return errors.New("no kafka address in env")
	}
	p, err := sarama.NewSyncProducer(strings.Split(kafkaAddresses, ","), config)
	if err != nil {
		glog.Errorln(err)
		return err
	}

	id, err := os.Hostname()
	if err != nil {
		return err
	}
	recorder := createRecorder(client)
	rl, err := resourcelock.New(resourcelock.EndpointsResourceLock,
		namespace,
		DefaultName,
		client.CoreV1(),
		resourcelock.ResourceLockConfig{
			Identity:      id,
			EventRecorder: recorder,
		})
	if err != nil {
		glog.Fatalf("error creating lock: %v", err)
	}

	leaderelection.RunOrDie(leaderelection.LeaderElectionConfig{
		Lock:          rl,
		LeaseDuration: 15 * time.Second,
		RenewDeadline: 10 * time.Second,
		RetryPeriod:   2 * time.Second,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(stop <-chan struct{}) {
				for {
					var msg apps.Instance
					msg = <-instance
					event := &Event{
						Id:      msg.InstanceId,
						AppName: msg.App,
						Status:  string(msg.Status),
						Version: msg.Metadata["VERSION"],
					}
					sendMsg(p, event)
				}
			},
			OnStoppedLeading: func() {
				glog.Fatalf("leader election lost")
			},
		},
	})

	return nil
}

func createRecorder(kubeClient *kubernetes.Clientset) record.EventRecorder {
	eventBroadcaster := record.NewBroadcaster()
	eventBroadcaster.StartLogging(glog.Infof)
	eventBroadcaster.StartRecordingToSink(&v1core.EventSinkImpl{Interface: v1core.New(kubeClient.CoreV1().RESTClient()).Events("")})
	return eventBroadcaster.NewRecorder(runtime.NewScheme(), v1.EventSource{Component: "register-server"})
}

func sendMsg(producer sarama.SyncProducer, toSend *Event) {
	v, _ := json.Marshal(toSend)
	msg := &sarama.ProducerMessage{
		Topic:     topic,
		Value:     sarama.ByteEncoder(v),
		Timestamp: time.Now(),
	}
	if partion, offset, err := producer.SendMessage(msg); err != nil {
		glog.Errorln(err)
		return
	} else {
		glog.Infof("event sender send instance event for %s ,partion:%d, offset:%d", toSend.AppName, partion, offset)
	}
}