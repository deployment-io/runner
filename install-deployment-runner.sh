#!/bin/bash

installRunner (){
    install_runner_arm() {
      OS_NAME="$(uname -s)"
      if [ "$OS_NAME" = "Linux" ]; then
        curl --silent --location "https://github.com/deployment-io/runner/releases/latest/download/deployment-runner_linux_arm64.tar.gz" | tar xz -C /tmp
      elif [ "$OS_NAME" = "Darwin" ]; then
        curl --silent --location "https://github.com/deployment-io/runner/releases/latest/download/deployment-runner_darwin_arm64.tar.gz" | tar xz -C /tmp
      else
        echo "Unsupported OS: $OS_NAME"
        return
      fi
      sudo mkdir -p /usr/local/bin && sudo mv /tmp/deployment-runner /usr/local/bin/deployment-runner
    }

    install_runner_amd() {
        OS_NAME="$(uname -s)"
        if [ "$OS_NAME" = "Linux" ]; then
          curl --silent --location "https://github.com/deployment-io/runner/releases/latest/download/deployment-runner_linux_amd64.tar.gz" | tar xz -C /tmp
        elif [ "$OS_NAME" = "Darwin" ]; then
          curl --silent --location "https://github.com/deployment-io/runner/releases/latest/download/deployment-runner_darwin_amd64.tar.gz" | tar xz -C /tmp
        else
          echo "Unsupported OS: $OS_NAME"
          return
        fi
        sudo mkdir -p /usr/local/bin && sudo mv /tmp/deployment-runner /usr/local/bin/deployment-runner
    }

    ARCH=$(uname -m)
    if [ "$ARCH" = "x86_64" ]; then
      install_runner_amd
    elif [ "$ARCH" = "aarch64" ]; then
      install_runner_arm
    elif [ "$ARCH" = "arm64" ]; then
      install_runner_arm
    else
      echo "Unsupported architecture: $ARCH"
      return
    fi
}

installRunner

if command -v deployment-runner &> /dev/null; then
    rm install-deployment-runner.sh
fi