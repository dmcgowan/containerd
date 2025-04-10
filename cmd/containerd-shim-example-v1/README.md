# Shim Example

This is a simple example of how to create a shim, it will not run any process but
show where to add your process code.

## How to run

To try this on MacOS

```sh
$ make binaries
$ sudo bin/ctr run --runtime $(pwd)/bin/containerd-shim-example-v1 --rootfs . mycontainer
```

You can look at the containerd logs to see the shim output.

> **_NOTE:_**  containerd should already be running, you can run containerd in another
tab after `make binaries` if you have not already done so.

 > **_NOTE:_** currently this requires some manual cleanup of the shim process and container

## How to add your functionality

Implement the task functions under `task/service.go`
