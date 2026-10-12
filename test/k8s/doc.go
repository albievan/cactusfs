// Package k8s installs the Helm chart of deploy/helm/cactusfs on a real
// Kubernetes - k3s, in one container - with the image this tree builds, and
// checks what the chart promises of a running cluster: the pods are admitted
// under the restricted Pod Security Standard and become ready, S3 answers
// through the Service, an object is read while a node is away and after a pod
// was deleted and came back, and the NetworkPolicy keeps a pod that is not of
// the release from the node ports.
//
// The tests are compiled only with -tags=k8s and are run by "make test-k8s".
// They are no part of "make check": k3s needs a privileged container, which a
// rootless engine does not give.
package k8s
