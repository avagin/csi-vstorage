# CSI NFS driver


## Kubernetes
### Requirements

The folllowing feature gates and runtime config have to be enabled to deploy the driver

```
FEATURE_GATES=CSIPersistentVolume=true,MountPropagation=true
RUNTIME_CONFIG="storage.k8s.io/v1alpha1=true"
```

Mountprogpation requries support for privileged containers. So, make sure privileged containers are enabled in the cluster.

### Example local-up-cluster.sh

```ALLOW_PRIVILEGED=true FEATURE_GATES=CSIPersistentVolume=true,MountPropagation=true RUNTIME_CONFIG="storage.k8s.io/v1alpha1=true" LOG_LEVEL=5 hack/local-up-cluster.sh```

### Deploy

```kubectl -f deploy/kubernetes create```

### Example Nginx application
Please update the NFS Server & share information in nginx.yaml file.

```kubectl -f examples/kubernetes/nginx.yaml create```

## Using CSC tool

### Build vstorageplugin
```
$ make vstorage
```

### Start Virtuozzo Storage driver
```
$ sudo ./_output/vstorageplugin --endpoint tcp://127.0.0.1:10000 --nodeid CSINode -v=5
```
