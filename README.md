
# Deployment Runner

Runner is an application that can be installed locally or on cloud environments to automate various DevOps and infrastructure tasks.

Some of its use cases are:
1. checking out source code.
2. building and deploying code.
3. creating and managing cloud dev environments.
4. CI/CD.
5. creating previews. 
6. deleting deployments and freeing cloud resources.

## Install locally on your device

### Linux and MacOS

```console
curl -O https://raw.githubusercontent.com/deployment-io/runner/master/install-deployment-runner.sh && source install-deployment-runner.sh
```

### Windows

Download and unzip the latest binary from releases.

### Usage

```console
TargetCloud=aws UserSecret=yourUserSecret UserKey=yourUserKey deployment-runner
```

[//]: # (For more information about installing and using the runner locally, see [Installing runner locally]&#40;https://deployment.io/docs/runner-installation/local-setup/&#41;)

## Install on AWS

For more information about installing the runner on AWS, see [Installing runner on AWS](https://deployment.io/docs/runner-installation/aws-setup/)

## Design Principles

### Security and Privacy

All deployment workflows and cloud operations are executed by the runner. Runner is the only application that has access to the source code and cloud. All data communication between the runner and control plane is encrypted and happens over TLS.

### Modularity

Command design pattern is used extensively to make sure it's easy to add new tasks without modifying existing tasks. 

### Composability

Complex tasks can be created by chaining simpler tasks.

### Reusability

Simple tasks can be reused across complex tasks.

## Connect with us

As an open-source tool, it would mean the world to us if you starred this repo :-).

## Contribute

We accept contributions in the form of issues and pull requests.