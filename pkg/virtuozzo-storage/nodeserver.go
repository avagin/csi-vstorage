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
	"crypto/md5"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"

	"github.com/container-storage-interface/spec/lib/go/csi/v0"
	"github.com/kolyshkin/goploop-cli"
	"golang.org/x/net/context"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"k8s.io/kubernetes/pkg/util/mount"
	"k8s.io/kubernetes/pkg/volume/util"

	"github.com/golang/glog"
	"github.com/avagin/csi-vstorage/pkg/csi-common"
	"github.com/avagin/csi-vstorage/pkg/virtuozzo-storage/vstorage"
)

type nodeServer struct {
	*csicommon.DefaultNodeServer
}

const workingDir = "/var/run/ploop-flexvol/"

func prepareVstorage(clusterName, clusterPasswd string, mount string) error {
	mounted, _ := vstorage.IsVstorage(mount)
	if mounted {
		return nil
	}

	// not mounted in proper place, prepare mount place and check other
	// mounts
	if err := os.MkdirAll(mount, 0700); err != nil {
		return err
	}

	v := vstorage.Vstorage{clusterName}
	p, _ := v.Mountpoint()
	if p != "" {
		return syscall.Mount(p, mount, "", syscall.MS_BIND, "")
	}

	if clusterPasswd == "" {
		return errors.New("Please provide vstorage credentials")
	}

	if err := v.Auth(clusterPasswd); err != nil {
		return err
	}
	if err := v.Mount(mount); err != nil {
		return err
	}

	return nil
}

func mountPloop(target, path string, volume *ploop.Ploop, readonly bool) (string, error) {
	target = filepath.Clean(target)
	path = filepath.Clean(path)

	statePath := fmt.Sprintf("%s/mounts/ploop-%x", workingDir, md5.Sum([]byte(path)))
	mntPath := fmt.Sprintf("%s/mnt", statePath)

	if err := os.MkdirAll(mntPath, 0700); err != nil {
		return "", err
	}
	mp := ploop.MountParam{Target: mntPath, Readonly: readonly}

	_, err := volume.Mount(&mp)
	if err != nil {
		os.Remove(mntPath)
		os.Remove(statePath)
		return "", err
	}

	return statePath, nil
}

func umountPloop(statePath string) error {
	mountPath := fmt.Sprintf("%s/mnt", statePath)
	if err := ploop.UmountByMount(mountPath); err != nil {
		return err
	}

	if err := os.Remove(mountPath); err != nil {
		return fmt.Errorf("Unable to remove %s: %v", mountPath, err)
	}

	if err := os.Remove(statePath); err != nil {
		return fmt.Errorf("Unable to remove %s: %v", statePath, err)
	}

	return nil
}

func (ns *nodeServer) NodePublishVolume(ctx context.Context, req *csi.NodePublishVolumeRequest) (*csi.NodePublishVolumeResponse, error) {

	glog.Infof("NodePublishVolume id %s target %s", req.GetTargetPath(), req.GetVolumeId())

	targetPath := req.GetTargetPath()
	notMnt, err := mount.New("").IsLikelyNotMountPoint(targetPath)
	if err != nil {
		if os.IsNotExist(err) {
			if err := os.MkdirAll(targetPath, 0750); err != nil {
				return nil, status.Error(codes.Internal, err.Error())
			}
			notMnt = true
		} else {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}

	if !notMnt {
		return &csi.NodePublishVolumeResponse{}, nil
	}

	mo := req.GetVolumeCapability().GetMount().GetMountFlags()
	if req.GetReadonly() {
		mo = append(mo, "ro")
	}

	readonly := req.GetReadonly()
	secret := req.GetNodePublishSecrets()
	cluster := secret["clusterName"]
	passwd := secret["clusterPassword"]

	mount := filepath.Join(workingDir, cluster)
	if err := prepareVstorage(cluster, passwd, mount); err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	path := mount
	if secret["volumePath"] != "" {
		path = filepath.Join(path, secret["volumePath"])
	}
	path = filepath.Join(path, req.GetVolumeId())
	volume, err := ploop.Open(filepath.Join(path, "DiskDescriptor.xml"))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	defer volume.Close()

	target := targetPath
	if m, _ := volume.IsMounted(); !m {
		stateDir := fmt.Sprintf("%s/mounts", workingDir)
		if err := os.MkdirAll(stateDir, 0700); err != nil {
			return nil, err
		}

		statePath, err := mountPloop(targetPath, path, &volume, readonly)
		if err != nil {
			return nil, err
		}

		target = filepath.Clean(target)

		// We need to know a mount point to make snapshots, so
		// we create our mount point and then bind-mount it to "target"
		// If it's mounted, let's mount it!
		mntLink := fmt.Sprintf("%s/kube-%x", stateDir, md5.Sum([]byte(target)))

		//glog.Infof("Create symlink %s %s", statePath, mntLink)
		if err := os.Symlink(statePath, mntLink); err != nil {
			umountPloop(statePath)
			return nil, err
		}

		mntPath := fmt.Sprintf("%s/mnt", statePath)
		if err := syscall.Mount(mntPath, targetPath, "", syscall.MS_BIND, ""); err != nil {
			umountPloop(statePath)
			os.Remove(mntLink)
			return nil, fmt.Errorf("Unable to bind mount %s -> %s: %v", mntPath, target, err)
		}

	} else {

		return nil, fmt.Errorf("Ploop volume already mounted")
	}

	return &csi.NodePublishVolumeResponse{}, nil
}

func (ns *nodeServer) NodeUnpublishVolume(ctx context.Context, req *csi.NodeUnpublishVolumeRequest) (*csi.NodeUnpublishVolumeResponse, error) {
	targetPath := req.GetTargetPath()
	notMnt, err := mount.New("").IsLikelyNotMountPoint(targetPath)

	if err != nil {
		if os.IsNotExist(err) {
			return nil, status.Error(codes.NotFound, "Targetpath not found")
		} else {
			return nil, status.Error(codes.Internal, err.Error())
		}
	}
	if notMnt {
		return nil, status.Error(codes.NotFound, "Volume not mounted")
	}

	err = util.UnmountPath(req.GetTargetPath(), mount.New(""))
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}

	mount := targetPath
	mntLink := fmt.Sprintf("%s/mounts/kube-%x", workingDir, md5.Sum([]byte(mount)))
	statePath, err := os.Readlink(mntLink)
	if err != nil {
		return nil, err
	}

	//glog.Infof("Umount %s(%s)", statePath, mntLink)
	if err := umountPloop(statePath); err != nil {
		return nil, err
	}

	if err := os.Remove(mntLink); err != nil {
		return nil, err
	}

	return &csi.NodeUnpublishVolumeResponse{}, nil
}

func (ns *nodeServer) NodeUnstageVolume(ctx context.Context, req *csi.NodeUnstageVolumeRequest) (*csi.NodeUnstageVolumeResponse, error) {
	return &csi.NodeUnstageVolumeResponse{}, nil
}

func (ns *nodeServer) NodeStageVolume(ctx context.Context, req *csi.NodeStageVolumeRequest) (*csi.NodeStageVolumeResponse, error) {
	return &csi.NodeStageVolumeResponse{}, nil
}
