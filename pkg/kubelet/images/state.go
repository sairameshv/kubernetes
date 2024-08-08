/*
Copyright 2016 The Kubernetes Authors.

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
	v1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"os"
	"path/filepath"
	"sync"
	"time"

	utiljson "k8s.io/apimachinery/pkg/util/json"
	"k8s.io/klog/v2"
)

const imageManagerstateFileName = "image_state_manager"

// ImagePullCacheMap type maps the imageRef to the ImagePullInfo
type ImagePullCacheMap map[string]*ImagePullInfo

// EnsuredInfo contains the data if an image is ensured and the last ensured time
type EnsuredInfo struct {
	// true for ensured secret
	Ensured bool `json:"ensured"`
	// the secret should be verified again if current time is after the due date.
	// and the due date is `PullImageSecretRecheckPeriod` after last ensured date.
	// `PullImageSecretRecheckPeriod` is configurable in kubelet config.
	LastEnsuredDate time.Time `json:"lastEnsuredDate"`
}

type ImagePullInfo struct {
	// TODO: (mikebrow) time of last pull for this imageRef
	// TODO: (mikebrow) time of pull for each particular auth hash
	//       note @mrunalp makes a good point that we can utilize /apimachinery/pkg/util/sets/string.go here
	mux sync.RWMutex

	// map of auths hash (keys) used to successfully pull this imageref
	Auths map[string]*EnsuredInfo `json:"auths"`
}

// reader interface used to retrieve image manager's cache
type reader interface {
	loadImageManagerCache(imageRef string) error
	getImagePullInfo(imageRef string) (pullInfo *ImagePullInfo)
	getAuthInfo(imageRef, hash string) (authInfo *EnsuredInfo)
}

// writer interface helps in updating the image manager's cache
type writer interface {
	storeImageManagerCache(imageRef string) error
	refreshImageManagerCache(recheck bool, recheckPeriod v1.Duration) error
	setImagePullInfo(imageRef string, hash string, data *EnsuredInfo) error
	deleteImagePullInfo(imageRef string) error
	setAuthInfo(imageRef, hash string, data *EnsuredInfo)
	deleteAuthInfo(imageRef, hash string)
}

// State interface provides methods for tracking and setting image manager cache
type State interface {
	reader
	writer
}

type imageManagerCache struct {
	lock  sync.RWMutex
	cache ImagePullCacheMap
	// imageStateManagerPath is the file path to store the image manager cache
	imageStateManagerPath string
}

func newImageManagerCache(rootDir string, imageRefs []string) *imageManagerCache {
	cache := &imageManagerCache{
		cache:                 make(ImagePullCacheMap),
		imageStateManagerPath: filepath.Join(rootDir, imageManagerstateFileName),
	}
	// load the cache data from the disk
	for _, imageRef := range imageRefs {
		err := cache.loadImageManagerCache(imageRef)
		if err != nil {
			klog.Errorf("Failed to load image manager cache for image ref %s: %v", imageRef, err)
			return nil
		}
	}
	return cache
}

func (c *imageManagerCache) getImagePullInfo(imageRef string) (pullInfo *ImagePullInfo) {
	c.lock.Lock()
	defer c.lock.Unlock()
	// load the data from the disk
	c.loadImageManagerCache(imageRef)
	if c.cache != nil {
		if _, ok := c.cache[imageRef]; ok {
			return c.cache[imageRef]
		}
	}
	return nil
}

func (c *imageManagerCache) getAuthInfo(imageRef, hash string) (authInfo *EnsuredInfo) {

	pullInfo := c.getImagePullInfo(imageRef)
	if pullInfo == nil {
		return nil
	}
	c.lock.Lock()
	defer c.lock.Unlock()
	c.cache[imageRef].mux.Lock()
	defer c.cache[imageRef].mux.Unlock()
	if _, ok := pullInfo.Auths[hash]; ok {
		return c.cache[imageRef].Auths[hash]
	}
	return nil
}

func (c *imageManagerCache) setImagePullInfo(imageRef string, hash string, data *EnsuredInfo) error {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.cache == nil {
		c.cache = make(ImagePullCacheMap)
	}
	authInfo := make(map[string]*EnsuredInfo)
	authInfo[hash] = data
	c.cache[imageRef] = &ImagePullInfo{Auths: authInfo}
	return c.storeImageManagerCache(imageRef)
}

func (c *imageManagerCache) deleteImagePullInfo(imageRef string) error {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.cache != nil {
		delete(c.cache, imageRef)
	}
	// Also delete from the disk
	if err := os.Remove(filepath.Join(c.imageStateManagerPath, imageRef)); err != nil {
		klog.Errorf("Failed to delete image manager cache for image ref %s: %v", imageRef, err)
		return err
	}
	return nil
}

func (c *imageManagerCache) setAuthInfo(imageRef, hash string, data *EnsuredInfo) {
	if c.cache == nil {
		c.cache = make(ImagePullCacheMap)
	}
	auth := make(map[string]*EnsuredInfo)
	auth[hash] = data
	c.lock.Lock()
	c.cache[imageRef] = &ImagePullInfo{Auths: auth}
	c.lock.Unlock()
}

func (c *imageManagerCache) deleteAuthInfo(imageRef, hash string) {
	c.lock.Lock()
	defer c.lock.Unlock()
	if c.cache != nil {
		if _, ok := c.cache[imageRef]; ok {
			c.cache[imageRef].mux.Lock()
			defer c.cache[imageRef].mux.Unlock()
			if _, present := c.cache[imageRef].Auths[hash]; present {
				delete(c.cache[imageRef].Auths, hash)
			}
		}
	}
}

func (c *imageManagerCache) storeImageManagerCache(imageRef string) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	info := c.getImagePullInfo(imageRef)
	if info == nil || imageRef == "" {
		return nil
	}
	// store the info to the disk
	byteData, err := utiljson.Marshal(info)
	if err != nil {
		return err
	}
	path := filepath.Join(c.imageStateManagerPath, imageRef)
	err = os.WriteFile(path, byteData, 0644)
	if err != nil {
		return err
	}
	return nil
}

func (c *imageManagerCache) loadImageManagerCache(imageRef string) error {
	path := filepath.Join(c.imageStateManagerPath, imageRef)
	_, err := os.Stat(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		} else {
			klog.ErrorS(err, "Failed to stat image manager cache file", "file", path)
			return err
		}
	}
	if imageRef == "" {
		return nil
	}
	c.lock.Lock()
	defer c.lock.Unlock()
	// load the info from the disk
	byteData, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	info := new(ImagePullInfo)
	err = utiljson.Unmarshal(byteData, info)
	if err != nil {
		return err
	}
	c.cache[imageRef] = info
	return nil
}

func (c *imageManagerCache) refreshImageManagerCache(recheck bool, recheckPeriod v1.Duration) error {
	var lock sync.RWMutex
	lock.Lock()
	defer lock.Unlock()
	if recheck && recheckPeriod.Duration == 0 {
		// Based on the design proposal of the enhancement, the kubelet is not supposed to invalidate the cache
		// Reference: https://github.com/kubernetes/enhancements/tree/master/keps/sig-node/2535-ensure-secret-pulled-images#proposal
		return nil
	}
	for k, v := range c.cache {
		for i, auth := range v.Auths {
			if auth != nil && auth.LastEnsuredDate.Add(recheckPeriod.Duration).Before(time.Now()) {
				c.deleteAuthInfo(k, i)
			}
			/* TODO: When do we delete the metadata?
			if len(v.Auths) == 0 {
				delete(m.ensureSecretPulledImages, k)
			}
			*/
			if err := c.setImagePullInfo(k, i, auth); err != nil {
				klog.Errorf("Failed to set image pull info for image manager cache %s: %v", k, err)
				return err
			}
		}
	}
	return nil
}
