// Copyright 2019 Layer5.io
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"

	"github.com/layer5io/meshery-traefik-mesh/traefik"
	"github.com/layer5io/meshery-traefik-mesh/traefik/oam"
	"github.com/layer5io/meshkit/logger"
	"github.com/layer5io/meshkit/utils/events"

	// "github.com/layer5io/meshkit/tracing"
	"github.com/layer5io/meshery-adapter-library/adapter"
	"github.com/layer5io/meshery-adapter-library/api/grpc"
	"github.com/layer5io/meshery-traefik-mesh/build"
	"github.com/layer5io/meshery-traefik-mesh/internal/config"
	configprovider "github.com/layer5io/meshkit/config/provider"
)

var (
	serviceName = "traefik-mesh-adapter"
	version     = "edge"
	gitsha      = "none"
)

func init() {
	// Create the config path if it doesn't exists as the entire adapter
	// expects that directory to exists, which may or may not be true
	if err := os.MkdirAll(path.Join(config.RootPath(), "bin"), 0750); err != nil {
		fmt.Println(err)
		os.Exit(1)
	}
}

// main is the entrypoint of the adapter
func main() {
	// Initialize Logger instance
	log, err := logger.New(serviceName, logger.Options{
		Format:     logger.SyslogLogFormat,
		DebugLevel: isDebug(),
	})
	if err != nil {
		fmt.Println(err)
		os.Exit(1)
	}

	err = os.Setenv("KUBECONFIG", path.Join(
		config.KubeConfig[configprovider.FilePath],
		fmt.Sprintf("%s.%s", config.KubeConfig[configprovider.FileName], config.KubeConfig[configprovider.FileType])),
	)

	if err != nil {
		// Fail silently
		log.Warn(err)
	}

	// Initialize application specific configs and dependencies
	// App and request config
	cfg, err := config.New(configprovider.ViperKey)
	if err != nil {
		log.Error(err)
		os.Exit(1)
	}

	service := &grpc.Service{}
	err = cfg.GetObject(adapter.ServerKey, service)
	if err != nil {
		log.Error(err)
		os.Exit(1)
	}

	kubeconfigHandler, err := config.NewKubeconfigBuilder(configprovider.ViperKey)
	if err != nil {
		log.Error(err)
		os.Exit(1)
	}

	// // Initialize Tracing instance
	// tracer, err := tracing.New(service.Name, service.TraceURL)
	// if err != nil {
	//      log.Err("Tracing Init Failed", err.Error())
	//      os.Exit(1)
	// }
	e := events.NewEventStreamer()
	// Initialize Handler intance
	handler := traefik.New(cfg, log, kubeconfigHandler, e)
	handler = adapter.AddLogger(log, handler)

	service.Handler = handler
	service.EventStreamer = e
	service.StartedAt = time.Now()
	service.Version = version
	service.GitSHA = gitsha

	go registerCapabilities(service.Port, log)        //Registering static capabilities
	go registerDynamicCapabilities(service.Port, log) //Registering latest capabilities periodically

	// Server Initialization
	log.Info("Adaptor Listening at port: ", service.Port)
	err = grpc.Start(service, nil)
	if err != nil {
		log.Error(err)
		os.Exit(1)
	}
}

func isDebug() bool {
	return os.Getenv("DEBUG") == "true"
}

func mesheryServerAddress() string {
	meshReg := os.Getenv("MESHERY_SERVER")

	if meshReg != "" {
		if strings.HasPrefix(meshReg, "http") {
			return meshReg
		}

		return "http://" + meshReg
	}

	return "http://localhost:9081"
}

func serviceAddress() string {
	svcAddr := os.Getenv("SERVICE_ADDR")

	if svcAddr != "" {
		return svcAddr
	}

	return "mesherylocal.layer5.io"
}

func registerCapabilities(port string, log logger.Handler) {
	// Register workloads
	if err := oam.RegisterWorkloads(mesheryServerAddress(), serviceAddress()+":"+port); err != nil {
		log.Info(err.Error())
	}

	// Register traits
	if err := oam.RegisterTraits(mesheryServerAddress(), serviceAddress()+":"+port); err != nil {
		log.Info(err.Error())
	}
}
func registerDynamicCapabilities(port string, log logger.Handler) {
	registerWorkloads(port, log)
	//Start the ticker
	const reRegisterAfter = 24
	ticker := time.NewTicker(reRegisterAfter * time.Hour)
	for {
		<-ticker.C
		registerWorkloads(port, log)
	}

}

func registerWorkloads(port string, log logger.Handler) {

	version := build.DefaultVersion
	url := build.DefaultURL
	gm := build.DefaultGenerationMethod
	// Prechecking to skip comp gen
	if os.Getenv("FORCE_DYNAMIC_REG") != "true" && oam.AvailableVersions[version] {
		log.Info("Components available statically for version ", version, ". Skipping dynamic component registeration")
		return
	}
	//If a URL is passed from env variable, it will be used for component generation with default method being "using manifests"
	// In case a helm chart URL is passed, COMP_GEN_METHOD env variable should be set to Helm otherwise the component generation fails
	if os.Getenv("COMP_GEN_URL") != "" && (os.Getenv("COMP_GEN_METHOD") == "Helm" || os.Getenv("COMP_GEN_METHOD") == "Manifest") {
		url = os.Getenv("COMP_GEN_URL")
		gm = os.Getenv("COMP_GEN_METHOD")
		log.Info("Registering workload components from url ", url, " using ", gm, " method...")
	}

	log.Info("Registering latest workload components for version ", version)
	// Register workloads
	for _, crd := range build.CRDNames {
		crdurl := url + crd
		log.Info("Registering ", crdurl)
		if err := adapter.CreateComponents(adapter.StaticCompConfig{
			URL:     crdurl,
			Method:  gm,
			Path:    build.WorkloadPath,
			DirName: version,
			Config:  build.NewConfig(version),
		}); err != nil {
			log.Info(err.Error())
			return
		}
	}

	//The below log is checked in the workflows. If you change this log, reflect that change in the workflow where components are generated
	log.Info("Component creation completed for version ", version)

	//Now we will register in case
	log.Info("Registering workloads with Meshery Server for version ", version)
	originalPath := oam.WorkloadPath
	oam.WorkloadPath = filepath.Join(originalPath, version)
	defer resetWorkloadPath(originalPath)
	if err := oam.RegisterWorkloads(mesheryServerAddress(), serviceAddress()+":"+port); err != nil {
		log.Info(err.Error())
		return
	}
	log.Info("Latest workload components successfully registered.")
}

func resetWorkloadPath(orig string) {
	oam.WorkloadPath = orig
}
