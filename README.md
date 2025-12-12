# vsock-unix-proxy

A simple light-weight proxy for connecting vsock ports with unix sockets and
vice-versa. Allows you to run a linux virtual machine and forward requests to
a unix socket bound inside the virtual machine. Tested with MacOS virtualization
framework and linuxkit.