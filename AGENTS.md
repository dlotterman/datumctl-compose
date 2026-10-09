# AGENTS.md

## `datumctl`

We run `datumctl` from a container.

The container can be found here: https://hub.docker.com/r/dlotterman/datumctl-container

There is a datumctl container locally that is already logged in, you can use it like:

`podman run -it --rm --network host -v datumctl-config:/root/.datumctl:z docker.io/dlotterman/datumctl-container:latest get dnszones`

More `datumctl` documentation here: 

https://www.datum.net/docs/datumctl/overview
https://www.datum.net/docs/agents/llms-txt

## podman

Try to do as much work inside of `rootless` podman containers as you can. If you need to build tooling, you can do anything you want inside rootless podman containers.

When running on a system using SELinux (likely on Fedora, Red Hat Linux, Alma Linux and Rocky Linux, or any Linux with the `dnf` package maanger, use `:z` at the end of volumes to re-label the mount for correct permissions.

When running podman, always try to use "rootless" first, that is podman run by the user and not "rootful" podman.

When running podman rootless, look at options like `--userns=keep-id`, `--group-add keep-groups` and other flags from: https://github.com/podman-container-tools/podman/blob/main/docs/tutorials/rootless_tutorial.md

