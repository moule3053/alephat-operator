# αlephαt: Kubernetes operator for multi-cluster resource management
A simple way to manage Kubernetes resources across a fleet of Kubernetes clusters.

## Description
The αlephαt operator runs on a management cluster from where you can create, update and delete Kubernetes resources across a fleet of Kubernetes clusters.

It allows you to deploy applications across multiple Kubernetes clusters by using the usual Kubernetes yaml manifests for your resources.

It's just a simple wrapper around the Kubernetes yaml manifests.

## Architecture

αlephαt requires a dedicated Kubernetes cluster called `management cluster` where the CRD and operstor reside.

The `management cluster` is responsible for managing Kubernetes resouces across a number of `workload clusters`.

αlephαt requires that the Kubeconfig files used by the `management cluster` to authenticate against the `workload clusters` be stored as secrets in the `management cluster`. The naming convenstion for the secrets is `kubeconfig-<cluster-name>`.

## How does αlephαt work?

αlephαt defines the `MulticlusterResource` custom resource.

The manifest for a `MultiClusterResource` resouce looks like the following.

```
apiVersion: alephat.io/v1
kind: MultiClusterResource
metadata:
  name: <multiclusterresource-name>
spec:
  targetClusters: <target clusters names comma separated>
  resourceManifest:
    <resource-name>.yaml: |
      <normal kubernetes yaml manifest>
```

For example, to deploy a Kubernetes Deployment across three clusters names `cluster1`, `cluster2` and `cluster3`, you can apply the following manifest.

```
apiVersion: alephat.io/v1
kind: MultiClusterResource
metadata:
  name: multicluster-deployment-nginx
spec:
  targetClusters: cluster1, cluster2, cluster3
  resourceManifest:
    deployment.yaml: |
      apiVersion: apps/v1
      kind: Deployment
      metadata:
        name: nginx-deployment
        namespace: nginx-namespace
        labels:
          app: nginx
      spec:
        replicas: 2
        selector:
          matchLabels:
            app: nginx
        template:
          metadata:
            labels:
              app: nginx
          spec:
            containers:
            - name: nginx
              image: nginx:1.14.2
              ports:
              - containerPort: 80
```

## Getting Started

## Users

### Prerequisites
- kubectl version v1.11.3+.
- Access to Kubernetes v1.11.3+ clusters, one management and a few workload clusters. For development you can use tools like [minikube](https://minikube.sigs.k8s.io/) or [kind](https://kind.sigs.k8s.io/) to create clusters on your local machine.

### Member clusters' kubeconfig as secret on the management cluster

The operator reads the Kubeconfig files of the member clusters from secrets on the management cluster.

Please create the secrets using the naming convention `kubeconfig-<cluster-name>`.

For example to create the Kubeconfig secret for cluster1, do as follows:

```
kubectl create secret generic kubeconfig-cluster1 --from-literal=kubeconfig='apiVersion: v1
clusters:
- cluster:
    certificate-authority-data: <certificate-authority-data>
    server: https://<control-plane-ip>:<port>
  name: kind-cluster1
contexts:
- context:
    cluster: kind-cluster1
    user: kind-cluster1
  name: kind-cluster1
current-context: kind-cluster1
kind: Config
preferences: {}
users:
- name: kind-cluster1
  user:
    client-certificate-data: <client-certificate-data>
    client-key-data: <client-key-data>'
```

### Connectivity

Make sure that there is network connectivity from the management cluster's control plane to the control planes of each of the member clusters.

### Deploy the CRD (Custom Resource Definition)

Deploy the CRD needed for the operator

```
kubectl apply -f deployment/operator/crd.yaml
```

### Create namespace

Create a namespace for the operator Deployment and related resources to reside in

```
kubectl apply -f deployment/operator/ns.yaml
```

### RBAC (Role Based Access Control)

Create the ServiceAccount, ClusterRole and ClusterRoleBinding needed for the operator to access resources in the management cluster

```
kubectl apply -f deployment/operator/rbac.yaml
```

### Deploy the operator

The operator runs as a Kubernetes operator

```
kubectl apply -f deployment/operator/operator-deployment.yaml
```

### Deploy sample resources

Deploy a sample application that creates a namespace, Deployment and Service acroos three clusters.

```
kubectl apply -f deployment/sample/sample-multiclusterresource.yaml
```


## To build from the source yourself

### Prerequisites
- go version v1.22.0+
- docker version 17.03+.
- kubectl version v1.11.3+.
- Access to Kubernetes v1.11.3+ clusters, one management and a few workload clusters. For development you can use tools like [minikube](https://minikube.sigs.k8s.io/) or [kind](https://kind.sigs.k8s.io/) to create clusters on your local machine.

### To Deploy on the cluster
**Build and push your image to the location specified by `IMG`:**

```sh
make docker-build docker-push IMG=<some-registry>/<image-name>:<tag>
```

**NOTE:** This image ought to be published in the personal registry you specified.
And it is required to have access to pull the image from the working environment.
Make sure you have the proper permission to the registry if the above commands don’t work.

**Install the CRDs into the cluster:**

```sh
make install
```

**Deploy the Manager to the cluster with the image specified by `IMG`:**

```sh
make deploy IMG=<some-registry>/<image-name>:<tag>
```

> **NOTE**: If you encounter RBAC errors, you may need to grant yourself cluster-admin
privileges or be logged in as admin.

**Create instances of your solution**
You can apply the samples (examples) from the config/sample:

```sh
kubectl apply -k config/samples/
```

>**NOTE**: Ensure that the samples has default values to test it out.

### To Uninstall
**Delete the instances (CRs) from the cluster:**

```sh
kubectl delete -k config/samples/
```

**Delete the APIs(CRDs) from the cluster:**

```sh
make uninstall
```

**UnDeploy the controller from the cluster:**

```sh
make undeploy
```

## Contributing
If you're interested to contribute to this project, please create a PR. Or, if you find a bug or require a new feature, please create an issue.

## License

Copyright 2024.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.

