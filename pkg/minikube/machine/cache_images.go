/*
Copyright 2016 The Kubernetes Authors All rights reserved.

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

package machine

import (
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/golang/glog"
	"github.com/google/go-containerregistry/pkg/authn"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/daemon"
	"github.com/google/go-containerregistry/pkg/v1/remote"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
	"github.com/pkg/errors"
	"golang.org/x/sync/errgroup"
	"k8s.io/minikube/pkg/minikube/assets"
	"k8s.io/minikube/pkg/minikube/bootstrapper"
	"k8s.io/minikube/pkg/minikube/command"
	"k8s.io/minikube/pkg/minikube/config"
	"k8s.io/minikube/pkg/minikube/constants"
	"k8s.io/minikube/pkg/minikube/cruntime"
	"k8s.io/minikube/pkg/minikube/vmpath"
)

// loadRoot is where images should be loaded from within the guest VM
var loadRoot = path.Join(vmpath.GuestPersistentDir, "images")

var getWindowsVolumeName = getWindowsVolumeNameCmd

// loadImageLock is used to serialize image loads to avoid overloading the guest VM
var loadImageLock sync.Mutex

// CacheImagesForBootstrapper will cache images for a bootstrapper
func CacheImagesForBootstrapper(imageRepository string, version string, clusterBootstrapper string) error {
	images := bootstrapper.GetCachedImageList(imageRepository, version, clusterBootstrapper)

	if err := CacheImages(images, constants.ImageCacheDir); err != nil {
		return errors.Wrapf(err, "Caching images for %s", clusterBootstrapper)
	}

	return nil
}

// CacheImages will cache images on the host
//
// The cache directory currently caches images using the imagename_tag
// For example, k8s.gcr.io/kube-addon-manager:v6.5 would be
// stored at $CACHE_DIR/k8s.gcr.io/kube-addon-manager_v6.5
func CacheImages(images []string, cacheDir string) error {
	var g errgroup.Group
	for _, image := range images {
		image := image
		g.Go(func() error {
			dst := filepath.Join(cacheDir, image)
			dst = sanitizeCacheDir(dst)
			if err := CacheImage(image, dst); err != nil {
				glog.Errorf("CacheImage %s -> %s failed: %v", image, dst, err)
				return errors.Wrapf(err, "caching image %s", dst)
			}
			glog.Infof("CacheImage %s -> %s succeeded", image, dst)
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return errors.Wrap(err, "caching images")
	}
	glog.Infoln("Successfully cached all images.")
	return nil
}

// LoadImages loads previously cached images into the container runtime
func LoadImages(cmd command.Runner, images []string, cacheDir string) error {
	glog.Infof("LoadImages start: %s", images)
	defer glog.Infof("LoadImages end")

	var g errgroup.Group
	// Load profile cluster config from file
	cc, err := config.Load()
	if err != nil && !os.IsNotExist(err) {
		glog.Errorln("Error loading profile config: ", err)
	}
	for _, image := range images {
		image := image
		g.Go(func() error {
			src := filepath.Join(cacheDir, image)
			src = sanitizeCacheDir(src)
			if err := transferAndLoadImage(cmd, cc.KubernetesConfig, src); err != nil {
				glog.Warningf("Failed to load %s: %v", src, err)
				return errors.Wrapf(err, "loading image %s", src)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return errors.Wrap(err, "loading cached images")
	}
	glog.Infoln("Successfully loaded all cached images.")
	return nil
}

// CacheAndLoadImages caches and loads images
func CacheAndLoadImages(images []string) error {
	if err := CacheImages(images, constants.ImageCacheDir); err != nil {
		return err
	}
	api, err := NewAPIClient()
	if err != nil {
		return err
	}
	defer api.Close()
	cc, err := config.Load()
	if err != nil {
		return err
	}
	h, err := api.Load(cc.Name)
	if err != nil {
		return err
	}

	runner, err := CommandRunner(h)
	if err != nil {
		return err
	}
	return LoadImages(runner, images, constants.ImageCacheDir)
}

// # ParseReference cannot have a : in the directory path
func sanitizeCacheDir(image string) string {
	if runtime.GOOS == "windows" && hasWindowsDriveLetter(image) {
		// not sanitize Windows drive letter.
		s := image[:2] + strings.Replace(image[2:], ":", "_", -1)
		glog.Infof("windows sanitize: %s -> %s", image, s)
		return s
	}
	return strings.Replace(image, ":", "_", -1)
}

func hasWindowsDriveLetter(s string) bool {
	if len(s) < 3 {
		return false
	}

	drive := s[:3]
	for _, b := range "CDEFGHIJKLMNOPQRSTUVWXYZABcdefghijklmnopqrstuvwxyzab" {
		if d := string(b) + ":"; drive == d+`\` || drive == d+`/` {
			return true
		}
	}

	return false
}

// Replace a drive letter to a volume name.
func replaceWinDriveLetterToVolumeName(s string) (string, error) {
	vname, err := getWindowsVolumeName(s[:1])
	if err != nil {
		return "", err
	}
	path := vname + s[3:]

	return path, nil
}

func getWindowsVolumeNameCmd(d string) (string, error) {
	cmd := exec.Command("wmic", "volume", "where", "DriveLetter = '"+d+":'", "get", "DeviceID")

	stdout, err := cmd.Output()
	if err != nil {
		return "", err
	}

	outs := strings.Split(strings.Replace(string(stdout), "\r", "", -1), "\n")

	var vname string
	for _, l := range outs {
		s := strings.TrimSpace(l)
		if strings.HasPrefix(s, `\\?\Volume{`) && strings.HasSuffix(s, `}\`) {
			vname = s
			break
		}
	}

	if vname == "" {
		return "", errors.New("failed to get a volume GUID")
	}

	return vname, nil
}

// transferAndLoadImage transfers and loads a single image from the cache
func transferAndLoadImage(cr command.Runner, k8s config.KubernetesConfig, src string) error {
	glog.Infof("Loading image from cache: %s", src)
	filename := filepath.Base(src)
	if _, err := os.Stat(src); err != nil {
		return err
	}
	dst := path.Join(loadRoot, filename)
	f, err := assets.NewFileAsset(src, loadRoot, filename, "0644")
	if err != nil {
		return errors.Wrapf(err, "creating copyable file asset: %s", filename)
	}
	if err := cr.Copy(f); err != nil {
		return errors.Wrap(err, "transferring cached image")
	}

	r, err := cruntime.New(cruntime.Config{Type: k8s.ContainerRuntime, Runner: cr})
	if err != nil {
		return errors.Wrap(err, "runtime")
	}
	loadImageLock.Lock()
	defer loadImageLock.Unlock()

	err = r.LoadImage(dst)
	if err != nil {
		return errors.Wrapf(err, "%s load %s", r.Name(), dst)
	}

	glog.Infof("Successfully loaded image %s from cache", src)
	return nil
}

// DeleteFromImageCacheDir deletes images from the cache
func DeleteFromImageCacheDir(images []string) error {
	for _, image := range images {
		path := filepath.Join(constants.ImageCacheDir, image)
		path = sanitizeCacheDir(path)
		glog.Infoln("Deleting image in cache at ", path)
		if err := os.Remove(path); err != nil {
			return err
		}
	}
	return cleanImageCacheDir()
}

func cleanImageCacheDir() error {
	err := filepath.Walk(constants.ImageCacheDir, func(path string, info os.FileInfo, err error) error {
		// If error is not nil, it's because the path was already deleted and doesn't exist
		// Move on to next path
		if err != nil {
			return nil
		}
		// Check if path is directory
		if !info.IsDir() {
			return nil
		}
		// If directory is empty, delete it
		entries, err := ioutil.ReadDir(path)
		if err != nil {
			return err
		}
		if len(entries) == 0 {
			if err = os.Remove(path); err != nil {
				return err
			}
		}
		return nil
	})
	return err
}

func getDstPath(dst string) (string, error) {
	if runtime.GOOS == "windows" && hasWindowsDriveLetter(dst) {
		// ParseReference does not support a Windows drive letter.
		// Therefore, will replace the drive letter to a volume name.
		var err error
		if dst, err = replaceWinDriveLetterToVolumeName(dst); err != nil {
			return "", errors.Wrap(err, "parsing docker archive dst ref: replace a Win drive letter to a volume name")
		}
	}

	return dst, nil
}

// CacheImage caches an image
func CacheImage(image, dst string) error {
	start := time.Now()
	glog.Infof("CacheImage: %s -> %s", image, dst)
	defer func() {
		glog.Infof("CacheImage: %s -> %s completed in %s", image, dst, time.Since(start))
	}()

	if _, err := os.Stat(dst); err == nil {
		glog.Infof("%s exists", dst)
		return nil
	}

	dstPath, err := getDstPath(dst)
	if err != nil {
		return errors.Wrap(err, "getting destination path")
	}

	if err := os.MkdirAll(filepath.Dir(dstPath), 0777); err != nil {
		return errors.Wrapf(err, "making cache image directory: %s", dst)
	}

	ref, err := name.ParseReference(image, name.WeakValidation)
	if err != nil {
		return errors.Wrap(err, "creating docker image name")
	}

	img, err := retrieveImage(ref)
	if err != nil {
		return errors.Wrap(err, "fetching image")
	}

	glog.Infoln("OPENING: ", dstPath)
	f, err := ioutil.TempFile(filepath.Dir(dstPath), filepath.Base(dstPath)+".*.tmp")
	if err != nil {
		return err
	}
	tag, err := name.NewTag(image, name.WeakValidation)
	if err != nil {
		return err
	}
	err = tarball.Write(tag, img, &tarball.WriteOptions{}, f)
	if err != nil {
		return err
	}
	err = f.Close()
	if err != nil {
		return err
	}
	err = os.Rename(f.Name(), dstPath)
	if err != nil {
		return err
	}
	glog.Infof("%s exists", dst)
	return nil
}

func retrieveImage(ref name.Reference) (v1.Image, error) {
	glog.Infof("retrieving image: %+v", ref)
	img, err := daemon.Image(ref)
	if err == nil {
		glog.Infof("found %s locally; caching", ref.Name())
		return img, err
	}
	glog.Infof("daemon image for %+v: %v", img, err)
	img, err = remote.Image(ref, remote.WithAuthFromKeychain(authn.DefaultKeychain))
	if err == nil {
		return img, err
	}
	glog.Warningf("failed authn download for %+v (trying anon): %+v", ref, err)
	img, err = remote.Image(ref)
	if err != nil {
		glog.Warningf("failed anon download for %+v: %+v", ref, err)
	}
	return img, err
}
