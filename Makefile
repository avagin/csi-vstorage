# Copyright 2018 Andrei Vagin.
#
# Licensed under the Apache License, Version 2.0 (the "License");
# you may not use this file except in compliance with the License.
# You may obtain a copy of the License at
#
#     http://www.apache.org/licenses/LICENSE-2.0
#
# Unless required by applicable law or agreed to in writing, software
# distributed under the License is distributed on an "AS IS" BASIS,
# WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
# See the License for the specific language governing permissions and
# limitations under the License.

.PHONY: all clean virtuozzo-storage

all: vstorage vstorage-ct

vstorage-test:
	docker build -t csi-vstorage-test -f ./pkg/virtuozzo-storage/dockerfile/Dockerfile.test .
	docker run --network=host --privileged csi-vstorage-test bash hack/vstorage-test.sh
test:
	go test github.com/kubernetes-csi/drivers/pkg/... -cover
	go vet github.com/kubernetes-csi/drivers/pkg/...
vstorage:
	if [ ! -d ./vendor ]; then dep ensure -vendor-only; fi
	CGO_ENABLED=0 GOOS=linux go build -a -ldflags '-extldflags "-static"' -o _output/vstorageplugin ./app/vstorageplugin
vstorage-ct:
	docker build -t docker.io/avagin/vstorageplugin:v0.2.0 -f pkg/virtuozzo-storage/dockerfile/Dockerfile .
clean:
	go clean -r -x
	-rm -rf _output
