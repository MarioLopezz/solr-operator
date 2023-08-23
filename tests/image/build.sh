podman build . --tag docker.io/kubernetesbigdataeg/solr:8.11.1-1
podman login docker.io -u kubernetesbigdataeg
podman push docker.io/kubernetesbigdataeg/solr:8.11.1-1
