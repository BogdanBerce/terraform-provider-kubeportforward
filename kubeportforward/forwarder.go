// Package kubeportforward represents the implementation of the provider
// Note:    most of this is from https://github.com/seuf/terraform-provider-kubeportforward/blob/master/data_source_kubeportforward.go
// License: https://github.com/seuf/terraform-provider-kubeportforward/blob/master/LICENSE
package kubeportforward

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"

	"github.com/mitchellh/go-homedir"
	v1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	apimachineryschema "k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/kubernetes"
	restclient "k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/portforward"
	"k8s.io/client-go/transport/spdy"
)

// ForwarderOptions represents configuration options for this forwarder.
type ForwarderOptions struct {
	// The hostname (in form of URI) of Kubernetes master.
	Host *string
	// The username to use for HTTP basic authentication when accessing the Kubernetes master endpoint.
	Username *string
	// The password to use for HTTP basic authentication when accessing the Kubernetes master endpoint.
	Password *string
	// Whether server should be accessed without verifying the TLS certificate.
	Insecure *bool
	// PEM-encoded client certificate for TLS authentication.
	ClientCertificate *string
	// PEM-encoded client certificate key for TLS authentication.
	ClientKey *string
	// PEM-encoded root certificates bundle for TLS authentication.
	ClusterCACertificate *string
	// A list of paths to kube config files.
	ConfigPaths *[]string

	ConfigContext         *string
	ConfigContextAuthInfo *string
	ConfigContextCluster  *string

	// Token to authenticate an service account
	Token *string

	ExecAPIVersion *string
	ExecCommand    *string
	ExecEnv        *[]string
	ExecArgs       *[]string
}

// Forwarder represents a forwader that can proxy a local port to a service in the cluster.
type Forwarder struct {
	opts      *ForwarderOptions
	config    *restclient.Config
	clientset *kubernetes.Clientset
	fw        *portforward.PortForwarder
}

// NewForwarder creates a new forwarder
func NewForwarder(opts *ForwarderOptions) (*Forwarder, error) {
	f := &Forwarder{
		opts: opts,
	}
	config, err := initializeRESTClientConfig(opts)
	if err != nil {
		return nil, err
	}
	clientset, err := initializeKubernetesClientset(config)
	if err != nil {
		return nil, err
	}
	f.config = config
	f.clientset = clientset
	return f, nil
}

// Forward forwards a local port to the specified namespace, service and port in the cluster.
// The return indicates the port number that we are listening on locally.
func (f *Forwarder) Forward(ctx context.Context, namespace, serviceName string, servicePort string) (int, error) {
	// get the service
	svc, err := f.clientset.CoreV1().Services(namespace).Get(ctx, serviceName, metav1.GetOptions{})
	if err != nil {
		return 0, fmt.Errorf("cannot get kubernetes service %s in namespace %s: %s", serviceName, namespace, err.Error())
	}

	// get the pods for the service
	selector := mapToSelectorStr(svc.Spec.Selector)
	if selector == "" {
		return 0, fmt.Errorf("no backing pods for service %s in %s on cluster %s", svc.Name, svc.Namespace, svc.ClusterName)
	}

	pods, err := f.clientset.CoreV1().Pods(svc.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return 0, fmt.Errorf("no pods found for %s: %s", selector, err.Error())
	}

	if len(pods.Items) == 0 {
		return 0, fmt.Errorf("no pods returned for service %s in %s on cluster %s", svc.Name, svc.Namespace, svc.ClusterName)
	}

	for _, pod := range pods.Items {
		// only consider running pods
		if pod.Status.Phase != v1.PodRunning {
			continue
		}

		// find the pod port next
		found := false
		for _, port := range svc.Spec.Ports {

			// try and find a numeric port first
			podPort := port.TargetPort.String()
			if _, err := strconv.Atoi(podPort); err != nil {
				// search a pods containers for the named port
				if namedPodPort, ok := portSearch(podPort, pod.Spec.Containers); ok == true {
					podPort = namedPodPort
				}
			}

			// match against the requested port
			if podPort == servicePort {
				found = true
				break
			}
		}

		// even though the pod was running, no matching port was found
		if found == false {
			continue
		}

		// create dialer
		host := strings.TrimPrefix(f.config.Host, "https://")
		host = strings.TrimPrefix(host, "http://")
		path := fmt.Sprintf("/api/v1/namespaces/%s/pods/%s/portforward", namespace, pod.Name)

		serverURL := &url.URL{Scheme: "https", Host: host, Path: path}
		if f.config.Insecure == true {
			serverURL.Scheme = "http"
		}

		transport, upgrader, err := spdy.RoundTripperFor(f.config)
		if err != nil {
			return 0, fmt.Errorf("cannot create roundtripper: %s", err.Error())
		}
		dialer := spdy.NewDialer(upgrader, &http.Client{Transport: transport}, "POST", serverURL)

		// create channels for flow control
		stopChannel := make(chan struct{}, 1)
		readyChannel := make(chan struct{})

		// listen on localhost and instruct the forwarder to pick a port to listen on
		listenAddresses := []string{"127.0.0.1"}
		fwdPorts := []string{fmt.Sprintf("0:%s", servicePort)}
		fw, err := portforward.NewOnAddresses(dialer, listenAddresses, fwdPorts, stopChannel, readyChannel, os.Stdout, os.Stderr)
		if err != nil {
			return 0, fmt.Errorf("cannot initialize port forwarder: %s", err.Error())
		}

		// start the forwarder and wait for it to become ready
		go fw.ForwardPorts()
		<-readyChannel

		actualFwdPorts, err := fw.GetPorts()
		if err != nil {
			return 0, fmt.Errorf("cannot get forwarded ports: %s", err.Error())
		}
		if len(actualFwdPorts) != 1 {
			return 0, fmt.Errorf("cannot get forwarded ports: unexpected length %d", len(actualFwdPorts))
		}

		f.fw = fw
		return int(actualFwdPorts[0].Local), nil
	}

	return 0, fmt.Errorf("no pod found")
}

// initializeRESTClientConfig creates a rest client for use in talking to the cluster.
func initializeRESTClientConfig(opts *ForwarderOptions) (*restclient.Config, error) {
	overrides := &clientcmd.ConfigOverrides{}
	loader := &clientcmd.ClientConfigLoadingRules{}

	configPaths := []string{}

	if opts.ConfigPaths != nil && len(*opts.ConfigPaths) > 0 {
		configPaths = append(configPaths, *opts.ConfigPaths...)
	}

	if len(configPaths) > 0 {
		expandedPaths := []string{}
		for _, p := range configPaths {
			path, err := homedir.Expand(p)
			if err != nil {
				return nil, err
			}
			if _, err := os.Stat(path); err != nil {
				return nil, fmt.Errorf("could not open kubeconfig %q: %v", p, err)
			}
			expandedPaths = append(expandedPaths, path)
		}

		if len(expandedPaths) == 1 {
			loader.ExplicitPath = expandedPaths[0]
		} else {
			loader.Precedence = expandedPaths
		}

		ctxSuffix := "; default context"

		if (opts.ConfigContext != nil && *opts.ConfigContext != "") || (opts.ConfigContextAuthInfo != nil && *opts.ConfigContextAuthInfo != "") || (opts.ConfigContextCluster != nil && *opts.ConfigContextCluster != "") {
			ctxSuffix = "; overriden context"
			if opts.ConfigContext != nil && *opts.ConfigContext != "" {
				overrides.CurrentContext = *opts.ConfigContext
				ctxSuffix += fmt.Sprintf("; config ctx: %s", overrides.CurrentContext)
			}

			overrides.Context = clientcmdapi.Context{}
			if opts.ConfigContextAuthInfo != nil && *opts.ConfigContextAuthInfo != "" {
				overrides.Context.AuthInfo = *opts.ConfigContextAuthInfo
				ctxSuffix += fmt.Sprintf("; auth_info: %s", overrides.Context.AuthInfo)
			}
			if opts.ConfigContextCluster != nil && *opts.ConfigContextCluster != "" {
				overrides.Context.Cluster = *opts.ConfigContextCluster
				ctxSuffix += fmt.Sprintf("; cluster: %s", overrides.Context.Cluster)
			}
		}
	}

	// Overriding with static configuration
	if opts.Insecure != nil {
		overrides.ClusterInfo.InsecureSkipTLSVerify = *opts.Insecure
	}
	if opts.ClusterCACertificate != nil && *opts.ClusterCACertificate != "" {
		buffer, err := base64.StdEncoding.DecodeString(*opts.ClusterCACertificate)
		if err != nil {
			return nil, err
		}
		overrides.ClusterInfo.CertificateAuthorityData = buffer
	}
	if opts.ClientCertificate != nil && *opts.ClientCertificate != "" {
		buffer, err := base64.StdEncoding.DecodeString(*opts.ClientCertificate)
		if err != nil {
			return nil, err
		}
		overrides.AuthInfo.ClientCertificateData = buffer
	}
	if opts.Host != nil && *opts.Host != "" {
		// Server has to be the complete address of the kubernetes cluster (scheme://hostname:port), not just the hostname,
		// because `overrides` are processed too late to be taken into account by `defaultServerUrlFor()`.
		// This basically replicates what defaultServerUrlFor() does with config but for overrides,
		// see https://github.com/kubernetes/client-go/blob/v12.0.0/rest/url_utils.go#L85-L87
		hasCA := len(overrides.ClusterInfo.CertificateAuthorityData) != 0
		hasCert := len(overrides.AuthInfo.ClientCertificateData) != 0
		defaultTLS := hasCA || hasCert || overrides.ClusterInfo.InsecureSkipTLSVerify
		host, _, err := restclient.DefaultServerURL(*opts.Host, "", apimachineryschema.GroupVersion{}, defaultTLS)
		if err != nil {
			return nil, fmt.Errorf("Failed to parse host: %s", err)
		}

		overrides.ClusterInfo.Server = host.String()
	}
	if opts.Username != nil {
		overrides.AuthInfo.Username = *opts.Username
	}
	if opts.Password != nil {
		overrides.AuthInfo.Password = *opts.Password
	}
	if opts.ClientKey != nil && *opts.ClientKey != "" {
		buffer, err := base64.StdEncoding.DecodeString(*opts.ClientKey)
		if err != nil {
			return nil, err
		}
		overrides.AuthInfo.ClientKeyData = buffer
	}
	if opts.Token != nil {
		overrides.AuthInfo.Token = *opts.Token
	}

	if opts.ExecCommand != nil && *opts.ExecCommand != "" {
		exec := &clientcmdapi.ExecConfig{}
		if opts.ExecAPIVersion != nil {
			exec.APIVersion = *opts.ExecAPIVersion
		}
		exec.Command = *opts.ExecCommand
		if opts.ExecArgs != nil && len(*opts.ExecArgs) > 0 {
			exec.Args = append(exec.Args, *opts.ExecArgs...)
		}
		if opts.ExecEnv != nil && len(*opts.ExecEnv) > 0 {
			for _, arg := range *opts.ExecEnv {
				parts := strings.SplitN(arg, "=", 2)
				if len(parts) != 2 {
					continue
				}
				exec.Env = append(exec.Env, clientcmdapi.ExecEnvVar{Name: parts[0], Value: parts[1]})
			}
		}
		overrides.AuthInfo.Exec = exec
	}

	cc := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loader, overrides)
	cfg, err := cc.ClientConfig()
	if err != nil {
		return nil, fmt.Errorf("invalid configuration: %s", err.Error())
	}

	return cfg, nil
}

// initializeKubernetesClientset initializes the Kubernetes clientset.
func initializeKubernetesClientset(config *restclient.Config) (*kubernetes.Clientset, error) {
	k, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, fmt.Errorf("failed to configure client: %s", err)
	}
	return k, nil
}

func mapToSelectorStr(msel map[string]string) string {
	selector := ""
	for k, v := range msel {
		if selector != "" {
			selector = selector + ","
		}
		selector = selector + fmt.Sprintf("%s=%s", k, v)
	}

	return selector
}

func portSearch(portName string, containers []v1.Container) (string, bool) {
	for _, container := range containers {
		for _, cp := range container.Ports {
			if cp.Name == portName {
				return fmt.Sprint(cp.ContainerPort), true
			}
		}
	}
	return "0", false
}
