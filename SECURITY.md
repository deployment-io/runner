## Introduction
At Deployment.io, we take security and compliance very seriously. It's our mission to make sure that a client's source code, cloud, and data are secure.

Please read on to know more about our security processes and approaches.

Please email security@deployment.io for any feedback or disclosing security vulnerabilities.

## Encrypted Data - At rest and In transit
All your traffic in and out of Deployment is sent over HTTPS or TLS.

All your secrets are encrypted using industry standard AES-256 encryption.

All your data in database is encrypted on disk.

## Data Retention Policy
All data stored by deployment.io is held for as long as you desire. We are just the custodian of your data and you can request a full copy of your data at any time.

Please email data@deployment.io to request for your data.

## Access to Your Source Code and Cloud
Deployment.io doesn't have any access to your cloud. The deployment runner is installed locally or on your cloud and access to your cloud never leaves your device or cloud.

Your source code is checked out using the deployment runner in your cloud. At no time do we checkout your source code on our infrastructure.

## Data Centers
We are hosted on AWS which helps us to provide a reliable service.

## Oauth 2.0 Access Tokens
We never request for long-lived Oauth access tokens. The access tokens for GitHub, GitLab, and Slack are short-lived and are refreshed when they expire.