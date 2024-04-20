
# Deployment Runner

Runner is an application that can be installed on cloud environments to automate various DevOps and infrastructure tasks.

Some of its responsibilities are:
1. checking out source code.
2. building and deploying code.
3. CI/CD.
4. creating previews. 
5. deleting deployments and freeing cloud resources.

## Get Started

For more information about installing the runner on AWS, see [Installing runner on AWS](https://deployment.io/docs/runner-installation/aws-setup/)

Runner CLI coming soon!

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