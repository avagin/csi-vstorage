#!/bin/bash

## This file is for app/hostpathplugin
## It could be used for other apps in this repo, but
## those applications may or may not take the same
## arguments

## Must be run from the root of the repo

UDS="/tmp/e2e-csi-sanity.sock"
CSI_ENDPOINT="unix://${UDS}"
CSI_MOUNTPOINT="/mnt"
CSI_SECRET=$GOPATH/src/github.com/avagin/csi-vstorage/hack/vstorage.secret
APP=vstorageplugin

# Start the application in the background
$GOPATH/src/github.com/avagin/csi-vstorage/_output/$APP --endpoint=$CSI_ENDPOINT --nodeid=1 &
pid=$!

# Need to skip Capacity testing since hostpath does not support it
$GOPATH/src/github.com/kubernetes-csi/csi-test/cmd/csi-sanity/csi-sanity --csi.mountdir=$CSI_MOUNTPOINT --csi.endpoint=$CSI_ENDPOINT --csi.secretfile $CSI_SECRET ; ret=$?
kill -9 $pid
rm -f $UDS

if [ $ret -ne 0 ] ; then
	exit $ret
fi

exit 0
