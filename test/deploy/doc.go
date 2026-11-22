// Package deploy tests deploy/deploy.sh, the deployment script, without a
// container engine, a cluster or a network: the real script runs under dash
// and bash against recording fakes of docker, podman, nerdctl, kubectl, ssh,
// k3s, curl, helm and git, which write what they were asked - every argument
// and a hash of what they read - to a transcript, and answer as each test
// tells them.
//
// The fakes are this test binary itself, found on PATH under those names
// (TestMain tells them apart by the name they were started with). Each test
// runs a copy of deploy/ in a directory of its own, with a PATH of the fakes
// and the few programs the script uses, and an environment that holds
// nothing else.
//
// The tests are compiled only with -tags=deployscript and are run by
// "make deploy-check", which is part of "make check". With the tag
// deployrender as well, the Kubernetes profiles of testdata/k8s are rendered
// with a real helm and judged by kubeconform, which the builder image has.
package deploy
