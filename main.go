package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hashicorp/terraform-plugin-sdk/v2/plugin"
	cli "github.com/jawher/mow.cli"

	"github.com/BogdanBerce/terraform-provider-kubeportforward/kubeportforward"
)

func main() {
	// if we have the "--host" argument specified, asume we're running the child process
	childMode := false
	for _, arg := range os.Args {
		if arg == "--host" {
			childMode = true
			break
		}
	}

	if childMode == true {
		runChild()
	} else {
		runParent()
	}
}

// runParent runs the parent process that represents the actual Terraform provider.
func runParent() {
	plugin.Serve(
		&plugin.ServeOpts{
			ProviderFunc: kubeportforward.Provider,
		},
	)
}

// runChild runs a child process that
func runChild() {
	app := cli.App("kubeportforward", "Port forwarder for kubernetes")

	opts := &kubeportforward.ForwarderOptions{}
	namespace := app.Cmd.StringOpt("namespace", "", "The namespace to connect to.")
	service := app.Cmd.StringOpt("service", "", "The service to connect to.")
	port := app.Cmd.StringOpt("port", "", "The port to connect to.")
	timeout := app.Cmd.IntOpt("timeout", 180, "The timeout after which to stop forwarding.")
	output := app.Cmd.StringOpt("output", "", "Output represents the file path where the current process's stdout will go.")

	opts.Host = app.Cmd.StringOpt("host", "", "The hostname (in form of URI) of Kubernetes master.")
	opts.Username = app.Cmd.StringOpt("username", "", "The username to use for HTTP basic authentication when accessing the Kubernetes master endpoint.")
	opts.Password = app.Cmd.StringOpt("password", "", "The password to use for HTTP basic authentication when accessing the Kubernetes master endpoint.")
	opts.Insecure = app.Cmd.BoolOpt("insecure", false, "Whether server should be accessed without verifying the TLS certificate.")
	opts.ClientCertificate = app.Cmd.StringOpt("client-certificate", "", "PEM-encoded client certificate for TLS authentication.")
	opts.ClientKey = app.Cmd.StringOpt("client-key", "", "PEM-encoded client certificate key for TLS authentication.")
	opts.ClusterCACertificate = app.Cmd.StringOpt("cluster-ca-certificate", "", "PEM-encoded root certificates bundle for TLS authentication.")
	opts.ConfigPaths = app.Cmd.StringsOpt("config-path", []string{}, "A list of paths to kube config files.")
	opts.ConfigContext = app.Cmd.StringOpt("config-context", "", "")
	opts.ConfigContextAuthInfo = app.Cmd.StringOpt("config-context-auth-info", "", "")
	opts.ConfigContextCluster = app.Cmd.StringOpt("config-context-cluster", "", "")
	opts.Token = app.Cmd.StringOpt("token", "", "Token to authenticate an service account.")
	opts.ExecAPIVersion = app.Cmd.StringOpt("exec-api-version", "", "")
	opts.ExecCommand = app.Cmd.StringOpt("exec-command", "", "")
	opts.ExecArgs = app.Cmd.StringsOpt("exec-arg", []string{}, "")
	opts.ExecEnv = app.Cmd.StringsOpt("exec-env", []string{}, "")

	app.Cmd.Action = func() {
		read, err := os.OpenFile("/dev/null", os.O_RDWR, 0)
		if err != nil {
			log.Fatalf("Failed to open /dev/null: %s\n", err)
		}
		write, err := os.OpenFile(*output, os.O_RDWR, 0666)
		if err != nil {
			log.Fatalf("Failed to open %s: %s\n", *output, err)
		}
		syscall.Dup2(int(read.Fd()), int(os.Stdin.Fd()))
		syscall.Dup2(int(write.Fd()), int(os.Stdout.Fd()))
		syscall.Dup2(int(write.Fd()), int(os.Stderr.Fd()))

		log.Printf("Starting child process forwarder for %s.%s:%s", *service, *namespace, *port)

		errs := make(chan error, 1)
		defer close(errs)

		forwarder, err := kubeportforward.NewForwarder(opts)
		if err != nil {
			log.Fatalf("Unable to create forwarder: %s", err.Error())
		}
		ctx, cancel := context.WithCancel(context.Background())
		port, err := forwarder.Forward(ctx, *namespace, *service, *port)
		if err != nil {
			log.Fatalf("Unable to forward port: %s", err.Error())
		}

		// print the output
		log.Printf("Local Port For Remote Service: %d\n", port)

		timeoutDuration := time.Duration(*timeout) * time.Second
		log.Printf("Forwarding time limit: %v\n", timeoutDuration)
		go func() {
			then := time.Now()
			for {
				select {
				case <-ctx.Done():
					return
				case <-time.After(10 * time.Second):
					remaining := timeoutDuration.Seconds() - time.Now().Sub(then).Seconds()
					log.Printf("Forwarding time remaining: %v\n", time.Duration(remaining)*time.Second)
				}
			}
		}()

		// listen to various signals to close the forwarder
		signals := make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGKILL)
		defer signal.Stop(signals)

		select {
		case s := <-signals:
			log.Printf("Received signal %v\n", s.String())
		case <-time.After(timeoutDuration):
		}

		cancel()
		os.Exit(0)
	}

	app.Run(os.Args)
}
