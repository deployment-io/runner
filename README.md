
# Deployment Runner

Runner is the application that gets installed on cloud environments and automates various DevOps and infrastructure tasks.

Some of its responsibilities are:
1. checking out source code.
2. building and deploying code.
3. CI/CD.
4. creating previews. 
5. deleting deployments and freeing cloud resources.

## Get Started

For more information about installing the runner on AWS, see [Installing runner on AWS](https://deployment.io/docs/runner-installation/aws-setup/)

Runner CLI coming soon!

## Design Philosophy

### Security and Privacy

All data communication between the runner and control plane is encrypted and happens over TLS. All deployment workflows and cloud operations are executed by the runner. Runner is the only application that has access to the source code and cloud.

### Modularity

Command design pattern is used extensively to make sure it's easy to add new tasks without modifying existing tasks. Complex tasks can be created by chaining simpler tasks.   

## Contribute

We accept contributions in the form of issues and pull requests.