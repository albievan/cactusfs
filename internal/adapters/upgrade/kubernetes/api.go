package kubernetes

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/albievan/cactusfs/internal/domain"
)

// The files of the token directory: what the chart's projected volume holds.
const (
	tokenFile     = "token"
	caFile        = "ca.crt"
	namespaceFile = "namespace"
)

// The bounds of what is read and how long a request may take.
const (
	// maxBody is the most of an answer of the API that is read: a
	// StatefulSet or a pod is a few tens of KiB.
	maxBody = 1 << 20
	// maxFile is the most of a file of the token directory that is read.
	maxFile = 256 << 10
	// requestTimeout bounds one request to the API.
	requestTimeout = 30 * time.Second
)

// The content types of a patch.
const (
	mergePatch     = "application/merge-patch+json"
	strategicPatch = "application/strategic-merge-patch+json"
)

// dnsName is a name the API accepts for a StatefulSet, a pod or a
// namespace (a DNS-1123 subdomain): what is put into a path is only ever
// one of these, so that nothing can be made of it but that path.
var dnsName = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

// validName reports whether name may be put into a path of the API.
func validName(name string) bool { return len(name) <= 253 && dnsName.MatchString(name) }

// APIBaseFromEnv returns the address of the in-cluster API from the
// variables Kubernetes gives every container, KUBERNETES_SERVICE_HOST and
// KUBERNETES_SERVICE_PORT, read through lookup (os.LookupEnv); "" when
// either is absent or empty.
func APIBaseFromEnv(lookup func(string) (string, bool)) string {
	host, ok := lookup("KUBERNETES_SERVICE_HOST")
	if !ok || host == "" {
		return ""
	}
	port, ok := lookup("KUBERNETES_SERVICE_PORT")
	if !ok || port == "" {
		return ""
	}
	return "https://" + net.JoinHostPort(host, port)
}

// checkAPIBase refuses an address of the API that is not https, or that
// carries more than a scheme, a host and a port.
func checkAPIBase(base string) error {
	u, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("the address of the Kubernetes API does not parse: %w", err)
	}
	if u.Scheme != "https" || u.Host == "" || u.User != nil || (u.Path != "" && u.Path != "/") || u.RawQuery != "" || u.Fragment != "" {
		return errors.New("the address of the Kubernetes API is https://<host>:<port> and nothing more")
	}
	return nil
}

// StatusError is an answer of the API that is not a success. It names the
// operation, the status and the reason the API gave (Conflict,
// TooManyRequests), never the message or the body.
type StatusError struct {
	// Op is what was asked: "get statefulset store-meta-0".
	Op string
	// Status is the HTTP status.
	Status int
	// Reason is the reason of the API's Status object; empty when there
	// was none.
	Reason string
}

// Error implements error.
func (e *StatusError) Error() string {
	text := "kubernetes: " + e.Op + ": the API answered " + strconv.Itoa(e.Status)
	if e.Reason != "" {
		text += " " + e.Reason
	}
	return text
}

// statusOf returns the status of an answer that was no success; 0 for any
// other error.
func statusOf(err error) int {
	var se *StatusError
	if errors.As(err, &se) {
		return se.Status
	}
	return 0
}

// api asks the in-cluster API, with the token and CA of the token
// directory, read again for every request: the kubelet replaces the token
// before it expires, and the CA may be rotated.
type api struct {
	base     string
	tokenDir string
	fixed    *http.Client

	// mu guards ca and client, the CA last read and the client made for
	// it.
	mu     sync.Mutex
	ca     []byte
	client *http.Client
}

// readFile reads a file of the token directory, bounded.
func (a *api) readFile(name string) ([]byte, error) {
	// Opened inside the directory: the kubelet's symbolic links (token ->
	// ..data/token) stay in it, and nothing outside it is read.
	root, err := os.OpenRoot(a.tokenDir)
	if err != nil {
		return nil, fmt.Errorf("kubernetes: %w", err)
	}
	defer func() { _ = root.Close() }()
	f, err := root.Open(name)
	if err != nil {
		return nil, fmt.Errorf("kubernetes: %w", err)
	}
	defer func() { _ = f.Close() }()
	data, err := io.ReadAll(io.LimitReader(f, maxFile+1))
	if err != nil {
		return nil, fmt.Errorf("kubernetes: reading %s: %w", name, err)
	}
	if len(data) > maxFile {
		return nil, fmt.Errorf("kubernetes: %s is larger than %d bytes", name, maxFile)
	}
	return data, nil
}

// namespace returns the namespace the token directory names.
func (a *api) namespace() (string, error) {
	data, err := a.readFile(namespaceFile)
	if err != nil {
		return "", err
	}
	ns := strings.TrimSpace(string(data))
	if len(ns) > 63 || !validName(ns) {
		return "", errors.New("kubernetes: the namespace of the token directory is not a namespace's name")
	}
	return ns, nil
}

// token returns the token, read now.
func (a *api) token() (string, error) {
	data, err := a.readFile(tokenFile)
	if err != nil {
		return "", err
	}
	token := strings.TrimSpace(string(data))
	if token == "" || strings.ContainsAny(token, " \t\r\n") {
		return "", errors.New("kubernetes: the token file holds no token")
	}
	return token, nil
}

// httpClient returns the client of the CA of the token directory: TLS 1.2
// or later, that CA and no other root, no proxy whatever the environment
// says. The client given in Config, when there is one, instead.
func (a *api) httpClient() (*http.Client, error) {
	if a.fixed != nil {
		return a.fixed, nil
	}
	ca, err := a.readFile(caFile)
	if err != nil {
		return nil, err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.client != nil && bytes.Equal(ca, a.ca) {
		return a.client, nil
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(ca) {
		return nil, errors.New("kubernetes: ca.crt of the token directory holds no certificate")
	}
	transport := &http.Transport{
		Proxy:                 nil,
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: pool},
		ForceAttemptHTTP2:     true,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: requestTimeout,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   2,
	}
	a.ca, a.client = ca, &http.Client{Transport: transport, Timeout: requestTimeout}
	return a.client, nil
}

// do sends one request and decodes a successful answer into out, when out
// is not nil. An answer that is no success is a *StatusError.
func (a *api) do(ctx context.Context, op, method, path, contentType string, body []byte, out any) error {
	client, err := a.httpClient()
	if err != nil {
		return err
	}
	token, err := a.token()
	if err != nil {
		return err
	}
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, strings.TrimSuffix(a.base, "/")+path, reader)
	if err != nil {
		return fmt.Errorf("kubernetes: %s: %w", op, err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := client.Do(req)
	if err != nil {
		// The error of the transport names the address, never the token.
		return fmt.Errorf("kubernetes: %s: %w: %w", op, domain.ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
	if err != nil {
		return fmt.Errorf("kubernetes: %s: %w: %w", op, domain.ErrUnavailable, err)
	}
	if len(data) > maxBody {
		return fmt.Errorf("kubernetes: %s: the answer is larger than %d bytes", op, maxBody)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		var status struct {
			Reason string `json:"reason"`
		}
		_ = json.Unmarshal(data, &status)
		reason := status.Reason
		if len(reason) > 64 || strings.ContainsFunc(reason, func(r rune) bool { return (r < 'A' || r > 'Z') && (r < 'a' || r > 'z') }) {
			reason = ""
		}
		return &StatusError{Op: op, Status: resp.StatusCode, Reason: reason}
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("kubernetes: %s: the answer does not parse: %w", op, err)
	}
	return nil
}

// objectMeta is what is read of an object's metadata.
type objectMeta struct {
	Name              string            `json:"name"`
	ResourceVersion   string            `json:"resourceVersion"`
	Annotations       map[string]string `json:"annotations"`
	Labels            map[string]string `json:"labels"`
	DeletionTimestamp *string           `json:"deletionTimestamp"`
}

// container is what is read of a container.
type container struct {
	Name  string `json:"name"`
	Image string `json:"image"`
}

// podSpec is what is read of a pod's spec.
type podSpec struct {
	Containers []container `json:"containers"`
}

// statefulSet is what is read of a StatefulSet.
type statefulSet struct {
	Metadata objectMeta `json:"metadata"`
	Spec     struct {
		Template struct {
			Metadata objectMeta `json:"metadata"`
			Spec     podSpec    `json:"spec"`
		} `json:"template"`
	} `json:"spec"`
}

// pod is what is read of a pod.
type pod struct {
	Metadata objectMeta `json:"metadata"`
	Spec     podSpec    `json:"spec"`
	Status   struct {
		Conditions []struct {
			Type   string `json:"type"`
			Status string `json:"status"`
		} `json:"conditions"`
		ContainerStatuses []struct {
			Name  string `json:"name"`
			Ready bool   `json:"ready"`
		} `json:"containerStatuses"`
	} `json:"status"`
}

// image returns the image of the named container; "" when there is none.
func (s podSpec) image(name string) string {
	for _, c := range s.Containers {
		if c.Name == name {
			return c.Image
		}
	}
	return ""
}

// readyWith reports whether the pod runs image in the named container, is
// not being deleted, and is Ready with that container ready.
func (p *pod) readyWith(name, image string) bool {
	if p == nil || p.Metadata.DeletionTimestamp != nil || p.Spec.image(name) != image {
		return false
	}
	ready := false
	for _, c := range p.Status.Conditions {
		if c.Type == "Ready" {
			ready = c.Status == "True"
		}
	}
	if !ready {
		return false
	}
	for _, c := range p.Status.ContainerStatuses {
		if c.Name == name {
			return c.Ready
		}
	}
	return false
}

// statefulSetPath returns the path of a StatefulSet.
func statefulSetPath(ns, name string) string {
	return "/apis/apps/v1/namespaces/" + ns + "/statefulsets/" + name
}

// podPath returns the path of a pod.
func podPath(ns, name string) string { return "/api/v1/namespaces/" + ns + "/pods/" + name }

// getStatefulSet reads a StatefulSet.
func (a *api) getStatefulSet(ctx context.Context, ns, name string) (*statefulSet, error) {
	var out statefulSet
	if err := a.do(ctx, "get statefulset "+name, http.MethodGet, statefulSetPath(ns, name), "", nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// getPod reads a pod; nil, nil when there is none.
func (a *api) getPod(ctx context.Context, ns, name string) (*pod, error) {
	var out pod
	err := a.do(ctx, "get pod "+name, http.MethodGet, podPath(ns, name), "", nil, &out)
	if statusOf(err) == http.StatusNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &out, nil
}

// patchStatefulSet patches a StatefulSet with a patch of the content type.
func (a *api) patchStatefulSet(ctx context.Context, ns, name, contentType string, patch any) error {
	body, err := json.Marshal(patch)
	if err != nil {
		return fmt.Errorf("kubernetes: patch statefulset %s: %w", name, err)
	}
	return a.do(ctx, "patch statefulset "+name, http.MethodPatch, statefulSetPath(ns, name), contentType, body, nil)
}

// evict asks the API to evict a pod (policy/v1 Eviction): the
// PodDisruptionBudget decides, and a 429 says not now. A pod that is not
// there is no failure.
func (a *api) evict(ctx context.Context, ns, name string) error {
	body, err := json.Marshal(map[string]any{
		"apiVersion": "policy/v1",
		"kind":       "Eviction",
		"metadata":   map[string]string{"name": name, "namespace": ns},
	})
	if err != nil {
		return fmt.Errorf("kubernetes: evict pod %s: %w", name, err)
	}
	err = a.do(ctx, "evict pod "+name, http.MethodPost, podPath(ns, name)+"/eviction", "application/json", body, nil)
	if statusOf(err) == http.StatusNotFound {
		return nil
	}
	return err
}
