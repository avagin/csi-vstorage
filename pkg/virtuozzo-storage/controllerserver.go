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
	"encoding/xml"
	"fmt"
	"io/ioutil"
	"os"
	"os/exec"
	"path"
	"path/filepath"

	"github.com/golang/glog"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/avagin/csi-vstorage/pkg/csi-common"
	"github.com/container-storage-interface/spec/lib/go/csi/v0"
	"github.com/kolyshkin/goploop-cli"
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

func createPloop(volumeID, mount string, secret map[string]string, options map[string]string, bytes uint64) error {
	var (
		volumePath, deltasPath string
	)

	for k, v := range secret {
		switch k {
		case "volumePath":
			volumePath = v
		case "deltasPath":
			deltasPath = v
		}
	}
	for k, v := range options {
		switch k {
		case "vzsReplicas":
		case "vzsFailureDomain":
		case "vzsEncoding":
		case "vzsTier":
		case "kubernetes.io/readwrite":
		case "kubernetes.io/fsType":
		default:
			glog.Errorf("Unknown parameter: %v = %v", k, v)
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

func removePloop(volumeID, mount string, options map[string]string) error {
	volumePath := options["volumePath"]
	deltasPath, ok := options["deltasPath"]
	if !ok {
		deltasPath = volumePath
	}
	imageDir := path.Join(mount, deltasPath, volumeID+".image")
	ploopPath := path.Join(mount, options["volumePath"], volumeID)
	ploopPathTmp := path.Join(mount, options["volumePath"], volumeID+".deleted")
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

type DiskParameters struct {
	DiskSize uint64 `xml:"Disk_size"`
}

type ParallelsDiskImage struct {
	DiskParameters DiskParameters `xml:"Disk_Parameters"`
}

func getPloopCapacity(ploopPath string) (uint64, error) {
	data, err := ioutil.ReadFile(filepath.Join(ploopPath, "DiskDescriptor.xml"))
	if err != nil {
		return 0, err
	}

	v := ParallelsDiskImage{}
	err = xml.Unmarshal([]byte(data), &v)
	if err != nil {
		return 0, err
	}
	return v.DiskParameters.DiskSize * 512, nil
}

func (cs *controllerServer) CreateVolume(ctx context.Context, req *csi.CreateVolumeRequest) (*csi.CreateVolumeResponse, error) {
	if err := cs.Driver.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		glog.V(3).Infof("invalid create volume req: %v", req)
		return nil, err
	}

	// Check arguments
	if len(req.GetName()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Name missing in request")
	}
	if req.GetVolumeCapabilities() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume Capabilities missing in request")
	}

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
	storageClassOptions["size"] = fmt.Sprintf("%d", volSizeBytes)

	secret := req.GetControllerCreateSecrets()
	cluster := secret["clusterName"]
	password := secret["clusterPassword"]

	mount := filepath.Join(workingDir, cluster)
	if err := prepareVstorage(cluster, password, mount); err != nil {
		return nil, err
	}

	volumeDir := path.Join(mount, secret["volumePath"])
	ploopPath := path.Join(volumeDir, volName)

	_, err := os.Stat(ploopPath)
	if err == nil {
		capacity, err := getPloopCapacity(ploopPath)
		if err != nil {
			return nil, err
		}
		if capacity >= volSizeBytes {
			return &csi.CreateVolumeResponse{
				Volume: &csi.Volume{
					Id:            volName,
					Attributes:    storageClassOptions,
					CapacityBytes: int64(volSizeBytes),
				},
			}, nil
		} else {
			return nil, status.Error(codes.AlreadyExists, fmt.Sprintf("Volume with the same name: %s but with different size already exist", req.GetName()))
		}
	}
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}

	if err := createPloop(volName, mount, secret, storageClassOptions, volSizeBytes); err != nil {
		return nil, err
	}

	return &csi.CreateVolumeResponse{
		Volume: &csi.Volume{
			Id:            volName,
			Attributes:    storageClassOptions,
			CapacityBytes: int64(volSizeBytes),
		},
	}, nil
}

func (cs *controllerServer) DeleteVolume(ctx context.Context, req *csi.DeleteVolumeRequest) (*csi.DeleteVolumeResponse, error) {
	// Check arguments
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	if err := cs.Driver.ValidateControllerServiceRequest(csi.ControllerServiceCapability_RPC_CREATE_DELETE_VOLUME); err != nil {
		glog.V(3).Infof("invalid delete volume req: %v", req)
		return nil, err
	}
	volumeID := req.GetVolumeId()
	secret := req.GetControllerDeleteSecrets()

	cluster := secret["clusterName"]
	password := secret["clusterPassword"]
	mount := filepath.Join(workingDir, cluster)
	if err := prepareVstorage(cluster, password, mount); err != nil {
		return nil, err
	}

	ploopPath := path.Join(mount, secret["volumePath"], volumeID)
	_, err := os.Stat(ploopPath)
	if err != nil && os.IsNotExist(err) {
		return &csi.DeleteVolumeResponse{}, nil
	}
	if err != nil {
		return nil, err
	}

	if err := removePloop(volumeID, mount, secret); err != nil {
		return nil, err
	}

	return &csi.DeleteVolumeResponse{}, nil
}

func (cs *controllerServer) ValidateVolumeCapabilities(ctx context.Context, req *csi.ValidateVolumeCapabilitiesRequest) (*csi.ValidateVolumeCapabilitiesResponse, error) {
	// Check arguments
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if req.GetVolumeCapabilities() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume capabilities missing in request")
	}

	return &csi.ValidateVolumeCapabilitiesResponse{Supported: true, Message: ""}, nil
}

func (cs *controllerServer) ControllerPublishVolume(ctx context.Context, req *csi.ControllerPublishVolumeRequest) (*csi.ControllerPublishVolumeResponse, error) {
	// Check arguments
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if len(req.GetNodeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}
	if req.GetVolumeCapability() == nil {
		return nil, status.Error(codes.InvalidArgument, "Volume Capability missing in request")
	}

	// Publish Volume Info
	pvInfo := map[string]string{}
	return &csi.ControllerPublishVolumeResponse{
		PublishInfo: pvInfo,
	}, nil
}

func (cs *controllerServer) ControllerUnpublishVolume(ctx context.Context, req *csi.ControllerUnpublishVolumeRequest) (*csi.ControllerUnpublishVolumeResponse, error) {
	// Check arguments
	if len(req.GetVolumeId()) == 0 {
		return nil, status.Error(codes.InvalidArgument, "Volume ID missing in request")
	}

	return &csi.ControllerUnpublishVolumeResponse{}, nil
}
