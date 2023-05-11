package controller

import (
	"context"
	"fmt"
	"github.com/icinga/icinga-kubernetes/pkg/database"
	schemav1 "github.com/icinga/icinga-kubernetes/pkg/schema/v1"
	"github.com/icinga/icinga-kubernetes/pkg/types"
	"github.com/jmoiron/sqlx"
	"io"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/metrics/pkg/apis/metrics/v1beta1"
	metricsv "k8s.io/metrics/pkg/client/clientset/versioned"
	"log"
)

type PodSync struct {
	clientset        *kubernetes.Clientset
	metricsClientset *metricsv.Clientset
	db               *sqlx.DB
}

func NewPodSync(clientset *kubernetes.Clientset, metricsClientset *metricsv.Clientset, db *sqlx.DB) *PodSync {
	return &PodSync{
		clientset:        clientset,
		metricsClientset: metricsClientset,
		db:               db,
	}
}

func (p *PodSync) GetPodMetrics(pod *corev1.Pod) (float64, float64, float64, float64, error) {
	podMetrics, err := p.metricsClientset.MetricsV1beta1().PodMetricses(pod.Namespace).Get(context.TODO(), pod.Name,
		metav1.GetOptions{})
	if err != nil {
		return 0, 0, 0, 0, err
	}

	cpuUsage := float64(0)
	memoryUsage := float64(0)
	storageUsage := float64(0)
	ephemeralStorageUsage := float64(0)

	for _, container := range podMetrics.Containers {
		cpuUsageValue := container.Usage[corev1.ResourceCPU]
		cpuUsageInt64 := cpuUsageValue.AsDec().UnscaledBig().Int64()
		cpuUsage = float64(cpuUsageInt64) / 1000.0

		memoryUsageValue := container.Usage[corev1.ResourceMemory]
		memoryUsageInt64 := memoryUsageValue.Value()
		memoryUsage = float64(memoryUsageInt64) / (1024 * 1024)

		storageUsageValue := container.Usage[corev1.ResourceStorage]
		storageUsageInt64 := storageUsageValue.Value()
		storageUsage = float64(storageUsageInt64) / (1024 * 1024)

		ephemeralStorageUsageValue := container.Usage[corev1.ResourceEphemeralStorage]
		ephemeralStorageUsageInt64 := ephemeralStorageUsageValue.Value()
		ephemeralStorageUsage = float64(ephemeralStorageUsageInt64) / (1024 * 1024)
	}

	return cpuUsage, memoryUsage, storageUsage, ephemeralStorageUsage, nil
}

func (p *PodSync) Sync(key string, obj interface{}, exists bool) error {
	if !exists {
		fmt.Printf("Pod %s does not exist anymore\n", key)

		namespace, name, err := cache.SplitMetaNamespaceKey(key)
		if err != nil {
			return err
		}
		_, err = p.db.Exec(`DELETE FROM pod WHERE namespace=? AND name=?`, namespace, name)
		if err != nil {
			return err
		}

		_, err = p.db.Exec(`DELETE FROM container_logs WHERE namespace=? AND pod_name=?`, namespace, name)
		if err != nil {
			return err
		}

		_, err = p.db.Exec(`DELETE FROM pod_metrics WHERE namespace=? AND pod_name=?`, namespace, name)
		if err != nil {
			return err
		}
	} else {
		fmt.Printf("Sync/Add/Update for Pod %s\n", obj.(*corev1.Pod).GetName())
		pod, err := schemav1.NewPodFromK8s(obj.(*corev1.Pod))
		if err != nil {
			return err
		}
		stmt := `INSERT INTO pod (name, namespace, uid, phase)
VALUES (:name, :namespace, :uid, :phase)
ON DUPLICATE KEY UPDATE name = VALUES(name), namespace = VALUES(namespace), uid = VALUES(uid), phase = VALUES(phase)`
		fmt.Printf("%+v\n", pod)
		_, err = p.db.NamedExecContext(context.TODO(), stmt, pod)
		if err != nil {
			return err
		}

		k8sPod := obj.(*corev1.Pod)
		// TODO: Loop over pod containers:
		for _, container := range k8sPod.Spec.Containers {
			if err := p.syncContainerLogs(k8sPod, container); err != nil {
				return err
			}
		}

		podMetrics, err := p.metricsClientset.MetricsV1beta1().PodMetricses(pod.Namespace).Get(context.TODO(), pod.Name,
			metav1.GetOptions{})
		for _, container := range podMetrics.Containers {
			if err := p.syncPodMetrics(k8sPod, container); err != nil {
				return err
			}
		}
	}

	return nil
}

func (p *PodSync) syncContainerLogs(pod *corev1.Pod, container corev1.Container) error {
	req := p.clientset.CoreV1().Pods(pod.Namespace).GetLogs(pod.Name, &corev1.PodLogOptions{Container: container.Name})
	body, err := req.Stream(context.TODO())
	if err != nil {
		return err
	}
	defer body.Close()
	logs, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	containerLog := schemav1.ContainerLog{
		ContainerName: container.Name,
		PodName:       pod.Name,
		Namespace:     pod.Namespace,
		Logs:          string(logs),
	}
	// TODO: Update logs in database via INSERT INTO ... ON DUPLICATE KEY. Add table for logs, i.e. container_logs.
	stmt := `INSERT INTO container_logs (namespace, pod_name, container_name, logs)
VALUES (:namespace, :pod_name, :container_name, :logs)
ON DUPLICATE KEY UPDATE namespace = VALUES(namespace), pod_name = VALUES(pod_name), 
                        container_name = VALUES(container_name), logs = VALUES(logs)`
	_, err = p.db.NamedExecContext(context.TODO(), stmt, containerLog)
	if err != nil {
		return err
	}

	return nil
}

func (p *PodSync) syncPodMetrics(pod *corev1.Pod, containerMetrics v1beta1.ContainerMetrics) error {
	metrics, err := p.metricsClientset.MetricsV1beta1().PodMetricses(pod.Namespace).Get(context.TODO(), pod.Name,
		metav1.GetOptions{})
	if err != nil {
		return err
	}
	cpuUsage, memoryUsage, storageUsage, ephemeralStorageUsage, err := p.GetPodMetrics(pod)
	if err != nil {
		return err
	}

	podMetrics := schemav1.PodMetrics{
		Namespace:             pod.Namespace,
		PodName:               pod.Name,
		ContainerName:         containerMetrics.Name,
		Timestamp:             types.UnixMilli(metrics.Timestamp.Time),
		Duration:              metrics.Window.Duration,
		CPUUsage:              cpuUsage,
		MemoryUsage:           memoryUsage,
		StorageUsage:          storageUsage,
		EphemeralStorageUsage: ephemeralStorageUsage,
	}

	stmt := database.BuildUpsertStmt(podMetrics)
	_, err = p.db.NamedExecContext(context.TODO(), stmt, podMetrics)
	if err != nil {
		return err
	}

	return nil
}

func (p *PodSync) WarmUp(indexer cache.Indexer) {
	stmt, err := p.db.Queryx(`SELECT namespace, name from pod`)
	if err != nil {
		klog.Fatal(err)
	}
	defer stmt.Close()

	for stmt.Next() {
		var pod corev1.Pod
		err := stmt.StructScan(&pod)
		if err != nil {
			log.Fatal(err)
		}
		indexer.Add(metav1.ObjectMeta{
			Name:      pod.Name,
			Namespace: pod.Namespace,
		})
	}
}
