/*
Copyright 2018 Andrei Vagin.

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

package vstorage

import (
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"

	"github.com/golang/glog"
	"golang.org/x/net/context"

	"github.com/container-storage-interface/spec/lib/go/csi/v0"
	"github.com/kolyshkin/goploop-cli"
	"github.com/kubernetes-csi/drivers/pkg/csi-common"
	"github.com/pborman/uuid"
)

const (
	deviceID           = "deviceID"
	provisionRoot      = "/tmp/"
	maxStorageCapacity = 1000
)

type controllerServer struct {
	*csicommon.DefaultControllerServer
}

const provisionerDir = "/export/virtuozzo-provisioner/"
const mountDir = provisionerDir + "mnt/"

func createPloop(mount string, options map[string]string, bytes uint64) error {
	var (
		volumePath, deltasPath, volumeID string
	)

	for k, v := range options {
		switch k {
		case "volumePath":
			volumePath = v
		case "deltasPath":
			deltasPath = v
		case "volumeID":
			volumeID = v
		case "vzsReplicas":
		case "vzsFailureDomain":
		case "vzsEncoding":
		case "vzsTier":
		case "kubernetes.io/readwrite":
		case "kubernetes.io/fsType":
		default:
		}
	}

	if volumePath == "" {
		return fmt.Errorf("volumePath isn't specified")
	}

	if deltasPath == "" {
		deltasPath = volumePath
	}

	if volumeID == "" {
		return fmt.Errorf("volumeID isn't specified")
	}

	// ploop driver takes kilobytes, so convert it
	volumeSize := bytes / 1024

	volumeDir := path.Join(mount, volumePath)
	ploopPath := path.Join(volumeDir, volumeID)

	deltaDir := path.Join(mount, deltasPath)
	// add .image suffix to handle case when deltasPath == volumePath
	imageDir := path.Join(deltaDir, volumeID+".image")
	imageFile := path.Join(imageDir, "root.hds")

	if err := os.MkdirAll(volumeDir, 0755); err != nil {
		return fmt.Errorf("Error creating dir %s: %v", volumeDir, err)
	}

	if err := os.MkdirAll(deltaDir, 0755); err != nil {
		return fmt.Errorf("Error creating dir %s: %v", deltaDir, err)
	}

	// create base dirs for ploop metadatas and ploop images
	if err := os.Mkdir(ploopPath, 0755); err != nil {
		return fmt.Errorf("Error creating dir %s: %v", ploopPath, err)
	}

	if err := os.Mkdir(imageDir, 0755); err != nil {
		os.Remove(ploopPath)
		return fmt.Errorf("Error creating dir %s: %v", imageDir, err)
	}

	for _, d := range []string{ploopPath, imageDir} {
		for k, v := range options {
			attr := ""
			switch k {
			case "vzsReplicas":
				attr = "replicas"
			case "vzsTier":
				attr = "tier"
			case "vzsEncoding":
				attr = "encoding"
			case "vzsFailureDomain":
				attr = "failure-domain"
			}
			if attr == "" {
				continue
			}

			cmd := "vstorage"
			args := []string{"set-attr", "-R", d,
				fmt.Sprintf("%s=%s", attr, v)}
			if err := exec.Command(cmd, args...).Run(); err != nil {
				os.Remove(ploopPath)
				os.Remove(imageDir)
				return fmt.Errorf("Unable to set %s to %s for %s: %v", attr, v, d, err)
			}
		}
	}

	// Create the ploop volume
	_, err := ploop.PloopVolumeCreate(ploopPath, volumeSize, imageFile)
	if err != nil {
		os.RemoveAll(ploopPath)
		os.RemoveAll(imageDir)
		return err
	}

	return nil
}

func removePloop(mount string, options map[string]string) error {
	volumePath := options["volumePath"]
	volumeID := options["volumeID"]
	deltasPath, ok := options["deltasPath"]
	if !ok {
		deltasPath = volumePath
	}
	imageDir := path.Join(mount, deltasPath, volumeID+".image")
	ploopPath := path.Join(mount, options["volumePath"], options["volumeID"])
	ploopPathTmp := path.Join(mount, options["volumePath"], options["volumeID"]+".deleted")
	err := os.Rename(ploopPath, ploopPathTmp)
	if err != nil {
		return err
	}

	cmd := "vstorage"
	args := []string{"revoke", "-R", imageDir}
	err = exec.Command(cmd, args...).Run()
	if err != nil {
		glog.Errorf("Unable to revoke a lease for %s", imageDir)
	}

	vol, err := ploop.PloopVolumeOpen(ploopPathTmp)
	if err != nil {
		return err
	}
	glog.Infof("Delete: %s", ploopPathTmp)
	err = vol.Delete()
	if err != nil {
		return err
	}
	os.RemoveAll(imageDir)
	return nil
}

func (cs *controllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	// Volume Name
	volName := req.GetName()
	if len(volName) == 0 {
		volName = uuid.NewUUID().String()
	}

	// Volume Size - Default is 1 GiB
	volSizeBytes := uint64(1 * 1024 * 1024 * 1024)
	if req.GetCapacityRange() != nil {
		volSizeBytes = uint64(req.GetCapacityRange().GetRequiredBytes())
	}

	storageClassOptions := map[string]string{}

	for k, v := range req.GetParameters() {
		storageClassOptions[k] = v
	}
	share := fmt.Sprintf("kubernetes-dynamic-pvc-%s", volName)
	storageClassOptions["volumeID"] = share
	storageClassOptions["size"] = fmt.Sprintf("%d", volSizeBytes)
	cluster := storageClassOptions["cluster"]
	password := storageClassOptions["passwd"]
	mount := filepath.Join(workingDir, cluster)
	if err := prepareVstorage(cluster, password, mount); err != nil {
		return nil, err
	}

	if err := createPloop(mount, storageClassOptions, volSizeBytes); err != nil {
		return nil, err
	}

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			Id:         volName,
			Attributes: storageClassOptions,
		},
	}, nil
}

func (cs *controllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {

	return &csi.DeleteVolumeResponse{}, nil
}

func (cs *controllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {

	return &csi.ValidateVolumeCapabilitiesResponse{Supported: true, Message: ""}, nil
}

func (cs *controllerServer) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	// Publish Volume Info
	pvInfo := map[string]string{}
	return &csi.ControllerPublishVolumeResponse{
		PublishInfo: pvInfo,
	}, nil
}

func (cs *controllerServer) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	return &csi.ControllerUnpublishVolumeResponse{}, nil
}
