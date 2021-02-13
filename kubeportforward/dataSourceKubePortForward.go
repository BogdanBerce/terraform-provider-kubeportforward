// Package kubeportforward represents the implementation of the provider
// Note:    most of this is from https://github.com/seuf/terraform-provider-kubeportforward/blob/master/data_source_kubeportforward.go
// License: https://github.com/seuf/terraform-provider-kubeportforward/blob/master/LICENSE
package kubeportforward

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"syscall"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
	"github.com/papertrail/go-tail/follower"
)

func dataSourceKubePortForward() *schema.Resource {
	return &schema.Resource{
		ReadContext: dataSourceKubePortForwardRead,
		Schema: map[string]*schema.Schema{
			"namespace": {
				Type:        schema.TypeString,
				Required:    true,
				Description: "Namespace where the service resides.",
			},
			"service": {
				Type:        schema.TypeString,
				Required:    true,
				Description: "Service to forward.",
			},
			"port": {
				Type:        schema.TypeString,
				Required:    true,
				Description: "The remote port.",
			},
			"timeout": {
				Type:        schema.TypeInt,
				Optional:    true,
				Default:     180,
				Description: "The timeout in seconds after which to stop forwarding",
			},
			"local_port": {
				Type:        schema.TypeString,
				Computed:    true,
				Description: "The local port. This will be the output of the data source.",
			},
			"forwarded": {
				Type:        schema.TypeBool,
				Computed:    true,
				Description: "The port is already forwarded.",
			},
		},
	}
}

func dataSourceKubePortForwardRead(ctx context.Context, d *schema.ResourceData, meta interface{}) diag.Diagnostics {
	namespace := d.Get("namespace").(string)
	serviceName := d.Get("service").(string)
	servicePort := d.Get("port").(string)
	timeout := d.Get("timeout").(int)
	localPort := d.Get("local_port").(string)
	forwarded := d.Get("forwarded").(bool)

	log.Printf("[DEBUG] namespace: %v\n", namespace)
	log.Printf("[DEBUG] service: %v\n", serviceName)
	log.Printf("[DEBUG] servicePort: %v\n", servicePort)
	log.Printf("[DEBUG] timeout: %v\n", timeout)

	f, err := ioutil.TempFile("", "kubeportforward_*.log")
	if err != nil {
		return diag.Errorf("cannot create log file: %v", err.Error())
	}
	output := f.Name()
	f.Close()

	cfg := meta.(KubeConfig)
	args := cfg.Args()
	args = append(args, []string{
		"--namespace", namespace,
		"--service", serviceName,
		"--port", servicePort,
		"--timeout", fmt.Sprintf("%d", timeout),
		"--output", output,
	}...)

	if forwarded == true {
		return diag.Errorf("port already forwarded")
	}

	ex, err := os.Executable()
	if err != nil {
		return diag.Errorf("unable to find executable: %s", err.Error())
	}

	port := make(chan string, 1)

	// create a random file name
	// buffer := make([]byte, 6)
	// if _, err := rand.Read(b); err != nil {
	// 	return diag.Errorf("unable to create random file name: %v", err.Error())
	// }
	// filepath.Join(os.TempDir(), fmt.Sprintf("kubeportforward-%s.log", hex.EncodeToString(buffer))))
	// open the file while also truncating it to ensure that it exists before we start the child
	file, err := os.OpenFile(output, os.O_RDWR|os.O_TRUNC|os.O_CREATE, 0666)
	if err != nil {
		return diag.Errorf("unable to open output file %s: %v", err.Error())
	}
	defer file.Close()

	// create a new tail for the file we've just created
	tail, err := follower.New(output, follower.Config{
		Whence: io.SeekEnd,
		Offset: 0,
		Reopen: true,
	})
	go func() {
		defer tail.Close()
		matcher := regexp.MustCompile(`.*Local\s+Port\s+For\s+Remote\s+Service:\s+(?P<port>\d+).*`)
		for line := range tail.Lines() {
			log.Printf("Child Process: %s\n", line.String())
			matches := matcher.FindStringSubmatch(line.String())
			if len(matches) > 0 {
				port <- matches[1]
			}
		}
	}()

	// run the command
	cmd := exec.Command(ex, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{}
	cmd.Dir = filepath.Dir(ex)
	cmd.Env = os.Environ()
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Setsid: true,
	}

	log.Printf("[DEBUG] Starting process %s with args %v\n", ex, args)
	if err = cmd.Start(); err != nil {
		return diag.Errorf("unable to start child process: %s", err.Error())
	}

	// release the process such that it continues running after the parent has completed
	if err := cmd.Process.Release(); err != nil {
		return diag.Errorf("unable to release child process: %s", err.Error())
	}

	// wait for the process to give us a local port
	select {
	case temp := <-port:
		localPort = temp
	case <-time.After(15 * time.Second):
		return diag.Errorf("did not get a local port from the child process")
	}

	d.Set("forwarded", true)
	d.Set("local_port", localPort)
	d.SetId(fmt.Sprintf("localhost-%s/%s-%s-%s", localPort, serviceName, namespace, servicePort))
	log.Printf("Forwarded localhost:%s to %s.%s:%s\n", localPort, serviceName, namespace, servicePort)

	return nil
}
