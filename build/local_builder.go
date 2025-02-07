// Copyright © 2021 Alibaba Group Holding Ltd.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package build

import (
	"fmt"
	image2 "github.com/alibaba/sealer/pkg/image"
	cache2 "github.com/alibaba/sealer/pkg/image/cache"
	"github.com/alibaba/sealer/pkg/image/reference"
	store2 "github.com/alibaba/sealer/pkg/image/store"
	"github.com/alibaba/sealer/pkg/logger"
	"github.com/alibaba/sealer/pkg/parser"
	"time"

	"github.com/pkg/errors"

	"github.com/opencontainers/go-digest"

	"github.com/alibaba/sealer/common"
	v1 "github.com/alibaba/sealer/types/api/v1"
	"github.com/alibaba/sealer/utils"
)

type Config struct {
	BuildType string
	NoCache   bool
	ImageName string
}

type builderLayer struct {
	baseLayers []v1.Layer
	newLayers  []v1.Layer
}

// LocalBuilder: local builder using local provider to build a cluster image
type LocalBuilder struct {
	Config           *Config
	Image            *v1.Image
	Cluster          *v1.Cluster
	ImageNamed       reference.Named
	ImageID          string
	Context          string
	KubeFileName     string
	LayerStore       store2.LayerStore
	ImageStore       store2.ImageStore
	ImageService     image2.Service
	Prober           image2.Prober
	FS               store2.Backend
	DockerImageCache *MountTarget
	builderLayer
}

func (l *LocalBuilder) Build(name string, context string, kubefileName string) error {
	err := l.initBuilder(name, context, kubefileName)
	if err != nil {
		return err
	}

	pipLine, err := l.GetBuildPipeLine()
	if err != nil {
		return err
	}

	for _, f := range pipLine {
		if err = f(); err != nil {
			return err
		}
	}
	return nil
}

func (l *LocalBuilder) initBuilder(name string, context string, kubefileName string) error {
	named, err := reference.ParseToNamed(name)
	if err != nil {
		return err
	}

	absContext, absKubeFile, err := ParseBuildArgs(context, kubefileName)
	if err != nil {
		return err
	}

	err = ValidateContextDirectory(absContext)
	if err != nil {
		return err
	}

	l.ImageNamed = named
	l.Context = absContext
	l.KubeFileName = absKubeFile
	return nil
}

func (l *LocalBuilder) GetBuildPipeLine() ([]func() error, error) {
	var buildPipeline []func() error
	if err := l.InitImageSpec(); err != nil {
		return nil, err
	}

	buildPipeline = append(buildPipeline,
		l.PullBaseImageNotExist,
		l.ExecBuild,
		l.CollectRegistryCache,
		l.UpdateImageMetadata,
		l.Cleanup,
	)
	return buildPipeline, nil
}

// init default Image metadata
func (l *LocalBuilder) InitImageSpec() error {
	kubeFile, err := utils.ReadAll(l.KubeFileName)
	if err != nil {
		return fmt.Errorf("failed to load kubefile: %v", err)
	}
	l.Image = parser.NewParse().Parse(kubeFile)
	if l.Image == nil {
		return fmt.Errorf("failed to parse kubefile, image is nil")
	}

	layer0 := l.Image.Spec.Layers[0]
	if layer0.Type != FromCmd {
		return fmt.Errorf("first line of kubefile must start with FROM")
	}

	logger.Info("init image spec success!")
	return nil
}

func (l *LocalBuilder) PullBaseImageNotExist() (err error) {
	if l.Image.Spec.Layers[0].Value == common.ImageScratch {
		return nil
	}
	if err = l.ImageService.PullIfNotExist(l.Image.Spec.Layers[0].Value); err != nil {
		return fmt.Errorf("failed to pull baseImage: %v", err)
	}
	logger.Info("pull base image %s success", l.Image.Spec.Layers[0].Value)
	return nil
}

func (l *LocalBuilder) ExecBuild() error {
	err := l.updateBuilderLayers(l.Image)
	if err != nil {
		return err
	}
	registryCache, err := NewRegistryCache()
	if err != nil {
		return err
	}
	l.DockerImageCache = registryCache
	var (
		canUseCache = !l.Config.NoCache
		parentID    = cache2.ChainID("")
		newLayers   = l.newLayers
	)

	baseLayerPaths := getBaseLayersPath(l.baseLayers)
	chainSvc, err := cache2.NewService()
	if err != nil {
		return err
	}

	hc := handlerContext{
		buildContext:  l.Context,
		continueCache: canUseCache,
		cacheSvc:      chainSvc,
		prober:        l.Prober,
		parentID:      parentID,
		ignoreError:   l.Config.BuildType == common.LiteBuild,
	}

	mhandler := handler{
		hc:         hc,
		layerStore: l.LayerStore,
	}
	for i := 0; i < len(newLayers); i++ {
		// take layer reference
		// we are to modify the layer
		layer := &newLayers[i]
		logger.Info("run build layer: %s %s", layer.Type, layer.Value)
		var (
			layerID digest.Digest
			cacheID digest.Digest
			forErr  error
		)

		switch layer.Type {
		case common.CMDCOMMAND, common.RUNCOMMAND:
			layerID, forErr = mhandler.handleCMDRUNCmd(*layer, baseLayerPaths...)
			if forErr != nil {
				return forErr
			}

		case common.COPYCOMMAND:
			layerID, cacheID, forErr = mhandler.handleCopyCmd(*layer)
			if forErr != nil {
				return forErr
			}
			// hit cache failed, so we save cacheID value to metadata cacheID for this layer.
			// and next time, the cacheID will be used to hit cache.
			if layerID != "" && cacheID != "" && !mhandler.hc.continueCache {
				// TODO set cache id under register.
				forErr = l.SetCacheID(layerID, cacheID.String())
				if forErr != nil {
					logger.Warn("set cache failed layer: %v, err: %v", layer, err)
				}
			}
		}

		layer.ID = layerID
		if layerID == "" {
			continue
		}
		baseLayerPaths = append(baseLayerPaths, l.FS.LayerDataDir(layer.ID))
	}

	logger.Info("exec all build instructs success !")
	return nil
}
func (l *LocalBuilder) CollectRegistryCache() error {
	if l.DockerImageCache == nil {
		return nil
	}
	// wait resource to sync
	time.Sleep(30 * time.Second)
	if !IsAllPodsRunning() {
		return fmt.Errorf("cache docker image failed,cluster pod not running")
	}
	imageLayer := v1.Layer{
		Type:  imageLayerType,
		Value: "",
	}
	layerDgst, err := l.registerLayer(l.DockerImageCache.GetMountUpper())
	if err != nil {
		return err
	}

	imageLayer.ID = layerDgst
	l.newLayers = append(l.newLayers, imageLayer)

	logger.Info("save image cache success")
	return nil
}

//This function only has meaning for copy layers
func (l *LocalBuilder) SetCacheID(layerID digest.Digest, cID string) error {
	return l.FS.SetMetadata(layerID, cacheID, []byte(cID))
}

func (l *LocalBuilder) registerLayer(tempTarget string) (digest.Digest, error) {
	layerDigest, err := l.LayerStore.RegisterLayerForBuilder(tempTarget)
	if err != nil {
		return "", fmt.Errorf("failed to register layer, err: %v", err)
	}

	return layerDigest, nil
}

func (l *LocalBuilder) UpdateImageMetadata() error {
	err := setClusterFileToImage(l.Image, l.ImageNamed.Raw())
	if err != nil {
		return fmt.Errorf("failed to set image metadata, err: %v", err)
	}

	l.Image.Spec.Layers = append(l.baseLayers, l.newLayers...)

	err = l.updateImageIDAndSaveImage()
	if err != nil {
		return fmt.Errorf("failed to save image metadata, err: %v", err)
	}

	logger.Info("update image %s to image metadata success !", l.ImageNamed.Raw())
	return nil
}

func (l *LocalBuilder) updateImageIDAndSaveImage() error {
	imageID, err := generateImageID(*l.Image)
	if err != nil {
		return err
	}

	l.Image.Spec.ID = imageID
	return l.ImageStore.Save(*l.Image, l.ImageNamed.Raw())
}

func (l *LocalBuilder) updateBuilderLayers(image *v1.Image) error {
	// we do not check the len of layers here, because we checked it before.
	// remove the first layer of image
	var (
		layer0    = image.Spec.Layers[0]
		baseImage *v1.Image
		err       error
	)

	// and the layer 0 must be from layer
	if layer0.Value == common.ImageScratch {
		// give a empty image
		baseImage = &v1.Image{}
	} else {
		baseImage, err = l.ImageStore.GetByName(image.Spec.Layers[0].Value)
		if err != nil {
			return fmt.Errorf("failed to get base image while updating base layers, err: %s", err)
		}
	}

	l.baseLayers = append([]v1.Layer{}, baseImage.Spec.Layers...)
	l.newLayers = append([]v1.Layer{}, image.Spec.Layers[1:]...)
	if len(l.baseLayers)+len(l.newLayers) > maxLayerDeep {
		return errors.New("current number of layers exceeds 128 layers")
	}
	return nil
}
func (l *LocalBuilder) Cleanup() (err error) {
	// umount registry
	if l.DockerImageCache != nil {
		l.DockerImageCache.CleanUp()
		return
	}

	return err
}

func NewLocalBuilder(config *Config) (Interface, error) {
	layerStore, err := store2.NewDefaultLayerStore()
	if err != nil {
		return nil, err
	}

	imageStore, err := store2.NewDefaultImageStore()
	if err != nil {
		return nil, err
	}

	service, err := image2.NewImageService()
	if err != nil {
		return nil, err
	}

	fs, err := store2.NewFSStoreBackend()
	if err != nil {
		return nil, fmt.Errorf("failed to init store backend, err: %s", err)
	}

	prober := image2.NewImageProber(service, config.NoCache)

	return &LocalBuilder{
		Config:       config,
		LayerStore:   layerStore,
		ImageStore:   imageStore,
		ImageService: service,
		Prober:       prober,
		FS:           fs,
		builderLayer: builderLayer{
			// for skip golang ci
			baseLayers: []v1.Layer{},
			newLayers:  []v1.Layer{},
		},
	}, nil
}
