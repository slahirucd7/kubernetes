/*
Copyright 2015 The Kubernetes Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package images

import (
	"context"
	goerrors "errors"
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"go.opentelemetry.io/otel/trace"
	v1 "k8s.io/api/core/v1"
	"k8s.io/klog/v2"

	"k8s.io/apimachinery/pkg/util/errors"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/tools/record"
	statsapi "k8s.io/kubelet/pkg/apis/stats/v1alpha1"
	"k8s.io/kubernetes/pkg/kubelet/container"
	"k8s.io/kubernetes/pkg/kubelet/events"
	"k8s.io/kubernetes/pkg/kubelet/metrics"
	"k8s.io/kubernetes/pkg/kubelet/util/sliceutils"
)

// instrumentationScope is OpenTelemetry instrumentation scope name
const instrumentationScope = "k8s.io/kubernetes/pkg/kubelet/images"

// StatsProvider is an interface for fetching stats used during image garbage
// collection.
type StatsProvider interface {
	// ImageFsStats returns the stats of the image filesystem.
	ImageFsStats(ctx context.Context) (*statsapi.FsStats, error)
}

// ImageGCManager is an interface for managing lifecycle of all images.
// Implementation is thread-safe.
type ImageGCManager interface {
	// Applies the garbage collection policy. Errors include being unable to free
	// enough space as per the garbage collection policy.
	GarbageCollect(ctx context.Context) error

	// Start async garbage collection of images.
	Start()

	GetImageList() ([]container.Image, error)

	// Delete all unused images.
	DeleteUnusedImages(ctx context.Context) error
}

// ImageGCPolicy is a policy for garbage collecting images. Policy defines an allowed band in
// which garbage collection will be run.
type ImageGCPolicy struct {
	// Any usage above this threshold will always trigger garbage collection.
	// This is the highest usage we will allow.
	HighThresholdPercent int

	// Any usage below this threshold will never trigger garbage collection.
	// This is the lowest threshold we will try to garbage collect to.
	LowThresholdPercent int

	// Minimum age at which an image can be garbage collected.
	MinAge time.Duration

	// Maximum age after which an image can be garbage collected, regardless of disk usage.
	// Currently gated by MaximumImageGCAge feature gate and Kubelet configuration.
	// If 0, the feature is disabled.
	MaxAge time.Duration
}

type realImageGCManager struct {
	// Container runtime
	runtime container.Runtime

	// Records of images and their use.
	imageRecords     map[string]*imageRecord
	imageRecordsLock sync.Mutex

	// The image garbage collection policy in use.
	policy ImageGCPolicy

	// statsProvider provides stats used during image garbage collection.
	statsProvider StatsProvider

	// Recorder for Kubernetes events.
	recorder record.EventRecorder

	// Reference to this node.
	nodeRef *v1.ObjectReference

	// Track initialization
	initialized bool

	// imageCache is the cache of latest image list.
	imageCache imageCache

	// tracer for recording spans
	tracer trace.Tracer
}

// imageCache caches latest result of ListImages.
type imageCache struct {
	// sync.Mutex is the mutex protects the image cache.
	sync.Mutex
	// images is the image cache.
	images []container.Image
}

// set sorts the input list and updates image cache.
// 'i' takes ownership of the list, you should not reference the list again
// after calling this function.
func (i *imageCache) set(images []container.Image) {
	i.Lock()
	defer i.Unlock()
	// The image list needs to be sorted when it gets read and used in
	// setNodeStatusImages. We sort the list on write instead of on read,
	// because the image cache is more often read than written
	sort.Sort(sliceutils.ByImageSize(images))
	i.images = images
}

// get gets image list from image cache.
// NOTE: The caller of get() should not do mutating operations on the
// returned list that could cause data race against other readers (e.g.
// in-place sorting the returned list)
func (i *imageCache) get() []container.Image {
	i.Lock()
	defer i.Unlock()
	return i.images
}

// Information about the images we track.
type imageRecord struct {
	// Time when this image was first detected.
	firstDetected time.Time

	// Time when we last saw this image being used.
	lastUsed time.Time

	// Size of the image in bytes.
	size int64

	// Pinned status of the image
	pinned bool
}

// NewImageGCManager instantiates a new ImageGCManager object.
func NewImageGCManager(runtime container.Runtime, statsProvider StatsProvider, recorder record.EventRecorder, nodeRef *v1.ObjectReference, policy ImageGCPolicy, tracerProvider trace.TracerProvider) (ImageGCManager, error) {
	// Validate policy.
	if policy.HighThresholdPercent < 0 || policy.HighThresholdPercent > 100 {
		return nil, fmt.Errorf("invalid HighThresholdPercent %d, must be in range [0-100]", policy.HighThresholdPercent)
	}
	if policy.LowThresholdPercent < 0 || policy.LowThresholdPercent > 100 {
		return nil, fmt.Errorf("invalid LowThresholdPercent %d, must be in range [0-100]", policy.LowThresholdPercent)
	}
	if policy.LowThresholdPercent > policy.HighThresholdPercent {
		return nil, fmt.Errorf("LowThresholdPercent %d can not be higher than HighThresholdPercent %d", policy.LowThresholdPercent, policy.HighThresholdPercent)
	}
	tracer := tracerProvider.Tracer(instrumentationScope)
	im := &realImageGCManager{
		runtime:       runtime,
		policy:        policy,
		imageRecords:  make(map[string]*imageRecord),
		statsProvider: statsProvider,
		recorder:      recorder,
		nodeRef:       nodeRef,
		initialized:   false,
		tracer:        tracer,
	}

	return im, nil
}

func (im *realImageGCManager) Start() {
	ctx := context.Background()
	go wait.Until(func() {
		// Initial detection make detected time "unknown" in the past.
		var ts time.Time
		if im.initialized {
			ts = time.Now()
		}
		_, err := im.detectImages(ctx, ts)
		if err != nil {
			klog.InfoS("Failed to monitor images", "err", err)
		} else {
			im.initialized = true
		}
	}, 5*time.Minute, wait.NeverStop)

	// Start a goroutine periodically updates image cache.
	go wait.Until(func() {
		images, err := im.runtime.ListImages(ctx)
		if err != nil {
			klog.InfoS("Failed to update image list", "err", err)
		} else {
			im.imageCache.set(images)
		}
	}, 30*time.Second, wait.NeverStop)

}

// Get a list of images on this node
func (im *realImageGCManager) GetImageList() ([]container.Image, error) {
	return im.imageCache.get(), nil
}

func (im *realImageGCManager) detectImages(ctx context.Context, detectTime time.Time) (sets.String, error) {
	imagesInUse := sets.NewString()

	images, err := im.runtime.ListImages(ctx)
	if err != nil {
		return imagesInUse, err
	}
	pods, err := im.runtime.GetPods(ctx, true)
	if err != nil {
		return imagesInUse, err
	}

	// Make a set of images in use by containers.
	for _, pod := range pods {
		for _, container := range pod.Containers {
			klog.V(5).InfoS("Container uses image", "pod", klog.KRef(pod.Namespace, pod.Name), "containerName", container.Name, "containerImage", container.Image, "imageID", container.ImageID)
			imagesInUse.Insert(container.ImageID)
		}
	}

	// Add new images and record those being used.
	now := time.Now()
	currentImages := sets.NewString()
	im.imageRecordsLock.Lock()
	defer im.imageRecordsLock.Unlock()
	for _, image := range images {
		klog.V(5).InfoS("Adding image ID to currentImages", "imageID", image.ID)
		currentImages.Insert(image.ID)

		// New image, set it as detected now.
		if _, ok := im.imageRecords[image.ID]; !ok {
			klog.V(5).InfoS("Image ID is new", "imageID", image.ID)
			im.imageRecords[image.ID] = &imageRecord{
				firstDetected: detectTime,
			}
		}

		// Set last used time to now if the image is being used.
		if isImageUsed(image.ID, imagesInUse) {
			klog.V(5).InfoS("Setting Image ID lastUsed", "imageID", image.ID, "lastUsed", now)
			im.imageRecords[image.ID].lastUsed = now
		}

		klog.V(5).InfoS("Image ID has size", "imageID", image.ID, "size", image.Size)
		im.imageRecords[image.ID].size = image.Size

		klog.V(5).InfoS("Image ID is pinned", "imageID", image.ID, "pinned", image.Pinned)
		im.imageRecords[image.ID].pinned = image.Pinned
	}

	// Remove old images from our records.
	for image := range im.imageRecords {
		if !currentImages.Has(image) {
			klog.V(5).InfoS("Image ID is no longer present; removing from imageRecords", "imageID", image)
			delete(im.imageRecords, image)
		}
	}

	return imagesInUse, nil
}

func (im *realImageGCManager) GarbageCollect(ctx context.Context) error {
	ctx, otelSpan := im.tracer.Start(ctx, "Images/GarbageCollect")
	defer otelSpan.End()

	freeTime := time.Now()
	images, err := im.imagesInEvictionOrder(ctx, freeTime)
	if err != nil {
		return err
	}

	images, err = im.freeOldImages(ctx, images, freeTime)
	if err != nil {
		return err
	}

	// Get disk usage on disk holding images.
	fsStats, err := im.statsProvider.ImageFsStats(ctx)
	if err != nil {
		return err
	}

	var capacity, available int64
	if fsStats.CapacityBytes != nil {
		capacity = int64(*fsStats.CapacityBytes)
	}
	if fsStats.AvailableBytes != nil {
		available = int64(*fsStats.AvailableBytes)
	}

	if available > capacity {
		klog.InfoS("Availability is larger than capacity", "available", available, "capacity", capacity)
		available = capacity
	}

	// Check valid capacity.
	if capacity == 0 {
		err := goerrors.New("invalid capacity 0 on image filesystem")
		im.recorder.Eventf(im.nodeRef, v1.EventTypeWarning, events.InvalidDiskCapacity, err.Error())
		return err
	}

	// If over the max threshold, free enough to place us at the lower threshold.
	usagePercent := 100 - int(available*100/capacity)
	if usagePercent >= im.policy.HighThresholdPercent {
		amountToFree := capacity*int64(100-im.policy.LowThresholdPercent)/100 - available
		klog.InfoS("Disk usage on image filesystem is over the high threshold, trying to free bytes down to the low threshold", "usage", usagePercent, "highThreshold", im.policy.HighThresholdPercent, "amountToFree", amountToFree, "lowThreshold", im.policy.LowThresholdPercent)
		freed, err := im.freeSpace(ctx, amountToFree, freeTime, images)
		if err != nil {
			return err
		}

		if freed < amountToFree {
			err := fmt.Errorf("Failed to garbage collect required amount of images. Attempted to free %d bytes, but only found %d bytes eligible to free.", amountToFree, freed)
			im.recorder.Eventf(im.nodeRef, v1.EventTypeWarning, events.FreeDiskSpaceFailed, err.Error())
			return err
		}
	}

	return nil
}

func (im *realImageGCManager) freeOldImages(ctx context.Context, images []evictionInfo, freeTime time.Time) ([]evictionInfo, error) {
	if im.policy.MaxAge == 0 {
		return images, nil
	}
	var deletionErrors []error
	remainingImages := make([]evictionInfo, 0)
	for _, image := range images {
		klog.V(5).InfoS("Evaluating image ID for possible garbage collection based on image age", "imageID", image.id)
		// Evaluate whether image is older than MaxAge.
		if freeTime.Sub(image.lastUsed) > im.policy.MaxAge {
			if err := im.freeImage(ctx, image); err != nil {
				deletionErrors = append(deletionErrors, err)
				remainingImages = append(remainingImages, image)
				continue
			}
			continue
		}
		remainingImages = append(remainingImages, image)
	}
	if len(deletionErrors) > 0 {
		return remainingImages, fmt.Errorf("wanted to free images older than %v, encountered errors in image deletion: %v", im.policy.MaxAge, errors.NewAggregate(deletionErrors))
	}
	return remainingImages, nil
}

func (im *realImageGCManager) DeleteUnusedImages(ctx context.Context) error {
	klog.InfoS("Attempting to delete unused images")
	freeTime := time.Now()
	images, err := im.imagesInEvictionOrder(ctx, freeTime)
	if err != nil {
		return err
	}
	_, err = im.freeSpace(ctx, math.MaxInt64, freeTime, images)
	return err
}

// Tries to free bytesToFree worth of images on the disk.
//
// Returns the number of bytes free and an error if any occurred. The number of
// bytes freed is always returned.
// Note that error may be nil and the number of bytes free may be less
// than bytesToFree.
func (im *realImageGCManager) freeSpace(ctx context.Context, bytesToFree int64, freeTime time.Time, images []evictionInfo) (int64, error) {
	// Delete unused images until we've freed up enough space.
	var deletionErrors []error
	spaceFreed := int64(0)
	for _, image := range images {
		klog.V(5).InfoS("Evaluating image ID for possible garbage collection based on disk usage", "imageID", image.id)
		// Images that are currently in used were given a newer lastUsed.
		if image.lastUsed.Equal(freeTime) || image.lastUsed.After(freeTime) {
			klog.V(5).InfoS("Image ID was used too recently, not eligible for garbage collection", "imageID", image.id, "lastUsed", image.lastUsed, "freeTime", freeTime)
			continue
		}

		// Avoid garbage collect the image if the image is not old enough.
		// In such a case, the image may have just been pulled down, and will be used by a container right away.
		if freeTime.Sub(image.firstDetected) < im.policy.MinAge {
			klog.V(5).InfoS("Image ID's age is less than the policy's minAge, not eligible for garbage collection", "imageID", image.id, "age", freeTime.Sub(image.firstDetected), "minAge", im.policy.MinAge)
			continue
		}

		if err := im.freeImage(ctx, image); err != nil {
			deletionErrors = append(deletionErrors, err)
			continue
		}
		spaceFreed += image.size

		if spaceFreed >= bytesToFree {
			break
		}
	}

	if len(deletionErrors) > 0 {
		return spaceFreed, fmt.Errorf("wanted to free %d bytes, but freed %d bytes space with errors in image deletion: %v", bytesToFree, spaceFreed, errors.NewAggregate(deletionErrors))
	}
	return spaceFreed, nil
}

func (im *realImageGCManager) freeImage(ctx context.Context, image evictionInfo) error {
	// Remove image. Continue despite errors.
	klog.InfoS("Removing image to free bytes", "imageID", image.id, "size", image.size)
	err := im.runtime.RemoveImage(ctx, container.ImageSpec{Image: image.id})
	if err != nil {
		return err
	}
	delete(im.imageRecords, image.id)
	metrics.ImageGarbageCollectedTotal.Inc()
	return err
}

// Queries all of the image records and arranges them in a slice of evictionInfo, sorted based on last time used, ignoring images pinned by the runtime.
func (im *realImageGCManager) imagesInEvictionOrder(ctx context.Context, freeTime time.Time) ([]evictionInfo, error) {
	imagesInUse, err := im.detectImages(ctx, freeTime)
	if err != nil {
		return nil, err
	}

	im.imageRecordsLock.Lock()
	defer im.imageRecordsLock.Unlock()

	// Get all images in eviction order.
	images := make([]evictionInfo, 0, len(im.imageRecords))
	for image, record := range im.imageRecords {
		if isImageUsed(image, imagesInUse) {
			klog.V(5).InfoS("Image ID is being used", "imageID", image)
			continue
		}
		// Check if image is pinned, prevent garbage collection
		if record.pinned {
			klog.V(5).InfoS("Image is pinned, skipping garbage collection", "imageID", image)
			continue

		}
		images = append(images, evictionInfo{
			id:          image,
			imageRecord: *record,
		})
	}
	sort.Sort(byLastUsedAndDetected(images))
	return images, nil
}

type evictionInfo struct {
	id string
	imageRecord
}

type byLastUsedAndDetected []evictionInfo

func (ev byLastUsedAndDetected) Len() int      { return len(ev) }
func (ev byLastUsedAndDetected) Swap(i, j int) { ev[i], ev[j] = ev[j], ev[i] }
func (ev byLastUsedAndDetected) Less(i, j int) bool {
	// Sort by last used, break ties by detected.
	if ev[i].lastUsed.Equal(ev[j].lastUsed) {
		return ev[i].firstDetected.Before(ev[j].firstDetected)
	}
	return ev[i].lastUsed.Before(ev[j].lastUsed)
}

func isImageUsed(imageID string, imagesInUse sets.String) bool {
	// Check the image ID.
	if _, ok := imagesInUse[imageID]; ok {
		return true
	}
	return false
}
