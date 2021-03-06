/*
Copyright 2020 Mirantis, Inc.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package cmd

import (
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/k0sproject/k0s/pkg/build"
	"github.com/k0sproject/k0s/pkg/telemetry"

	"github.com/avast/retry-go"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli/v2"

	"github.com/k0sproject/k0s/pkg/applier"
	"github.com/k0sproject/k0s/pkg/certificate"
	"github.com/k0sproject/k0s/pkg/component"
	"github.com/k0sproject/k0s/pkg/component/server"
	"github.com/k0sproject/k0s/pkg/component/worker"
	"github.com/k0sproject/k0s/pkg/constant"
	"github.com/k0sproject/k0s/pkg/performance"
	"github.com/k0sproject/k0s/pkg/util"

	"github.com/k0sproject/k0s/pkg/apis/v1beta1"
	config "github.com/k0sproject/k0s/pkg/apis/v1beta1"
)

// ServerCommand ...
func ServerCommand() *cli.Command {
	return &cli.Command{
		Name:   "server",
		Usage:  "Run server",
		Action: startServer,
		Flags: []cli.Flag{
			&cli.StringFlag{
				Name:    "config",
				Aliases: []string{"c"},
				Value:   "k0s.yaml",
			},
			&cli.BoolFlag{
				Name:  "enable-worker",
				Value: false,
			},
			&cli.StringFlag{
				Name:  "profile",
				Value: "default",
				Usage: "worker profile to use on the node",
			},
		},
		ArgsUsage: "[join-token]",
	}
}

func configFromCmdFlag(ctx *cli.Context) (*config.ClusterConfig, error) {
	clusterConfig := ConfigFromYaml(ctx)

	errors := clusterConfig.Validate()
	if len(errors) > 0 {
		messages := make([]string, len(errors))
		for _, e := range errors {
			messages = append(messages, e.Error())
		}
		return nil, fmt.Errorf("config yaml does not pass validation, following errors found:%s", strings.Join(messages, "\n"))
	}

	return clusterConfig, nil
}

func startServer(ctx *cli.Context) error {
	perfTimer := performance.NewTimer("server-start").Buffer().Start()
	clusterConfig, err := configFromCmdFlag(ctx)
	if err != nil {
		return err
	}

	// create directories early with the proper permissions
	if err = util.InitDirectory(constant.DataDir, constant.DataDirMode); err != nil {
		return err
	}
	if err := util.InitDirectory(constant.CertRootDir, constant.CertRootDirMode); err != nil {
		return err
	}

	componentManager := component.NewManager()
	certificateManager := certificate.Manager{}

	var join = false
	var joinClient *v1beta1.JoinClient
	token := ctx.Args().First()
	if token != "" {
		join = true
		joinClient, err = v1beta1.JoinClientFromToken(token)
		if err != nil {
			return errors.Wrapf(err, "failed to create join client")
		}

		componentManager.AddSync(&server.CASyncer{
			JoinClient: joinClient,
		})
	}
	componentManager.AddSync(&server.Certificates{
		ClusterSpec: clusterConfig.Spec,
		CertManager: certificateManager,
	})

	logrus.Infof("using public address: %s", clusterConfig.Spec.API.Address)
	logrus.Infof("using sans: %s", clusterConfig.Spec.API.SANs)
	dnsAddress, err := clusterConfig.Spec.Network.DNSAddress()
	if err != nil {
		return err
	}
	logrus.Infof("DNS address: %s", dnsAddress)
	var storageBackend component.Component

	switch clusterConfig.Spec.Storage.Type {
	case v1beta1.KineStorageType, "":
		storageBackend = &server.Kine{
			Config: clusterConfig.Spec.Storage.Kine,
		}
	case v1beta1.EtcdStorageType:
		storageBackend = &server.Etcd{
			Config:      clusterConfig.Spec.Storage.Etcd,
			Join:        join,
			CertManager: certificateManager,
			JoinClient:  joinClient,
		}
	default:
		return errors.New(fmt.Sprintf("Invalid storage type: %s", clusterConfig.Spec.Storage.Type))
	}
	logrus.Infof("Using storage backend %s", clusterConfig.Spec.Storage.Type)
	componentManager.Add(storageBackend)

	componentManager.Add(&server.APIServer{
		Storage:       storageBackend,
		ClusterConfig: clusterConfig,
	})
	componentManager.Add(&server.Konnectivity{
		ClusterConfig: clusterConfig,
	})
	componentManager.Add(&server.Scheduler{
		ClusterConfig: clusterConfig,
	})
	componentManager.Add(&server.ControllerManager{
		ClusterConfig: clusterConfig,
	})
	componentManager.Add(&applier.Manager{})
	componentManager.Add(&server.K0SControlAPI{
		ConfigPath: ctx.String("config"),
	})

	if clusterConfig.Telemetry.Enabled {
		componentManager.Add(&telemetry.Component{
			ClusterConfig: clusterConfig,
			Version:       build.Version,
		})
	}

	perfTimer.Checkpoint("starting-component-init")
	// init components
	if err := componentManager.Init(); err != nil {
		return err
	}
	perfTimer.Checkpoint("finished-component-init")

	// Set up signal handling. Use buffered channel so we dont miss
	// signals during startup
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt, syscall.SIGINT, syscall.SIGTERM)

	perfTimer.Checkpoint("starting-components")
	// Start components
	err = componentManager.Start()
	perfTimer.Checkpoint("finished-starting-components")
	if err != nil {
		logrus.Errorf("failed to start server components: %s", err)
		c <- syscall.SIGTERM
	}

	perfTimer.Checkpoint("starting-reconcilers")
	// in-cluster component reconcilers
	reconcilers := createClusterReconcilers(clusterConfig)
	if err == nil {
		// Start all reconcilers
		for _, reconciler := range reconcilers {
			if err := reconciler.Run(); err != nil {
				logrus.Errorf("failed to start reconciler: %s", err.Error())
			}
		}
	}
	perfTimer.Checkpoint("started-reconcilers")

	if err == nil && ctx.Bool("enable-worker") {
		perfTimer.Checkpoint("starting-worker")
		err = enableServerWorker(clusterConfig, componentManager, ctx.String("profile"))
		if err != nil {
			logrus.Errorf("failed to start worker components: %s", err)
			if err := componentManager.Stop(); err != nil {
				logrus.Errorf("componentManager.Stop: %s", err)
			}
			return err
		}
		perfTimer.Checkpoint("started-worker")
	}

	perfTimer.Output()

	// Wait for k0s process termination
	<-c
	logrus.Info("Shutting down k0s server")

	// Stop all reconcilers first
	for _, reconciler := range reconcilers {
		if err := reconciler.Stop(); err != nil {
			logrus.Warningf("failed to stop reconciler: %s", err.Error())
		}
	}

	// Stop components
	if err := componentManager.Stop(); err != nil {
		logrus.Errorf("error while stoping component manager %s", err)
	}
	return nil
}

func createClusterReconcilers(clusterConf *config.ClusterConfig) map[string]component.Component {
	reconcilers := make(map[string]component.Component)
	clusterSpec := clusterConf.Spec

	defaultPSP, err := server.NewDefaultPSP(clusterSpec)
	if err != nil {
		logrus.Warnf("failed to initialize default PSP reconciler: %s", err.Error())
	} else {
		reconcilers["default-psp"] = defaultPSP
	}

	proxy, err := server.NewKubeProxy(clusterConf)
	if err != nil {
		logrus.Warnf("failed to initialize kube-proxy reconciler: %s", err.Error())
	} else {
		reconcilers["kube-proxy"] = proxy
	}

	coreDNS, err := server.NewCoreDNS(clusterConf)
	if err != nil {
		logrus.Warnf("failed to initialize CoreDNS reconciler: %s", err.Error())
	} else {
		reconcilers["coredns"] = coreDNS
	}

	initNetwork(reconcilers, clusterConf)

	metricServer, err := server.NewMetricServer(clusterConf)
	if err != nil {
		logrus.Warnf("failed to initialize metric server reconciler: %s", err.Error())
	} else {
		reconcilers["metricServer"] = metricServer
	}

	kubeletConfig, err := server.NewKubeletConfig(clusterSpec)
	if err != nil {
		logrus.Warnf("failed to initialize kubelet config reconciler: %s", err.Error())
	} else {
		reconcilers["kubeletConfig"] = kubeletConfig
	}

	systemRBAC, err := server.NewSystemRBAC(clusterSpec)
	if err != nil {
		logrus.Warnf("failed to initialize system RBAC reconciler: %s", err.Error())
	} else {
		reconcilers["systemRBAC"] = systemRBAC
	}

	return reconcilers
}

func initNetwork(reconcilers map[string]component.Component, conf *config.ClusterConfig) {
	if conf.Spec.Network.Provider != "calico" {
		logrus.Warnf("network provider set to custom, k0s will not manage it")
		return
	}

	manifestsSaver, err := server.NewManifestsSaver()
	if err != nil {
		logrus.Warnf("failed to initialize calico reconciler manifests saver: %s", err.Error())
		return
	}

	calico, err := server.NewCalico(conf, manifestsSaver)

	if err != nil {
		logrus.Warnf("failed to initialize calico reconciler: %s", err.Error())
		return
	}

	reconcilers["calico"] = calico

}

func enableServerWorker(clusterConfig *config.ClusterConfig, componentManager *component.Manager, profile string) error {
	if !util.FileExists(constant.KubeletAuthConfigPath) {
		// wait for server to start up
		err := retry.Do(func() error {
			if !util.FileExists(constant.AdminKubeconfigConfigPath) {
				return fmt.Errorf("file does not exist: %s", constant.AdminKubeconfigConfigPath)
			}
			return nil
		})
		if err != nil {
			return err
		}

		var bootstrapConfig string
		err = retry.Do(func() error {
			config, err := createKubeletBootstrapConfig(clusterConfig, "worker", time.Minute)
			if err != nil {
				return err
			}
			bootstrapConfig = config

			return nil
		})

		if err != nil {
			return err
		}
		if err := handleKubeletBootstrapToken(bootstrapConfig); err != nil {
			return err
		}
	}
	worker.KernelSetup()

	kubeletConfigClient, err := loadKubeletConfigClient()
	if err != nil {
		return err
	}

	containerd := &worker.ContainerD{}
	kubelet := &worker.Kubelet{
		KubeletConfigClient: kubeletConfigClient,
		Profile:             profile,
	}

	if err := containerd.Init(); err != nil {
		logrus.Errorf("failed to init containerd: %s", err)
	}
	if err := kubelet.Init(); err != nil {
		logrus.Errorf("failed to init kubelet: %s", err)
	}
	if err := containerd.Run(); err != nil {
		logrus.Errorf("failed to run containerd: %s", err)
	}
	if err := kubelet.Run(); err != nil {
		logrus.Errorf("failed to run kubelet: %s", err)
	}

	componentManager.Add(containerd)
	componentManager.Add(kubelet)

	return nil
}
