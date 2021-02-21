// Package kubeportforward represents the implementation of the provider
// Note:    some of this is from https://github.com/hashicorp/terraform-provider-kubernetes/blob/22d2d6f3222bb5ee1ce3b170b7765be131d10eae/kubernetes/provider.go
// License: https://github.com/hashicorp/terraform-provider-kubernetes/blob/22d2d6f3222bb5ee1ce3b170b7765be131d10eae/LICENSE
package kubeportforward

import (
	"context"
	"encoding/base64"
	"fmt"
	"log"
	"os"
	"path/filepath"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"

	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

// KubeConfig gets the configured restclient.Config
type KubeConfig interface {
	Args() []string
}

// Args gets the current args that we will pass to the child process
func (c *kubeConfig) Args() []string {
	return c.args
}

type kubeConfig struct {
	args       []string
	configData *schema.ResourceData
}

// Provider returns a *schema.Provider
func Provider() *schema.Provider {
	return &schema.Provider{
		ConfigureContextFunc: configureContext,
		Schema: map[string]*schema.Schema{
			"host": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_HOST", ""),
				Description: "The hostname (in form of URI) of Kubernetes master.",
			},
			"username": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_USER", ""),
				Description: "The username to use for HTTP basic authentication when accessing the Kubernetes master endpoint.",
			},
			"password": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_PASSWORD", ""),
				Description: "The password to use for HTTP basic authentication when accessing the Kubernetes master endpoint.",
			},
			"insecure": {
				Type:        schema.TypeBool,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_INSECURE", false),
				Description: "Whether server should be accessed without verifying the TLS certificate.",
			},
			"client_certificate": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_CLIENT_CERT_DATA", ""),
				Description: "PEM-encoded client certificate for TLS authentication.",
			},
			"client_key": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_CLIENT_KEY_DATA", ""),
				Description: "PEM-encoded client certificate key for TLS authentication.",
			},
			"cluster_ca_certificate": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_CLUSTER_CA_CERT_DATA", ""),
				Description: "PEM-encoded root certificates bundle for TLS authentication.",
			},
			"config_paths": {
				Type:        schema.TypeList,
				Elem:        &schema.Schema{Type: schema.TypeString},
				Optional:    true,
				Description: "A list of paths to kube config files. Can be set with KUBE_CONFIG_PATHS environment variable.",
			},
			"config_path": {
				Type:          schema.TypeString,
				Optional:      true,
				DefaultFunc:   schema.EnvDefaultFunc("KUBE_CONFIG_PATH", nil),
				Description:   "Path to the kube config file. Can be set with KUBE_CONFIG_PATH.",
				ConflictsWith: []string{"config_paths"},
			},
			"config_context": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_CTX", ""),
			},
			"config_context_auth_info": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_CTX_AUTH_INFO", ""),
				Description: "",
			},
			"config_context_cluster": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_CTX_CLUSTER", ""),
				Description: "",
			},
			"token": {
				Type:        schema.TypeString,
				Optional:    true,
				DefaultFunc: schema.EnvDefaultFunc("KUBE_TOKEN", ""),
				Description: "Token to authenticate an service account",
			},
			"exec": {
				Type:     schema.TypeList,
				Optional: true,
				MaxItems: 1,
				Elem: &schema.Resource{
					Schema: map[string]*schema.Schema{
						"api_version": {
							Type:     schema.TypeString,
							Required: true,
						},
						"command": {
							Type:     schema.TypeString,
							Required: true,
						},
						"env": {
							Type:     schema.TypeMap,
							Optional: true,
							Elem:     &schema.Schema{Type: schema.TypeString},
						},
						"args": {
							Type:     schema.TypeList,
							Optional: true,
							Elem:     &schema.Schema{Type: schema.TypeString},
						},
					},
				},
				Description: "",
			},
		},
		ResourcesMap: map[string]*schema.Resource{},
		DataSourcesMap: map[string]*schema.Resource{
			"kubeportforward": dataSourceKubePortForward(),
		},
	}
}

func configureContext(ctx context.Context, d *schema.ResourceData) (interface{}, diag.Diagnostics) {
	args, err := initializeArguments(d)
	if err != nil {
		return nil, diag.FromErr(err)
	}
	m := &kubeConfig{
		args:       args,
		configData: d,
	}
	return m, diag.Diagnostics{}
}

func initializeArguments(d *schema.ResourceData) ([]string, error) {
	// if err := checkConfigurationValid(d); err != nil {
	// 	return nil, err
	// }

	args := []string{}
	hasConfigPath := false
	if v, ok := d.Get("config_path").(string); ok && v != "" {
		args = append(args, []string{"--config-path", v}...)
		hasConfigPath = true
	} else if v, ok := d.Get("config_paths").([]interface{}); ok && len(v) > 0 {
		for _, p := range v {
			args = append(args, []string{"--config-path", p.(string)}...)
			hasConfigPath = true
		}
	} else if v := os.Getenv("KUBE_CONFIG_PATHS"); v != "" {
		// NOTE we have to do this here because the schema
		// does not yet allow you to set a default for a TypeList
		for _, p := range filepath.SplitList(v) {
			args = append(args, []string{"--config-path", p}...)
			hasConfigPath = true
		}
	}

	if hasConfigPath == true {
		kubectx, ctxOk := d.GetOk("config_context")
		authInfo, authInfoOk := d.GetOk("config_context_auth_info")
		cluster, clusterOk := d.GetOk("config_context_cluster")
		if ctxOk || authInfoOk || clusterOk {
			if ctxOk {
				args = append(args, []string{"--config-context", kubectx.(string)}...)
			}

			if authInfoOk {
				args = append(args, []string{"--config-context-auth-info", authInfo.(string)}...)
			}
			if clusterOk {
				args = append(args, []string{"--config-context-cluster", cluster.(string)}...)
			}
		}
	}

	// Overriding with static configuration
	if v, ok := d.GetOk("insecure"); ok && v.(bool) == true {
		args = append(args, []string{"--insecure"}...)
	}
	if v, ok := d.GetOk("cluster_ca_certificate"); ok && v.(string) != "" {
		args = append(args, []string{"--cluster-ca-certificate", base64.StdEncoding.EncodeToString([]byte(v.(string)))}...)
	}
	if v, ok := d.GetOk("client_certificate"); ok && v.(string) != "" {
		args = append(args, []string{"--client-certificate", base64.StdEncoding.EncodeToString([]byte(v.(string)))}...)
	}
	if v, ok := d.GetOk("host"); ok && v.(string) != "" {
		args = append(args, []string{"--host", v.(string)}...)
	}
	if v, ok := d.GetOk("username"); ok && v.(string) != "" {
		args = append(args, []string{"--username", v.(string)}...)
	}
	if v, ok := d.GetOk("password"); ok && v.(string) != "" {
		args = append(args, []string{"--password", v.(string)}...)
	}
	if v, ok := d.GetOk("client_key"); ok && v.(string) != "" {
		args = append(args, []string{"--client-key", base64.StdEncoding.EncodeToString([]byte(v.(string)))}...)
	}
	if v, ok := d.GetOk("token"); ok && v.(string) != "" {
		args = append(args, []string{"--token", v.(string)}...)
	}

	if v, ok := d.GetOk("exec"); ok {
		if spec, ok := v.([]interface{})[0].(map[string]interface{}); ok {
			if v := spec["api_version"].(string); v != "" {
				args = append(args, []string{"--exec-api-version", v}...)
			}
			if v := spec["command"].(string); v != "" {
				args = append(args, []string{"--exec-command", v}...)
			}
			if v := expandStringSlice(spec["args"].([]interface{})); len(v) > 0 {
				for _, vv := range v {
					args = append(args, []string{"--exec-arg", vv}...)
				}
			}
			if v := spec["env"].(map[string]interface{}); len(v) > 0 {
				for kk, vv := range v {
					args = append(args, []string{"--exec-env", fmt.Sprintf("%s=%s", kk, vv)}...)
				}
			}
		} else {
			return nil, fmt.Errorf("Failed to parse exec")
		}
	}

	return args, nil
}

var apiTokenMountPath = "/var/run/secrets/kubernetes.io/serviceaccount"

// inCluster returns true if we are running inside of the cluster.
func inCluster() bool {
	host, port := os.Getenv("KUBERNETES_SERVICE_HOST"), os.Getenv("KUBERNETES_SERVICE_PORT")
	if host == "" || port == "" {
		return false
	}

	if _, err := os.Stat(apiTokenMountPath); err != nil {
		return false
	}
	return true
}

// checkConfigurationValid checks if the configuration is valid.
func checkConfigurationValid(d *schema.ResourceData) error {
	if inCluster() {
		log.Printf("[DEBUG] Terraform appears to be running inside the Kubernetes cluster")
		return nil
	}

	if os.Getenv("KUBE_CONFIG_PATHS") != "" {
		return nil
	}

	atLeastOneOf := []string{
		"host",
		"config_path",
		"config_paths",
		"client_certificate",
		"token",
		"exec",
	}
	for _, a := range atLeastOneOf {
		if _, ok := d.GetOk(a); ok {
			return nil
		}
	}

	return fmt.Errorf(`provider not configured: you must configure a path to your kubeconfig or explicitly supply credentials via the provider block or environment variables`)
}

// expandStringSlice converts the
func expandStringSlice(s []interface{}) []string {
	result := make([]string, len(s), len(s))
	for k, v := range s {
		// Handle the Terraform parser bug which turns empty strings in lists to nil.
		if v == nil {
			result[k] = ""
		} else {
			result[k] = v.(string)
		}
	}
	return result
}
