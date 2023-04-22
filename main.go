package main

import (
	"context"
	"flag"
	"fmt"
	"strings"
	"sync"
	"time"

	appsv1alpha1 "github.com/openkruise/kruise/apis/apps/v1alpha1"
	"github.com/openkruise/kruise/pkg/client"
	daemonruntime "github.com/openkruise/kruise/pkg/daemon/criruntime"
	runtimeimage "github.com/openkruise/kruise/pkg/daemon/criruntime/imageruntime"
	daemonutil "github.com/openkruise/kruise/pkg/daemon/util"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
)

const defaultImagePullingTimeout = 10 * time.Minute
const defaultImagePullingProgressLogInterval = 5 * time.Second

var images = flag.String("images", "", "images to pull (comma separated)")
var runtimePath = flag.String("runtime", "/var/run", "mount path for container runtime")

func main() {
	klog.InitFlags(nil)
	flag.Parse()
	if images == nil || *images == "" {
		klog.Error("No images specified")
		return
	}
	klog.Info("runtime path: ", *runtimePath)

	f, err := newDaemon()
	if err != nil {
		klog.Error(err)
		return
	}

	imageList := strings.Split(strings.Trim(*images, "\""), ",")
	for _, image := range imageList {
		if err := Pull(image, f); err != nil {
			klog.Error(err)
		}
	}
}

// NewDaemon create a daemon
func newDaemon() (daemonruntime.Factory, error) {
	cfg := config.GetConfigOrDie()
	cfg.UserAgent = "image-prepuller"
	if err := client.NewRegistry(cfg); err != nil {
		klog.Error("Failed to init clientset registry: %v", err)
		return nil, err
	}

	genericClient := client.GetGenericClient()
	if genericClient == nil || genericClient.KubeClient == nil || genericClient.KruiseClient == nil {
		return nil, fmt.Errorf("generic client can not be nil")
	}

	accountManager := daemonutil.NewImagePullAccountManager(genericClient.KubeClient)
	runtimeFactory, err := daemonruntime.NewFactory(*runtimePath, accountManager)
	if err != nil {
		return nil, fmt.Errorf("failed to new runtime factory: %v", err)
	}

	/*
		s, err := runtimeFactory.GetImageService().PullImage(context.TODO(), "docker.io/library/redis", "alpine", nil, nil)
		if err != nil {
			return fmt.Errorf("failed to pull image: %v", err)
		}
		klog.Infof("pull image status: %v", s.C())
	*/
	return runtimeFactory, nil
}

// return image name and tag
func parseImageName(imgName string) (string, string, error) {
	// Keep this in sync with TransportFromImageName!
	name, tag, valid := strings.Cut(imgName, ":")
	if !valid {
		return imgName, "latest", nil
	}
	if name == "" {
		return "", "", fmt.Errorf(`Invalid image name "%s", empty name`, imgName)
	}
	if tag == "" {
		tag = "latest"
	}
	return name, tag, nil
}

func Pull(image string, f daemonruntime.Factory) error {
	name, tag, err := parseImageName(image)
	if err != nil {
		return err
	}
	pullContext, cancel := context.WithTimeout(context.Background(), defaultImagePullingTimeout)
	defer cancel()
	startTime := metav1.Now()
	newStatus := &appsv1alpha1.ImageTagStatus{
		Tag:       tag,
		Phase:     appsv1alpha1.ImagePhasePulling,
		StartTime: &startTime,
	}
	w := &pullWorker{
		name: name,
		tagSpec: appsv1alpha1.ImageTagSpec{
			Tag: tag,
		},
		secrets: nil,
		runtime: f.GetImageService(),
		active:  true,
		stopCh:  make(chan struct{}),
	}
	defer func() {
		cost := time.Since(startTime.Time)
		if newStatus.Phase == appsv1alpha1.ImagePhaseFailed {
			klog.Warningf("Worker failed to pull image %s:%s, cost %v, err: %v", w.name, tag, cost, newStatus.Message)
		} else {
			klog.Infof("Successfully pull image %s:%s, cost %vs", w.name, tag, cost)
		}
	}()
	backoffLimit := 3
	var lastError error
	for i := 0; i <= backoffLimit; i++ {
		lastError = w.doPullImage(pullContext, newStatus)
		if lastError != nil {
			cancel()
			klog.Warningf("Pulling image %s:%s backoff %d, error %v", w.name, w.tagSpec.Tag, 1, lastError)
			continue
		}
		w.finishPulling(newStatus, appsv1alpha1.ImagePhaseSucceeded, "")
		cancel()
		break
	}

	return lastError
}

type pullWorker struct {
	sync.Mutex

	name    string
	tagSpec appsv1alpha1.ImageTagSpec
	secrets []v1.Secret
	runtime runtimeimage.ImageService

	active bool
	stopCh chan struct{}
}

func (w *pullWorker) doPullImage(ctx context.Context, newStatus *appsv1alpha1.ImageTagStatus) (err error) {
	tag := w.tagSpec.Tag
	startTime := metav1.Now()

	klog.Infof("Worker is starting to pull image %s:%s", w.name, tag)

	if _, e := w.getImageInfo(ctx); e == nil {
		klog.Infof("Image %s:%s is already exists", w.name, tag)
		newStatus.Progress = 100
		return nil
	}

	// make it asynchronous for CRI runtime will block in pulling image
	var statusReader runtimeimage.ImagePullStatusReader
	pullChan := make(chan struct{})
	go func() {
		statusReader, err = w.runtime.PullImage(ctx, w.name, w.tagSpec.Tag, w.secrets, nil)
		close(pullChan)
	}()

	closeStatusReader := func() {
		select {
		case <-pullChan:
		}
		if statusReader != nil {
			statusReader.Close()
		}
	}

	select {
	case <-w.stopCh:
		go closeStatusReader()
		klog.Infof("Pulling image %v:%v is stopped.", w.name, tag)
		return fmt.Errorf("pulling image %s:%s is stopped", w.name, tag)
	case <-ctx.Done():
		go closeStatusReader()
		klog.Infof("Pulling image %s:%s is canceled", w.name, tag)
		return fmt.Errorf("pulling image %s:%s is canceled", w.name, tag)
	case <-pullChan:
		if err != nil {
			return err
		}
	}
	defer statusReader.Close()

	progress := 0
	var progressInfo string
	logTicker := time.NewTicker(defaultImagePullingProgressLogInterval)
	defer logTicker.Stop()

	for {
		select {
		case <-w.stopCh:
			klog.Infof("Pulling image %v:%v is stopped.", w.name, tag)
			return fmt.Errorf("pulling image %s:%s is stopped", w.name, tag)
		case <-ctx.Done():
			klog.Infof("Pulling image %s:%s is canceled", w.name, tag)
			return fmt.Errorf("pulling image %s:%s is canceled", w.name, tag)
		case <-logTicker.C:
			klog.Infof("Pulling image %s:%s, cost: %v, progress: %v%%, detail: %v", w.name, tag, time.Since(startTime.Time), progress, progressInfo)
		case progressStatus, ok := <-statusReader.C():
			if !ok {
				return fmt.Errorf("pulling image %s:%s internal error", w.name, tag)
			}
			progress = progressStatus.Process
			progressInfo = progressStatus.DetailInfo
			newStatus.Progress = int32(progressStatus.Process)
			klog.V(1).Infof("Pulling image %s:%s, cost: %v, progress: %v%%, detail: %v", w.name, tag, time.Since(startTime.Time), progress, progressInfo)
			if progressStatus.Finish {
				if progressStatus.Err == nil {
					return nil
				}
				return fmt.Errorf("pulling image %s:%s error %v", w.name, tag, progressStatus.Err)
			}
		}
	}
}

func (w *pullWorker) getImageInfo(ctx context.Context) (*runtimeimage.ImageInfo, error) {
	imageInfos, err := w.runtime.ListImages(ctx)
	if err != nil {
		klog.Infof("List images failed, err %v", err)
		return nil, err
	}
	for _, info := range imageInfos {
		klog.V(2).Info(info)
		if info.ContainsImage(w.name, w.tagSpec.Tag) {
			return &info, nil
		}
	}
	return nil, fmt.Errorf("image %v:%v not found", w.name, w.tagSpec.Tag)
}

func (w *pullWorker) finishPulling(newStatus *appsv1alpha1.ImageTagStatus, phase appsv1alpha1.ImagePullPhase, message string) {
	newStatus.Phase = phase
	now := metav1.Now()
	newStatus.CompletionTime = &now
	newStatus.Message = message
	//klog.V(5).Infof("pulling image %v finished, status=%#v", w.ImageRef(), newStatus)
	//w.statusUpdater.UpdateStatus(newStatus)
}
