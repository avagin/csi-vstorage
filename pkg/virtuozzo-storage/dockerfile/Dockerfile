FROM centos:7

ADD virtuozzo.repo /etc/yum.repos.d/
ADD vzlinux.repo /etc/yum.repos.d/
ADD openvz-factory.repo /etc/yum.repos.d/
RUN printf "upgrade \n install vstorage-ctl vstorage-client ploop gdisk \n clean all \n run" | yum shell -y

# Copy nfsplugin from build _output directory
COPY _output/vstorageplugin /vstorageplugin

ENTRYPOINT ["/vstorageplugin"]
